package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/wes/jokku/internal/types"
)

// withDatabases is the sample cluster with two databases, one used by both
// apps, one by none, and an external app. The store lists them by name, so
// they come mixed in among the apps.
func withDatabases() *types.ClusterStatus {
	st := sampleStatus()
	now := time.Now()
	st.Apps = []types.AppView{
		st.Apps[1], // blog
		{Name: "home", Kind: types.KindExternal, Domains: []string{"home.example.com"}, Target: "http://192.168.1.50:8123", Via: "control"},
		{Name: "mysql-scratch", Kind: "mysql", Release: 1, Healthy: 1, Wanted: 1,
			Deploy: &types.Deploy{Status: types.StatusSucceeded, CreatedAt: now.Add(-48 * time.Hour)}},
		{Name: "postgres-shopdb", Kind: "postgres", Release: 2, Healthy: 1, Wanted: 1,
			Deploy: &types.Deploy{Status: types.StatusSucceeded, CreatedAt: now.Add(-2 * time.Hour)},
			Links:  []types.DatabaseLink{{App: "blog", Var: "DATABASE_URL"}, {App: "shop", Var: "DATABASE_URL"}}},
		{Name: "redis-cache", Kind: "redis", Release: 1, Asleep: true, Wanted: 1,
			Links: []types.DatabaseLink{{App: "shop", Var: "REDIS_URL"}}},
		st.Apps[0], // shop
	}
	st.Instances = append(st.Instances,
		types.InstanceView{App: "postgres-shopdb", Instance: types.Instance{Name: "postgres.1", Release: 2, Node: "control", State: "healthy", Desired: "running", CPUs: 1, MemoryMB: 512}},
		types.InstanceView{App: "mysql-scratch", Instance: types.Instance{Name: "mysql.1", Release: 1, Node: "worker-1", State: "healthy", Desired: "running", CPUs: 1, MemoryMB: 1024}},
		types.InstanceView{App: "redis-cache", Instance: types.Instance{Name: "redis.1", Release: 1, Node: "control", State: "stopped", Desired: "running", CPUs: 1, MemoryMB: 256}})
	return st
}

func appsModel(w, h int) *model {
	m := &model{ctx: context.Background(), cursor: map[view]int{}, st: withDatabases(), at: time.Now()}
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

// section is the lines of a view from the one starting with from up to the
// next blank line or section heading.
func section(out, from string) []string {
	var lines []string
	in := false
	for _, l := range strings.Split(ansi.Strip(out), "\n") {
		switch {
		case strings.HasPrefix(l, from):
			in = true
			continue
		case in && (strings.TrimSpace(l) == "" || !strings.HasPrefix(l, " ")):
			return lines
		case in:
			lines = append(lines, l)
		}
	}
	return lines
}

func names(lines []string) []string {
	var out []string
	for _, l := range lines {
		if f := strings.Fields(l); len(f) > 1 {
			out = append(out, f[0]+" "+f[1])
		}
	}
	return out
}

// TestOverviewListsDatabasesApart: the overview lists the apps, then the
// external ones and the databases under headings of their own, each
// database with the apps that use it.
func TestOverviewListsDatabasesApart(t *testing.T) {
	m := appsModel(120, 40)
	out := m.View()
	if got := fmt.Sprint(names(section(out, "Apps"))); got != "[● blog ● shop]" {
		t.Errorf("Apps: %s\n%s", got, ansi.Strip(out))
	}
	if ext := section(out, "External"); len(ext) != 1 || !strings.Contains(ext[0], "→ home") ||
		!strings.Contains(ext[0], "http://192.168.1.50:8123") || !strings.Contains(ext[0], "via control") {
		t.Errorf("External: %q\n%s", ext, ansi.Strip(out))
	}
	dbs := section(out, "Databases")
	if got := fmt.Sprint(names(dbs)); got != "[● scratch ● shopdb ● cache]" {
		t.Errorf("Databases: %s\n%s", got, ansi.Strip(out))
	}
	for i, want := range []string{"mysql     not linked", "postgres  used by blog, shop", "redis     used by shop"} {
		if i < len(dbs) && !strings.Contains(dbs[i], want) {
			t.Errorf("database line %q, want %q", dbs[i], want)
		}
	}
	if h := ansi.Strip(m.header()); !strings.Contains(h, "2 apps  ·  1 external  ·  3 databases") {
		t.Errorf("header: %s", h)
	}
	// Instances asleep are counted apart, not as unhealthy.
	m.st.Totals = types.ClusterTotals{Nodes: 3, NodesReady: 3, Instances: 21, InstancesHealthy: 21, InstancesAsleep: 1}
	if h := ansi.Strip(m.header()); !strings.Contains(h, "21/21 instances healthy · 1 asleep") {
		t.Errorf("header with an app asleep: %s", h)
	}

	// The cursor moves down the apps, the external app, then the databases.
	for range 3 {
		m.key("j")
	}
	if a, _ := m.selectedApp(); a.Name != "mysql-scratch" {
		t.Errorf("fourth row: %s", a.Name)
	}
	m.key("j")
	m.key("enter")
	if m.view != instancesView || m.filterApp != "postgres-shopdb" || len(m.instances()) != 1 {
		t.Errorf("drill into a database: view %v filter %q instances %d", m.view, m.filterApp, len(m.instances()))
	}

	// No databases, no heading (the tab is always there).
	m = &model{ctx: context.Background(), cursor: map[view]int{}, st: sampleStatus(), at: time.Now()}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if out := ansi.Strip(m.overview()); strings.Contains(out, "Databases") || strings.Contains(m.header(), "database") {
		t.Errorf("a Databases heading with none:\n%s", out)
	}
}

// TestAppsShowTheirDatabases: on the Apps view each app has the databases
// it uses under it, and the databases follow, each with the apps using it.
func TestAppsShowTheirDatabases(t *testing.T) {
	m := appsModel(120, 40)
	m.key("3")
	out := m.View()
	if os.Getenv("SHOW") != "" {
		for _, w := range []int{80, 100, 120} {
			mm := appsModel(w, 30)
			mm.key("1")
			t.Log("\n" + mm.View())
			mm.key("3")
			mm.key("G")
			t.Log("\n" + mm.View())
		}
	}
	apps := section(out, "  APP")
	var got []string
	for _, l := range apps {
		got = append(got, strings.Join(strings.Fields(l)[:2], " "))
	}
	if want := "[● blog └ shopdb ● shop └ shopdb └ cache]"; fmt.Sprint(got) != want {
		t.Errorf("apps and their databases: %v, want %s\n%s", got, want, ansi.Strip(out))
	}
	for _, l := range apps {
		switch f := strings.Fields(l); {
		case f[1] == "shopdb" && (!strings.Contains(l, "running") || !strings.Contains(l, "postgres") || !strings.HasSuffix(l, "DATABASE_URL")):
			t.Errorf("shopdb under an app: %q", l)
		case f[1] == "cache" && (!strings.Contains(l, "asleep") || !strings.HasSuffix(l, "REDIS_URL")):
			t.Errorf("cache under shop: %q", l)
		}
	}
	if ext := section(out, "  EXTERNAL"); len(ext) != 1 || !strings.Contains(ext[0], "→ home") ||
		!strings.Contains(ext[0], "http://192.168.1.50:8123") || !strings.HasSuffix(ext[0], "home.example.com") {
		t.Errorf("the external app: %q", ext)
	}
	dbs := section(out, "  DATABASE")
	if got := fmt.Sprint(names(dbs)); got != "[● scratch ● shopdb ● cache]" {
		t.Errorf("databases: %s\n%s", got, ansi.Strip(out))
	}
	if len(dbs) == 3 && (!strings.HasSuffix(dbs[0], "not linked") || !strings.HasSuffix(dbs[1], "blog, shop")) {
		t.Errorf("used by: %q", dbs)
	}

	// The lines under the apps aren't rows to select: the fourth row is
	// the first database, whose info command shows under the list.
	for range 3 {
		m.key("j")
	}
	if a, _ := m.selectedApp(); a.Name != "mysql-scratch" {
		t.Errorf("fourth row: %s", a.Name)
	}
	if out := ansi.Strip(m.View()); !strings.Contains(out, "mysql-scratch, a mysql database · jokku db:mysql:info scratch") {
		t.Errorf("no info command for the selected database:\n%s", out)
	}

	// The columns line up, sub-lines included, colors and all.
	defer lipgloss.SetColorProfile(lipgloss.ColorProfile())
	lipgloss.SetColorProfile(termenv.ANSI256)
	for _, w := range []int{80, 100, 160} {
		m := appsModel(w, 40)
		m.key("3")
		for _, l := range strings.Split(m.View(), "\n") {
			if lipgloss.Width(l) > w {
				t.Errorf("%d columns: a %d column line", w, lipgloss.Width(l))
			}
		}
	}
	m = appsModel(200, 40)
	m.key("3")
	col := -1
	for _, l := range strings.Split(ansi.Strip(m.View()), "\n") {
		for _, f := range []string{"DOMAINS", "USED BY", "shop.example.com", "home.example.com", "DATABASE_URL", "blog, shop"} {
			if i := strings.Index(l, f); i >= 0 {
				i = utf8.RuneCountInString(l[:i])
				if col == -1 {
					col = i
				} else if i != col {
					t.Errorf("%q starts at column %d, not %d: %q", f, i, col, l)
				}
			}
		}
	}
}

// TestAppsShowTheirSize: each app and database shows the vCPUs and memory
// of its instances, "mixed" when its process types differ, and the line
// under the Apps view names each process type's for the selected app.
func TestAppsShowTheirSize(t *testing.T) {
	m := appsModel(160, 40)
	m.key("3")
	out := ansi.Strip(m.View())
	for _, want := range []string{"SIZE", "● blog                 running    ●              1 vCPU 256M   v3",
		"└ shopdb             running    ●              1 vCPU 512M   postgres", "● scratch              running    ●              1 vCPU 1.0G   mysql"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "  blog · 1 web at 1 vCPU 256M · jokku resource:limit blog") {
		t.Errorf("no sizes under the list for the selected app:\n%s", out)
	}

	// A worker of another size makes shop mixed; the line under the list
	// names both.
	m.st.Instances = append(m.st.Instances, types.InstanceView{App: "shop",
		Instance: types.Instance{Name: "worker.1", Release: 12, Node: "worker-1", State: "healthy", Desired: "running", CPUs: 2, MemoryMB: 1024}})
	m.key("j")
	out = ansi.Strip(m.View())
	if !strings.Contains(out, "● shop                 degraded   ●●○            mixed         v12") {
		t.Errorf("shop isn't mixed:\n%s", out)
	}
	if !strings.Contains(out, "  shop · 2 web at 1 vCPU 256M · 1 worker at 2 vCPU 1.0G · jokku resource:limit shop") {
		t.Errorf("no sizes under the list for shop:\n%s", out)
	}

	// The overview shows them too.
	m.key("1")
	if out := ansi.Strip(m.View()); !strings.Contains(out, "● blog                 ●            1 vCPU 256M   v3") {
		t.Errorf("overview:\n%s", out)
	}
}
