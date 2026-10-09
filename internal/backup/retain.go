package backup

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Retention says which backups to keep: every one from the last Recent,
// the last one of each of the last Daily days (UTC, today included), and
// always the newest.
type Retention struct {
	Recent time.Duration
	Daily  int
	Also   []string // backups to keep regardless, such as one about to be restored
}

// Keep picks the backups (names, from NameFormat) that r keeps at now.
// Names that aren't times are kept: they aren't Jokku's to delete.
func (r Retention) Keep(names []string, now time.Time) map[string]bool {
	keep := map[string]bool{}
	lastOfDay := map[string]string{}
	newest := ""
	firstDay := now.UTC().AddDate(0, 0, -r.Daily+1).Format(time.DateOnly)
	for _, n := range names {
		t, err := time.Parse(NameFormat, n)
		if err != nil {
			keep[n] = true
			continue
		}
		newest = max(newest, n)
		if now.Sub(t) < r.Recent {
			keep[n] = true
		}
		if day := t.Format(time.DateOnly); r.Daily > 0 && day >= firstDay && n > lastOfDay[day] {
			lastOfDay[day] = n
		}
	}
	for _, n := range lastOfDay {
		keep[n] = true
	}
	for _, n := range r.Also {
		keep[n] = true
	}
	if newest != "" {
		keep[newest] = true
	}
	return keep
}

// Prune deletes the backups in a path that r doesn't keep, returning the
// names deleted and how many are left. The blocks only they used stay until
// Collect.
func Prune(ctx context.Context, st Store, p string, r Retention, now time.Time) ([]string, int, error) {
	names, err := List(ctx, st, p)
	if err != nil {
		return nil, 0, err
	}
	keep := r.Keep(names, now)
	var deleted []string
	for _, n := range names {
		if keep[n] {
			continue
		}
		if err := st.Delete(ctx, backupKey(p, n)); err != nil {
			return deleted, len(names) - len(deleted), err
		}
		deleted = append(deleted, n)
	}
	return deleted, len(names) - len(deleted), nil
}

// Usage is what a path holds.
type Usage struct {
	Backups int
	Blocks  int
	Bytes   int64 // blocks and backup files
}

// Collect deletes the blocks no backup in a path uses, and says what is
// left. It must not run while a backup of the path is: that backup's new
// blocks aren't in any backup file yet.
func Collect(ctx context.Context, st Store, key *Key, p string) (Usage, int, int64, error) {
	var u Usage
	names, err := List(ctx, st, p)
	if err != nil {
		return u, 0, 0, err
	}
	used := map[string]bool{}
	for _, n := range names {
		m, err := ReadManifest(ctx, st, key, p, n)
		if err != nil {
			return u, 0, 0, fmt.Errorf("reading backup %s: %w", n, err)
		}
		for _, b := range m.Names {
			used[b] = true
		}
	}
	objs, err := st.List(ctx, prefix(p))
	if err != nil {
		return u, 0, 0, err
	}
	deleted, freed := 0, int64(0)
	for _, o := range objs {
		rest := strings.TrimPrefix(o.Key, prefix(p))
		if !strings.HasPrefix(rest, blocksDir) {
			u.Bytes += o.Size
			continue
		}
		name := rest[strings.LastIndex(rest, "/")+1:]
		if used[name] {
			u.Blocks++
			u.Bytes += o.Size
			continue
		}
		if err := st.Delete(ctx, o.Key); err != nil {
			return u, deleted, freed, err
		}
		deleted++
		freed += o.Size
	}
	u.Backups = len(names)
	return u, deleted, freed, nil
}
