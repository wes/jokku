package backup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"
)

func TestRetentionKeeps(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	var names []string
	// Every 6 hours for 5 days back.
	for h := 0; h <= 5*24; h += 6 {
		names = append(names, now.Add(-time.Duration(h)*time.Hour).Format(NameFormat))
	}
	names = append(names, "not-a-time")
	keep := Retention{Recent: 24 * time.Hour, Daily: 3}.Keep(names, now)
	var kept []string
	for n := range keep {
		kept = append(kept, n)
	}
	sort.Strings(kept)
	want := []string{
		"2026-10-07T18-00-00Z", // the last of Oct 7, the third day
		"2026-10-08T18-00-00Z", // within 24h from here on
		"2026-10-09T00-00-00Z",
		"2026-10-09T06-00-00Z",
		"2026-10-09T12-00-00Z",
		"not-a-time",
	}
	// 2026-10-08T12-00-00Z is exactly 24h old: out of Recent, and not the
	// last of its day.
	if !slices.Equal(kept, want) {
		t.Errorf("kept %v\nwant %v", kept, want)
	}

	// The newest backup stays, however old.
	old := []string{"2020-01-01T00-00-00Z", "2020-01-02T00-00-00Z"}
	if keep := (Retention{Recent: time.Hour, Daily: 1}).Keep(old, now); len(keep) != 1 || !keep["2020-01-02T00-00-00Z"] {
		t.Errorf("old backups: kept %v", keep)
	}
}

func TestPruneAndCollect(t *testing.T) {
	ctx := context.Background()
	st := Dir(t.TempDir())
	key := NewKey()
	src := filepath.Join(t.TempDir(), "disk.ext4")
	shared := random()
	var last []byte
	for i, name := range []string{"2026-10-01T00-00-00Z", "2026-10-02T00-00-00Z", "2026-10-03T00-00-00Z"} {
		disk(t, src, 8, map[int64][]byte{0: shared, 1 + int64(i): random()})
		run(t, st, key, src, name, nil)
		last, _ = os.ReadFile(src)
	}
	before, _ := st.List(ctx, "jokku/shop/data/blocks/")
	if len(before) != 4 {
		t.Fatalf("%d blocks before, want 4", len(before))
	}

	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	deleted, left, err := Prune(ctx, st, "jokku/shop/data", Retention{Recent: time.Hour, Daily: 2}, now)
	if err != nil || !slices.Equal(deleted, []string{"2026-10-01T00-00-00Z"}) || left != 2 {
		t.Fatalf("Prune: %v, %d, %v", deleted, left, err)
	}
	u, n, freed, err := Collect(ctx, st, key, "jokku/shop/data")
	if err != nil || n != 1 || freed == 0 || u.Backups != 2 || u.Blocks != 3 {
		t.Fatalf("Collect: %+v, deleted %d, freed %d, %v", u, n, freed, err)
	}
	if got := restore(t, st, key, "2026-10-03T00-00-00Z"); !bytes.Equal(got, last) {
		t.Error("the newest backup no longer restores")
	}
	if u, n, _, _ := Collect(ctx, st, key, "jokku/shop/data"); n != 0 || u.Blocks != 3 {
		t.Errorf("a second Collect deleted %d blocks", n)
	}
}
