package tui

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/wes/jokku/internal/types"
)

func sampleBackups() *types.BackupsOverview {
	now := time.Now()
	run := func(ago time.Duration, status string) *types.BackupRun {
		return &types.BackupRun{Name: now.Add(-ago).UTC().Format("2006-01-02T15-04-05Z"), Status: status, StartedAt: now.Add(-ago - time.Minute),
			FinishedAt: now.Add(-ago), Error: map[string]string{types.StatusFailed: "uploading a block: connection reset"}[status]}
	}
	points := func(n int, fail int) []types.BackupPoint {
		var out []types.BackupPoint
		for i := range n {
			p := types.BackupPoint{At: now.Add(-time.Duration(n-i) * 15 * time.Minute), Status: types.StatusSucceeded, NewBytes: int64((i*7919)%13+1) << 20}
			if i == fail {
				p.Status = types.StatusFailed
			}
			out = append(out, p)
		}
		return out
	}
	ok := func(every time.Duration, last time.Duration) *types.VolumeBackups {
		r := run(last, types.StatusSucceeded)
		return &types.VolumeBackups{Destination: "tigris", Path: "jokku/shop/pgdata", EverySeconds: int64(every / time.Second),
			KeepRecentSeconds: 86400, KeepDaily: 30, Next: r.StartedAt.Add(every), AutoRestore: true, FailoverSeconds: 300,
			StoredBytes: 1400 << 20, StoredBackups: 98, Last: r, Succeeded: r}
	}
	failed := ok(15*time.Minute, 2*time.Hour)
	failed.Last = run(9*time.Minute, types.StatusFailed)
	cluster := ok(time.Hour, 12*time.Minute)
	cluster.Cluster, cluster.Path = true, "jokku/cluster"
	return &types.BackupsOverview{
		Destinations: []types.BackupDestination{{Name: "tigris", Bucket: "acme-backups", Encrypt: true}},
		Key:          &types.BackupKeyStatus{ID: "3f9a1b2c4d5e6f70", Saved: true},
		Cluster:      cluster, ClusterRecent: points(20, -1),
		Volumes: []types.VolumeProtection{
			{App: "shop", Volume: "pgdata", Node: "jokku2", SizeMB: 10240, UsedMB: 1300, Status: "ready", Backups: ok(15*time.Minute, 4*time.Minute), Recent: points(24, -1)},
			{App: "duckhouse-status", Volume: "kuma-data", Node: "jokku2", SizeMB: 1024, UsedMB: 120, Status: "ready", Backups: failed, Recent: points(24, 23)},
			{App: "blog", Volume: "uploads", Node: "jokku1", SizeMB: 10240, UsedMB: 3100, Status: "ready"},
		},
	}
}

func backupsModel(w, h int) *model {
	m := &model{ctx: context.Background(), cursor: map[view]int{}, st: sampleStatus(), at: time.Now(), bk: sampleBackups()}
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.key("7")
	return m
}

func TestBackupsView(t *testing.T) {
	if os.Getenv("SHOW") != "" {
		for _, w := range []int{80, 100} {
			mm := backupsModel(w, 30)
			mm.key("j")
			t.Log("\n" + mm.View())
		}
	}
	m := backupsModel(150, 45)
	out := m.View()
	for _, want := range []string{"7 Backups", "1 volume failed to back up", "1 volume not backed up", "2 of 3 volumes",
		"cluster (control node)", "shop / pgdata", "not backed up", "saved", "tigris"} {
		if !strings.Contains(out, want) {
			t.Errorf("the view lacks %q", want)
		}
	}

	// Turning a volume's backups off takes two presses; on takes one.
	m.key("j") // shop / pgdata
	if cmd := m.key(" "); cmd != nil || !strings.Contains(m.View(), "press space again") {
		t.Error("a first space turned backups off")
	}
	if cmd := m.key(" "); cmd == nil {
		t.Error("a second space didn't turn backups off")
	}
	m.bkBusy = map[string]string{}
	m.key("j")
	m.key("j") // blog / uploads, not backed up
	if cmd := m.key(" "); cmd == nil || m.bkBusy["blog/uploads"] == "" {
		t.Error("space didn't turn backups on for an unprotected volume")
	}
	if !strings.Contains(m.View(), "saving") {
		t.Error("the row doesn't say it is saving")
	}

	// With two destinations, turning on asks which.
	m = backupsModel(150, 45)
	m.bk.Destinations = append(m.bk.Destinations, types.BackupDestination{Name: "r2", Bucket: "b2", Encrypt: true})
	m.key("G")
	if cmd := m.key(" "); cmd != nil || !m.bkPicking || !strings.Contains(m.View(), "Back up blog / uploads to:") {
		t.Fatal("no destination picker")
	}
	m.key("right")
	if cmd := m.key("enter"); cmd == nil || m.bkPicking {
		t.Error("enter didn't pick a destination")
	}

	// All well: it says so.
	m = backupsModel(150, 45)
	m.bk.Volumes = m.bk.Volumes[:1]
	if out := m.View(); !strings.Contains(out, "Everything is backed up") {
		t.Errorf("all backed up, but the view doesn't say so:\n%s", out)
	}
	if s := nextSchedule(15 * 60); s != "1h" {
		t.Errorf("after 15m comes %q", s)
	}
	if s := nextSchedule(86400); s != "off" {
		t.Errorf("after 1d comes %q", s)
	}
}

func TestBackupsViewFits(t *testing.T) {
	for _, size := range [][2]int{{200, 50}, {150, 45}, {100, 30}, {80, 24}, {40, 10}} {
		m := backupsModel(size[0], size[1])
		for _, k := range []string{"7", "j", "j", "j", "b", "s", "a", "k", " ", "G", "1"} {
			m.key(k)
			for _, l := range strings.Split(m.View(), "\n") {
				if w := lipgloss.Width(l); w > size[0] {
					t.Errorf("%dx%d after %q: a %d column line", size[0], size[1], k, w)
					break
				}
			}
			if n := strings.Count(m.View(), "\n") + 1; n > size[1] {
				t.Errorf("%dx%d after %q: %d lines", size[0], size[1], k, n)
			}
		}
	}
}
