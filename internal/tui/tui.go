// Package tui is "jokku top": a live, interactive view of the cluster's
// nodes, apps, instances and events, refreshed every two seconds from the
// API.
package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"github.com/wes/jokku/internal/client"
	"github.com/wes/jokku/internal/types"
)

const refreshEvery = 2 * time.Second

type view int

const (
	overview view = iota
	nodesView
	appsView
	instancesView
	eventsView
	trafficView
	backupsView
	logsView
)

var tabNames = []string{"Overview", "Nodes", "Apps", "Instances", "Events", "Traffic", "Backups"}

// Run starts the dashboard and blocks until the user quits.
func Run(ctx context.Context, api *client.Client, in io.Reader, out io.Writer) error {
	if f, ok := out.(*os.File); !ok || !term.IsTerminal(int(f.Fd())) {
		return fmt.Errorf("jokku top needs a terminal; over ssh, use ssh -t")
	}
	m := &model{ctx: ctx, api: api, cursor: map[view]int{}, bkBusy: map[string]string{}}
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithInput(in), tea.WithOutput(out), tea.WithContext(ctx)).Run()
	if err == tea.ErrProgramKilled {
		return nil
	}
	return err
}

type model struct {
	ctx    context.Context
	api    *client.Client
	st     *types.ClusterStatus
	err    error
	at     time.Time
	view   view
	back   view
	cursor map[view]int
	width  int
	height int
	help   bool

	filterApp, filterNode string // instance list filters (Enter on an app or node)
	logApp                string
	logs                  []string
	logErr                error

	tr *traffic

	// The Backups view (backups.go).
	bk          *types.BackupsOverview
	bkErr       error
	bkBusy      map[string]string // row -> what it is doing
	bkFlash     string
	bkFlashWarn bool
	bkFlashAt   time.Time
	bkConfirm   string // row whose backups the next space turns off
	bkPicking   bool   // choosing a destination for bkPickFor
	bkPickFor   string
	bkPick      int
}

type statusMsg struct {
	st  *types.ClusterStatus
	err error
}

type tickMsg struct{}

type logsMsg struct {
	app   string
	lines []string
	err   error
}

func (m *model) Init() tea.Cmd { return tea.Batch(m.fetch(), m.fetchBackups(), tick()) }

func tick() tea.Cmd {
	return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *model) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		st, err := m.api.ClusterStatus(ctx)
		return statusMsg{st, err}
	}
}

func (m *model) fetchLogs(app string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		var lines []string
		err := m.api.Logs(ctx, app, types.LogOptions{Tail: 300}, func(e types.Event) { lines = append(lines, e.Message) })
		return logsMsg{app, lines, err}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case statusMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.st, m.err, m.at = msg.st, nil, time.Now()
		}
	case logsMsg:
		if msg.app == m.logApp {
			m.logs, m.logErr = msg.lines, msg.err
		}
	case backupsMsg:
		if msg.err != nil {
			m.bkErr = msg.err
		} else {
			m.bk, m.bkErr = msg.ov, nil
			m.move(0)
		}
	case backupDoneMsg:
		delete(m.bkBusy, msg.row)
		if msg.err != nil {
			m.flash(msg.err.Error(), true)
		} else {
			m.flash("✔ "+msg.what, false)
		}
		return m, m.fetchBackups()
	case tickMsg:
		cmds := []tea.Cmd{m.fetch(), tick()}
		switch m.view {
		case logsView:
			cmds = append(cmds, m.fetchLogs(m.logApp))
		case overview, backupsView:
			cmds = append(cmds, m.fetchBackups())
		}
		return m, tea.Batch(cmds...)
	case tea.KeyMsg:
		before := m.view
		cmds := []tea.Cmd{m.key(msg.String())}
		switch {
		case before != trafficView && m.view == trafficView:
			cmds = append(cmds, m.startTraffic())
		case before == trafficView && m.view != trafficView:
			m.stopTraffic()
		}
		if before != backupsView && m.view == backupsView {
			cmds = append(cmds, m.fetchBackups())
		}
		return m, tea.Batch(cmds...)
	case requestsMsg, trafficEndMsg, frameMsg, trafficRetryMsg:
		if m.tr == nil {
			return m, nil
		}
		return m, m.updateTraffic(msg)
	}
	return m, nil
}

func (m *model) key(k string) tea.Cmd {
	if m.help {
		m.help = false
		return nil
	}
	if m.trafficKey(k) {
		return nil
	}
	if m.view == backupsView {
		if cmd, ok := m.backupsKey(k); ok {
			return cmd
		}
	}
	switch k {
	case "q", "ctrl+c":
		return tea.Quit
	case "?":
		m.help = true
	case "r":
		return m.fetch()
	case "tab", "right":
		if m.view < backupsView {
			m.view++
		} else if m.view == backupsView {
			m.view = overview
		}
	case "shift+tab", "left":
		if m.view > overview && m.view <= backupsView {
			m.view--
		} else if m.view == overview {
			m.view = backupsView
		}
	case "1", "2", "3", "4", "5", "6", "7":
		m.view = view(k[0] - '1')
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "pgup":
		m.move(-10)
	case "pgdown":
		m.move(10)
	case "home", "g":
		m.cursor[m.view] = 0
	case "end", "G":
		m.cursor[m.view] = 1 << 30
		m.move(0)
	case "enter":
		switch m.view {
		case nodesView:
			if n, ok := m.selectedNode(); ok {
				m.filterApp, m.filterNode, m.view = "", n.Name, instancesView
				m.cursor[instancesView] = 0
			}
		case appsView, overview:
			if a, ok := m.selectedApp(); ok {
				m.filterApp, m.filterNode, m.view = a.Name, "", instancesView
				m.cursor[instancesView] = 0
			}
		}
	case "esc", "backspace":
		switch {
		case m.view == logsView:
			m.view = m.back
		case m.filterApp != "" || m.filterNode != "":
			m.filterApp, m.filterNode = "", ""
		}
	case "l":
		app := ""
		switch m.view {
		case appsView, overview:
			if a, ok := m.selectedApp(); ok {
				app = a.Name
			}
		case instancesView:
			if in, ok := m.selectedInstance(); ok {
				app = in.App
			}
		}
		if app != "" {
			m.back, m.view, m.logApp, m.logs, m.logErr = m.view, logsView, app, nil, nil
			return m.fetchLogs(app)
		}
	}
	return nil
}

func (m *model) rows() int {
	if m.st == nil {
		return 0
	}
	switch m.view {
	case nodesView:
		return len(m.st.Nodes)
	case appsView, overview:
		return len(m.st.Apps)
	case instancesView:
		return len(m.instances())
	case eventsView:
		return len(m.st.Events)
	case logsView:
		return len(m.logs)
	case backupsView:
		return len(m.bkRows())
	}
	return 0
}

func (m *model) move(d int) {
	n := m.rows()
	c := m.cursor[m.view] + d
	if c >= n {
		c = n - 1
	}
	if c < 0 {
		c = 0
	}
	m.cursor[m.view] = c
}

func (m *model) selectedNode() (types.NodeView, bool) {
	c := m.cursor[nodesView]
	if m.st == nil || c >= len(m.st.Nodes) {
		return types.NodeView{}, false
	}
	return m.st.Nodes[c], true
}

func (m *model) selectedApp() (types.AppView, bool) {
	c := m.cursor[m.view]
	if m.st == nil || c >= len(m.st.Apps) {
		return types.AppView{}, false
	}
	return m.st.Apps[c], true
}

func (m *model) selectedInstance() (types.InstanceView, bool) {
	list := m.instances()
	c := m.cursor[instancesView]
	if c >= len(list) {
		return types.InstanceView{}, false
	}
	return list[c], true
}

// instances is the instance list, filtered and sorted by app and process.
func (m *model) instances() []types.InstanceView {
	if m.st == nil {
		return nil
	}
	var out []types.InstanceView
	for _, in := range m.st.Instances {
		if (m.filterApp == "" || in.App == m.filterApp) && (m.filterNode == "" || in.Node == m.filterNode) {
			out = append(out, in)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Styles

var (
	accent    = lipgloss.AdaptiveColor{Light: "#5A3FD6", Dark: "#A78BFA"}
	dim       = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"}
	good      = lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#4ADE80"}
	warn      = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FBBF24"}
	bad       = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#F87171"}
	sTitle    = lipgloss.NewStyle().Bold(true).Foreground(accent)
	sDim      = lipgloss.NewStyle().Foreground(dim)
	sGood     = lipgloss.NewStyle().Foreground(good)
	sWarn     = lipgloss.NewStyle().Foreground(warn)
	sBad      = lipgloss.NewStyle().Foreground(bad)
	sHead     = lipgloss.NewStyle().Bold(true).Foreground(dim)
	sTab      = lipgloss.NewStyle().Padding(0, 1).Foreground(dim)
	sTabOn    = lipgloss.NewStyle().Padding(0, 1).Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#111827"}).Background(accent)
	sSelected = lipgloss.NewStyle().Background(lipgloss.AdaptiveColor{Light: "#EDE9FE", Dark: "#312E81"})
	sSection  = lipgloss.NewStyle().Bold(true).Foreground(accent).MarginTop(1)
)
