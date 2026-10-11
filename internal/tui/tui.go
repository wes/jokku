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
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"github.com/wes/jokku/internal/client"
	"github.com/wes/jokku/internal/database"
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
	// Ask the terminal for its background (the colors follow it) before the
	// program starts reading keys and could take the answer for one.
	lipgloss.HasDarkBackground()
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
	// The Traffic view's last hour and day (latency.go).
	lat    *types.Latency
	latAt  time.Time // when it was last asked for
	latErr error

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
	case latencyMsg:
		m.lat, m.latErr = msg.l, msg.err
	case tickMsg:
		cmds := []tea.Cmd{m.fetch(), tick()}
		switch m.view {
		case logsView:
			cmds = append(cmds, m.fetchLogs(m.logApp))
		case overview, backupsView:
			cmds = append(cmds, m.fetchBackups())
		case trafficView:
			if time.Since(m.latAt) >= latencyEvery {
				cmds = append(cmds, m.fetchLatency())
			}
		}
		return m, tea.Batch(cmds...)
	case tea.KeyMsg:
		before := m.view
		cmds := []tea.Cmd{m.key(msg.String())}
		switch {
		case before != trafficView && m.view == trafficView:
			cmds = append(cmds, m.startTraffic(), m.fetchLatency())
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
		return len(m.appList())
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
	list := m.appList()
	c := m.cursor[m.view]
	if c >= len(list) {
		return types.AppView{}, false
	}
	return list[c], true
}

// appList is the apps the way the overview and the Apps view list them:
// apps, then databases.
func (m *model) appList() []types.AppView {
	if m.st == nil {
		return nil
	}
	var apps, dbs []types.AppView
	for _, a := range m.st.Apps {
		if isDatabase(a) {
			dbs = append(dbs, a)
		} else {
			apps = append(apps, a)
		}
	}
	return append(apps, dbs...)
}

// isDatabase says whether an app is a database (jokku db:), rather than an
// app of the user's own or an external one.
func isDatabase(a types.AppView) bool {
	_, ok := database.Lookup(a.Kind)
	return ok
}

// dbName is a database's own name: shopdb for the app postgres-shopdb.
func dbName(a types.AppView) string { return strings.TrimPrefix(a.Name, a.Kind+"-") }

// dbUse is a database an app is linked to, and the config var that holds
// its URL.
type dbUse struct {
	db      types.AppView
	varName string
}

// databasesOf maps each app to the databases it's linked to.
func databasesOf(list []types.AppView) map[string][]dbUse {
	out := map[string][]dbUse{}
	for _, a := range list {
		for _, l := range a.Links {
			out[l.App] = append(out[l.App], dbUse{a, l.Var})
		}
	}
	return out
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

// selected is the selected row's background: a shade lighter than a dark
// terminal, or darker than a light one. Each color mode gets its own, since
// the nearest of 256 colors to a warm brown is a dark red, which reads as
// an error.
var selected = lipgloss.CompleteAdaptiveColor{
	Light: lipgloss.CompleteColor{TrueColor: "#ECE5DB", ANSI256: "254", ANSI: "7"},
	Dark:  lipgloss.CompleteColor{TrueColor: "#2F2A24", ANSI256: "236", ANSI: "8"},
}

// tabOn is the open tab's background, the accent; with 16 colors, yellow,
// which is closer to amber than the red the accent would become.
var tabOn = lipgloss.CompleteAdaptiveColor{
	Light: lipgloss.CompleteColor{TrueColor: "#A9580F", ANSI256: "130", ANSI: "3"},
	Dark:  lipgloss.CompleteColor{TrueColor: "#E8963F", ANSI256: "173", ANSI: "3"},
}

// Styles: the website's terminal colors, warm amber on dark (or a darker
// amber on a light terminal).

var (
	accent    = lipgloss.AdaptiveColor{Light: "#A9580F", Dark: "#E8963F"}
	fg        = lipgloss.AdaptiveColor{Light: "#1F1812", Dark: "#E6DCCD"}
	dim       = lipgloss.AdaptiveColor{Light: "#6B5C4C", Dark: "#8A7C6A"}
	good      = lipgloss.AdaptiveColor{Light: "#2F7D32", Dark: "#8BD17C"}
	warn      = lipgloss.AdaptiveColor{Light: "#9A6A00", Dark: "#F2C94C"}
	bad       = lipgloss.AdaptiveColor{Light: "#B42318", Dark: "#F07A6A"}
	cyan      = lipgloss.AdaptiveColor{Light: "#1F7A70", Dark: "#8FD0C4"}
	sTitle    = lipgloss.NewStyle().Bold(true).Foreground(accent)
	sAccent   = lipgloss.NewStyle().Foreground(accent)
	sDim      = lipgloss.NewStyle().Foreground(dim)
	sGood     = lipgloss.NewStyle().Foreground(good)
	sWarn     = lipgloss.NewStyle().Foreground(warn)
	sBad      = lipgloss.NewStyle().Foreground(bad)
	sCyan     = lipgloss.NewStyle().Foreground(cyan)
	sHead     = lipgloss.NewStyle().Bold(true).Foreground(dim)
	sTab      = lipgloss.NewStyle().Padding(0, 1).Foreground(dim)
	sTabOn    = lipgloss.NewStyle().Padding(0, 1).Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#FFFAF3", Dark: "#1C130A"}).Background(tabOn)
	sSelected = lipgloss.NewStyle().Background(selected)
	sSection  = lipgloss.NewStyle().Bold(true).Foreground(accent).MarginTop(1)
)
