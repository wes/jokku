package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wes/jokku/internal/types"
)

// The Backups view: everything that is (and isn't) backed up, at a glance,
// and the switches to change it. Restores stay on the command line: they
// replace data.

type backupsMsg struct {
	ov  *types.BackupsOverview
	err error
}

// backupDoneMsg ends an action started from the view.
type backupDoneMsg struct {
	row  string
	what string // said on success
	err  error
}

// bkRow is one line of the list: the cluster's own backups, or a volume.
type bkRow struct {
	key     string
	cluster bool
	vol     *types.VolumeProtection
}

// schedules are what s cycles through; 0 is off.
var schedules = []time.Duration{15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour, 0}

func (m *model) fetchBackups() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		ov, err := m.api.BackupsOverview(ctx)
		return backupsMsg{ov, err}
	}
}

func (m *model) bkRows() []bkRow {
	if m.bk == nil {
		return nil
	}
	rows := []bkRow{{key: "cluster", cluster: true}}
	for i := range m.bk.Volumes {
		v := &m.bk.Volumes[i]
		rows = append(rows, bkRow{key: v.App + "/" + v.Volume, vol: v})
	}
	return rows
}

func (m *model) selectedBk() (bkRow, bool) {
	rows := m.bkRows()
	c := m.cursor[backupsView]
	if c >= len(rows) {
		return bkRow{}, false
	}
	return rows[c], true
}

// backups is what the row's backups look like: nil when it isn't backed up.
func (r bkRow) backups(ov *types.BackupsOverview) *types.VolumeBackups {
	if r.cluster {
		return ov.Cluster
	}
	return r.vol.Backups
}

func (r bkRow) title() string {
	if r.cluster {
		return "the cluster itself"
	}
	return r.vol.App + " / " + r.vol.Volume
}

// health sums up how a row's backups are going: ok, waiting (no backup
// yet), overdue, failed, running, manual, or off (not backed up).
func health(b *types.VolumeBackups) string {
	switch {
	case b == nil:
		return "off"
	case b.Last != nil && b.Last.Status == types.StatusRunning:
		return "running"
	case b.Last != nil && b.Last.Status == types.StatusFailed:
		return "failed"
	case b.Succeeded == nil:
		return "waiting"
	case b.EverySeconds == 0:
		return "manual"
	case time.Since(b.Succeeded.FinishedAt) > 2*time.Duration(b.EverySeconds)*time.Second+5*time.Minute:
		return "overdue"
	}
	return "ok"
}

func healthDot(h string) string {
	switch h {
	case "ok", "manual":
		return sGood.Render("●")
	case "running":
		return sTitle.Render("◉")
	case "waiting", "overdue":
		return sWarn.Render("●")
	case "failed":
		return sBad.Render("●")
	}
	return sDim.Render("○")
}

// Keys

func (m *model) backupsKey(k string) (tea.Cmd, bool) {
	if m.bkBusy == nil {
		m.bkBusy = map[string]string{}
	}
	if m.bkPicking {
		return m.pickKey(k), true
	}
	r, ok := m.selectedBk()
	if !ok {
		return nil, false
	}
	confirm := m.bkConfirm
	m.bkConfirm = ""
	if _, busy := m.bkBusy[r.key]; busy && (k == "space" || k == " " || k == "b" || k == "s" || k == "a") {
		m.flash("still busy with "+r.title(), false)
		return nil, true
	}
	b := r.backups(m.bk)
	switch k {
	case " ", "space":
		if b == nil {
			return m.turnOn(r), true
		}
		if confirm != r.key {
			m.bkConfirm = r.key
			m.flash(fmt.Sprintf("press space again to stop backing up %s (its backups stay in the bucket)", r.title()), true)
			return nil, true
		}
		return m.act(r, "stopped backing up "+r.title(), func(ctx context.Context) error {
			if r.cluster {
				return m.api.UnsetClusterBackup(ctx)
			}
			return m.api.UnsetVolumeBackup(ctx, r.vol.App, r.vol.Volume)
		}), true
	case "b":
		if b == nil {
			m.flash(r.title()+" isn't backed up: press space to start", true)
			return nil, true
		}
		m.bkBusy[r.key] = "backing up"
		return m.act(r, "backed up "+r.title(), func(ctx context.Context) error {
			if r.cluster {
				return m.api.RunClusterBackup(ctx, func(types.Event) {})
			}
			return m.api.RunBackup(ctx, r.vol.App, r.vol.Volume, func(types.Event) {})
		}), true
	case "s":
		if b == nil {
			return nil, true
		}
		next := nextSchedule(b.EverySeconds)
		req := types.SetVolumeBackupRequest{Destination: b.Destination, Every: next}
		return m.set(r, req, "now backing up "+r.title()+" "+scheduleText(next)), true
	case "a":
		if b == nil || r.cluster {
			return nil, true
		}
		req := types.SetVolumeBackupRequest{Destination: b.Destination, AutoRestore: "on"}
		what := "auto-restore on for " + r.title()
		if b.AutoRestore {
			req.AutoRestore, what = "off", "auto-restore off for "+r.title()+": if its node dies, it waits for it"
		}
		return m.set(r, req, what), true
	}
	return nil, false
}

// turnOn starts backing a row up, asking where when there is a choice.
func (m *model) turnOn(r bkRow) tea.Cmd {
	switch len(m.bk.Destinations) {
	case 0:
		m.flash("add a destination first: jokku backups:destination-add <name> --endpoint ... --bucket ...", true)
		return nil
	case 1:
		return m.set(r, types.SetVolumeBackupRequest{Destination: m.bk.Destinations[0].Name}, "now backing up "+r.title()+" to "+m.bk.Destinations[0].Name)
	}
	m.bkPicking, m.bkPickFor, m.bkPick = true, r.key, 0
	return nil
}

func (m *model) pickKey(k string) tea.Cmd {
	n := len(m.bk.Destinations)
	switch k {
	case "esc", "q":
		m.bkPicking = false
	case "up", "left", "k", "shift+tab":
		m.bkPick = (m.bkPick + n - 1) % n
	case "down", "right", "j", "tab":
		m.bkPick = (m.bkPick + 1) % n
	case "enter", " ", "space":
		m.bkPicking = false
		for _, r := range m.bkRows() {
			if r.key == m.bkPickFor {
				d := m.bk.Destinations[m.bkPick].Name
				return m.set(r, types.SetVolumeBackupRequest{Destination: d}, "now backing up "+r.title()+" to "+d)
			}
		}
	}
	return nil
}

func (m *model) set(r bkRow, req types.SetVolumeBackupRequest, what string) tea.Cmd {
	m.bkBusy[r.key] = "saving"
	return m.act(r, what, func(ctx context.Context) error {
		if r.cluster {
			_, err := m.api.SetClusterBackup(ctx, req)
			return err
		}
		_, err := m.api.SetVolumeBackup(ctx, r.vol.App, r.vol.Volume, req)
		return err
	})
}

func (m *model) act(r bkRow, what string, fn func(context.Context) error) tea.Cmd {
	if m.bkBusy[r.key] == "" {
		m.bkBusy[r.key] = "saving"
	}
	return func() tea.Msg {
		return backupDoneMsg{row: r.key, what: what, err: fn(m.ctx)}
	}
}

func (m *model) flash(msg string, warning bool) {
	m.bkFlash, m.bkFlashWarn, m.bkFlashAt = msg, warning, time.Now()
}

// nextSchedule is the schedule after every (seconds), as backups:set --every
// takes it.
func nextSchedule(every int64) string {
	next := schedules[0]
	for i, s := range schedules {
		if s == time.Duration(every)*time.Second {
			next = schedules[(i+1)%len(schedules)]
		}
	}
	if next == 0 {
		return "off"
	}
	return shortDuration(next)
}

func scheduleText(s string) string {
	if s == "off" {
		return "only when asked"
	}
	return "every " + s
}

// View

func (m *model) backupsView() string {
	if m.bk == nil {
		if m.bkErr != nil {
			return sBad.Render("  Cannot load backups: " + m.bkErr.Error())
		}
		return sDim.Render("  Loading backups...")
	}
	var b strings.Builder
	b.WriteString(m.protection())
	b.WriteString("\n" + m.bkTable())
	if detail := m.bkDetail(); detail != "" {
		b.WriteString("\n" + detail)
	}
	return b.String()
}

// protection is the summary at the top: how much is backed up, and whether
// anything needs attention.
func (m *model) protection() string {
	ov := m.bk
	counts := map[string]int{}
	for _, v := range ov.Volumes {
		counts[health(v.Backups)]++
	}
	total := len(ov.Volumes)
	protected := total - counts["off"]
	var b strings.Builder

	// The headline.
	var issues []string
	switch {
	case len(ov.Destinations) == 0:
		issues = append(issues, sBad.Render("Nothing is backed up yet: add a destination with jokku backups:destination-add"))
	default:
		if ov.Key != nil && !ov.Key.Saved {
			issues = append(issues, sBad.Render("The backup key isn't saved: run jokku backups:key"))
		}
		if ov.Cluster == nil {
			issues = append(issues, sBad.Render("The cluster itself isn't backed up"))
		} else if h := health(ov.Cluster); h == "failed" || h == "overdue" {
			issues = append(issues, sBad.Render("The cluster's backups are "+h))
		}
		if n := counts["failed"]; n > 0 {
			issues = append(issues, sBad.Render(plural(n, "volume")+" failed to back up"))
		}
		if n := counts["overdue"]; n > 0 {
			issues = append(issues, sWarn.Render(plural(n, "volume")+" overdue"))
		}
		if n := counts["off"]; n > 0 {
			issues = append(issues, sWarn.Render(plural(n, "volume")+" not backed up"))
		}
	}
	if len(issues) == 0 {
		b.WriteString("  " + sGood.Render("✔ Everything is backed up") + sDim.Render(" · "+m.lastAndNext()) + "\n")
	} else {
		b.WriteString("  " + sWarn.Render("⚠ ") + strings.Join(issues, sDim.Render(" · ")) + "\n")
	}

	barWidth := max(min(m.width-40, 40), 10)
	label := fmt.Sprintf("%d of %d volumes", protected, total)
	if total == 0 {
		label = "no volumes yet"
	}
	fmt.Fprintf(&b, "  %s %s  %s\n", sHead.Render(pad("Volumes", 12)), protectionBar(counts, total, barWidth), label)

	cluster := sDim.Render("○ not backed up (select it below and press space)")
	if c := ov.Cluster; c != nil {
		cluster = healthDot(health(c)) + " " + summaryLine(c)
	}
	fmt.Fprintf(&b, "  %s %s\n", sHead.Render(pad("Cluster", 12)), cluster)
	key := sDim.Render("○ none yet (made with the first encrypted destination)")
	if k := ov.Key; k != nil {
		if k.Saved {
			key = sGood.Render("●") + " saved" + sDim.Render(" · ID "+k.ID)
		} else {
			key = sBad.Render("● not saved") + sDim.Render(" · run jokku backups:key, and keep it somewhere safe")
		}
	}
	fmt.Fprintf(&b, "  %s %s\n", sHead.Render(pad("Key", 12)), key)
	var dests []string
	for _, d := range ov.Destinations {
		how := "encrypted"
		if !d.Encrypt {
			how = sWarn.Render("not encrypted")
		}
		dests = append(dests, d.Name+sDim.Render(" ("+d.Bucket+", ")+how+sDim.Render(")"))
	}
	if len(dests) == 0 {
		dests = []string{sDim.Render("none")}
	}
	fmt.Fprintf(&b, "  %s %s\n", sHead.Render(pad("Destinations", 12)), strings.Join(dests, sDim.Render(" · ")))
	return b.String()
}

// lastAndNext says when the newest backup was, and the next is due.
func (m *model) lastAndNext() string {
	var last, next time.Time
	all := []*types.VolumeBackups{m.bk.Cluster}
	for _, v := range m.bk.Volumes {
		all = append(all, v.Backups)
	}
	for _, b := range all {
		if b == nil {
			continue
		}
		if b.Succeeded != nil && b.Succeeded.FinishedAt.After(last) {
			last = b.Succeeded.FinishedAt
		}
		if !b.Next.IsZero() && (next.IsZero() || b.Next.Before(next)) {
			next = b.Next
		}
	}
	out := "last backup " + ago(last)
	if !next.IsZero() {
		out += ", next " + until(next)
	}
	return out
}

func protectionBar(counts map[string]int, total, width int) string {
	if total == 0 {
		return sDim.Render(strings.Repeat("░", width))
	}
	cells := func(n int) int { return int(float64(n)/float64(total)*float64(width) + 0.5) }
	good := cells(counts["ok"] + counts["manual"] + counts["running"])
	warnN := cells(counts["waiting"] + counts["overdue"])
	badN := cells(counts["failed"])
	good = min(good, width)
	warnN = min(warnN, width-good)
	badN = min(badN, width-good-warnN)
	return sGood.Render(strings.Repeat("█", good)) + sWarn.Render(strings.Repeat("█", warnN)) +
		sBad.Render(strings.Repeat("█", badN)) + sDim.Render(strings.Repeat("░", width-good-warnN-badN))
}

func summaryLine(b *types.VolumeBackups) string {
	parts := []string{scheduleText(everyText(b))}
	switch h := health(b); h {
	case "running":
		parts = append(parts, sTitle.Render("backing up now"))
	case "failed":
		parts = append(parts, sBad.Render("failed "+ago(b.Last.StartedAt)))
	default:
		if b.Succeeded != nil {
			parts = append(parts, "last "+ago(b.Succeeded.FinishedAt))
		} else {
			parts = append(parts, sWarn.Render("no backup yet"))
		}
	}
	if !b.Next.IsZero() {
		parts = append(parts, "next "+until(b.Next))
	}
	parts = append(parts, fmt.Sprintf("%d kept, %s", b.StoredBackups, bytesText(b.StoredBytes)))
	return strings.Join(parts, sDim.Render(" · "))
}

func everyText(b *types.VolumeBackups) string {
	if b.EverySeconds == 0 {
		return "off"
	}
	return shortDuration(time.Duration(b.EverySeconds) * time.Second)
}

func (m *model) bkTable() string {
	rows := m.bkRows()
	wide, mid := m.width >= 118, m.width >= 78
	var b strings.Builder
	switch {
	case wide:
		b.WriteString(sHead.Render(fmt.Sprintf("    %-28s %-12s %-14s %-17s %-9s %-7s %-6s %-9s %s",
			"WHAT", "NODE", "USED", "LAST BACKUP", "NEXT", "EVERY", "KEPT", "STORED", "AUTO-RESTORE")) + "\n")
	case mid:
		b.WriteString(sHead.Render(fmt.Sprintf("    %-26s %-17s %-9s %-7s %s", "WHAT", "LAST BACKUP", "NEXT", "EVERY", "STORED")) + "\n")
	default:
		b.WriteString(sHead.Render("    WHAT") + "\n")
	}
	height := max(m.height-20, 3)
	start, end := window(m.cursor[backupsView], len(rows), height)
	for i := start; i < end; i++ {
		r := rows[i]
		bk := r.backups(m.bk)
		h := health(bk)
		name := r.title()
		if r.cluster {
			name = "cluster (control node)"
		}
		last, next, every, kept, stored, auto := sDim.Render("not backed up"), "", "", "", "", ""
		if bk != nil {
			every, kept, stored = everyText(bk), fmt.Sprint(bk.StoredBackups), bytesText(bk.StoredBytes)
			if !bk.Next.IsZero() {
				next = until(bk.Next)
			}
			switch h {
			case "running":
				last = sTitle.Render("backing up now")
			case "failed":
				last = sBad.Render("failed " + ago(bk.Last.StartedAt))
			case "waiting":
				last = sWarn.Render("not yet")
			case "overdue":
				last = sWarn.Render("overdue " + ago(bk.Succeeded.FinishedAt))
			default:
				last = sGood.Render("✓ ") + ago(bk.Succeeded.FinishedAt)
			}
			if !r.cluster {
				auto = sDim.Render("off")
				if bk.AutoRestore {
					auto = "on"
				}
			}
		} else if !r.cluster {
			last = sDim.Render("not backed up") + sDim.Render(" (space)")
		}
		if busy, ok := m.bkBusy[r.key]; ok {
			last = sTitle.Render(busy + "…")
		}
		node, used := "", ""
		if !r.cluster {
			node = r.vol.Node
			used = mem(r.vol.UsedMB) + " / " + mem(r.vol.SizeMB)
			if r.vol.Status == "restoring" {
				last = sTitle.Render("restoring…")
			}
		}
		var line string
		switch {
		case wide:
			line = fmt.Sprintf("  %s %-28s %-12s %-14s %s %-9s %-7s %-6s %-9s %s", healthDot(h), trunc(name, 28), trunc(node, 12), used,
				pad(last, 17), next, every, kept, stored, auto)
		case mid:
			line = fmt.Sprintf("  %s %-26s %s %-9s %-7s %s", healthDot(h), trunc(name, 26), pad(last, 17), next, every, stored)
		default:
			line = fmt.Sprintf("  %s %s", healthDot(h), name)
		}
		b.WriteString(m.selectable(backupsView, i, line) + "\n")
	}
	if len(m.bk.Volumes) == 0 {
		b.WriteString(sDim.Render("    no volumes yet: jokku storage:mount <app> <name>:<path>") + "\n")
	}
	return b.String()
}

// bkDetail describes the selected row, with a chart of its recent backups.
func (m *model) bkDetail() string {
	if m.height < 22 {
		return m.flashLine()
	}
	r, ok := m.selectedBk()
	if !ok {
		return m.flashLine()
	}
	var b strings.Builder
	if m.bkPicking {
		b.WriteString("  " + sTitle.Render("Back up "+r.title()+" to:") + " ")
		for i, d := range m.bk.Destinations {
			if i == m.bkPick {
				b.WriteString(sTabOn.Render(d.Name))
			} else {
				b.WriteString(sTab.Render(d.Name))
			}
		}
		b.WriteString(sDim.Render("   ←→ choose · enter confirm · esc cancel") + "\n")
		return b.String()
	}
	bk := r.backups(m.bk)
	title := sTitle.Render(r.title())
	if bk == nil {
		b.WriteString("  " + title + sDim.Render(" isn't backed up. ") + "Press space to back it up")
		if len(m.bk.Destinations) == 1 {
			b.WriteString(" to " + m.bk.Destinations[0].Name)
		}
		b.WriteString(sDim.Render(", every 15 minutes, keeping 24 hours of backups and 30 dailies.") + "\n")
		return b.String() + m.flashLine()
	}
	b.WriteString("  " + title + sDim.Render(" → "+bk.Destination+" "+bk.Path) + "\n")
	parts := []string{scheduleText(everyText(bk))}
	if !r.cluster {
		if bk.AutoRestore {
			parts = append(parts, "if its node dies, restored elsewhere after "+shortDuration(time.Duration(bk.FailoverSeconds)*time.Second))
		} else {
			parts = append(parts, "if its node dies, it waits for it")
		}
	}
	parts = append(parts, fmt.Sprintf("keeping every backup from the last %s and the last of each day for %d days",
		shortDuration(time.Duration(bk.KeepRecentSeconds)*time.Second), bk.KeepDaily))
	b.WriteString(wrapParts(parts, m.width-4, "  "))
	recent := m.bk.ClusterRecent
	if !r.cluster {
		recent = r.vol.Recent
	}
	b.WriteString("  " + sparkline(recent, recentWidth) + " " + sDim.Render("recent backups, by what each added") + "\n")
	b.WriteString(sDim.Render(fmt.Sprintf("  %s stored in %d backups", bytesText(bk.StoredBytes), bk.StoredBackups)) + "\n")
	if bk.Last != nil && bk.Last.Status == types.StatusFailed {
		b.WriteString("  " + sBad.Render("The last backup failed: "+bk.Last.Error) + "\n")
	}
	if !r.cluster && r.vol.OldCopy != nil {
		b.WriteString("  " + sWarn.Render("An old copy is kept on "+r.vol.OldCopy.Node+": "+r.vol.OldCopy.Disk+
			" (jokku storage:discard-old-copy "+r.vol.App+" "+r.vol.Volume+")") + "\n")
	}
	if !r.cluster {
		b.WriteString(sDim.Render("  restore with: jokku backups:restore "+r.vol.App+" "+r.vol.Volume+" [<backup>]") + "\n")
	} else {
		b.WriteString(sDim.Render("  rebuild a lost control node with: sudo jokku restore-cluster") + "\n")
	}
	return b.String() + m.flashLine()
}

// wrapParts joins parts with dots, starting a new line (after indent) when
// the next part would pass width.
func wrapParts(parts []string, width int, indent string) string {
	var b strings.Builder
	line := ""
	for _, p := range parts {
		switch {
		case line == "":
			line = p
		case len(line)+3+len(p) > width:
			b.WriteString(indent + sDim.Render(line) + "\n")
			line = p
		default:
			line += " · " + p
		}
	}
	if line != "" {
		b.WriteString(indent + sDim.Render(line) + "\n")
	}
	return b.String()
}

func (m *model) flashLine() string {
	if m.bkFlash == "" || time.Since(m.bkFlashAt) > 8*time.Second {
		return ""
	}
	if m.bkFlashWarn {
		return "  " + sWarn.Render(m.bkFlash) + "\n"
	}
	return "  " + sGood.Render(m.bkFlash) + "\n"
}

const recentWidth = 24

// sparkline charts recent backups by how much each added, failures in red.
func sparkline(points []types.BackupPoint, width int) string {
	levels := []rune("▁▂▃▄▅▆▇█")
	var top int64
	for _, p := range points {
		top = max(top, p.NewBytes)
	}
	if len(points) > width {
		points = points[len(points)-width:]
	}
	var b strings.Builder
	b.WriteString(sDim.Render(strings.Repeat("·", width-len(points))))
	for _, p := range points {
		switch p.Status {
		case types.StatusFailed:
			b.WriteString(sBad.Render("✕"))
		case types.StatusRunning:
			b.WriteString(sTitle.Render("•"))
		default:
			i := 0
			if top > 0 {
				i = int(float64(p.NewBytes) / float64(top) * float64(len(levels)-1))
			}
			b.WriteString(sGood.Render(string(levels[i])))
		}
	}
	return b.String()
}

func bytesText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func shortDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}

func until(t time.Time) string {
	d := time.Until(t)
	if d <= 30*time.Second {
		return "now"
	}
	return "in " + since(time.Now().Add(-d))
}

// backupsLine is the Overview's one-line summary of backups.
func (m *model) backupsLine() string {
	if m.bk == nil {
		return ""
	}
	counts := map[string]int{}
	for _, v := range m.bk.Volumes {
		counts[health(v.Backups)]++
	}
	total := len(m.bk.Volumes)
	parts := []string{fmt.Sprintf("%d of %d volumes backed up", total-counts["off"], total)}
	dot := sGood.Render("●")
	if counts["failed"] > 0 || m.bk.Cluster == nil || len(m.bk.Destinations) == 0 {
		dot = sBad.Render("●")
	} else if counts["off"]+counts["overdue"]+counts["waiting"] > 0 {
		dot = sWarn.Render("●")
	}
	if m.bk.Cluster == nil {
		parts = append(parts, "cluster not backed up")
	} else if m.bk.Cluster.Succeeded != nil {
		parts = append(parts, "cluster "+ago(m.bk.Cluster.Succeeded.FinishedAt))
	}
	if counts["failed"] > 0 {
		parts = append(parts, plural(counts["failed"], "failure"))
	}
	return "  " + dot + " " + strings.Join(parts, sDim.Render(" · ")) + sDim.Render("   (7 for details)") + "\n"
}
