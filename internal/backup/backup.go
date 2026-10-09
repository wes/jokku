// Package backup copies volume disks to S3-compatible object storage and
// back. A volume's backups live under one path in a bucket:
//
//	<path>/jokku-backup.json      what the backups are: format, volume, key ID
//	<path>/backups/<time>.backup  one per backup: the disk's size and which block goes where
//	<path>/blocks/ab/abcd...      the disk's 1 MiB blocks, named by their contents
//
// A block is uploaded once and shared by every backup that holds it, so
// each backup is complete on its own but only adds the blocks that changed.
// Holes and all-zero blocks are never stored. Blocks and backup files are
// compressed, and encrypted when there is a key.
package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wes/jokku/internal/volume"
)

const (
	infoFile   = "jokku-backup.json"
	backupsDir = "backups/"
	blocksDir  = "blocks/"
	backupExt  = ".backup"
	// NameFormat names backups by when they were made, in UTC; names sort
	// in time order.
	NameFormat = "2006-01-02T15-04-05Z"
)

// Info is jokku-backup.json, which says what a path holds.
type Info struct {
	Format    int    `json:"format"`
	BlockSize int    `json:"block_size"`
	Encrypted bool   `json:"encrypted"`
	KeyID     string `json:"key_id,omitempty"`
	// Volume is the ID of the volume whose backups these are, so two
	// volumes (on two clusters, say) never mix their backups in one path.
	Volume string `json:"volume"`
	App    string `json:"app,omitempty"`
	Name   string `json:"name,omitempty"`
}

// Manifest is one backup: the disk's size and its blocks.
type Manifest struct {
	Version   int       `json:"version"`
	Name      string    `json:"name"`
	Volume    string    `json:"volume"`
	Created   time.Time `json:"created"`
	Size      int64     `json:"size"`
	BlockSize int       `json:"block_size"`
	// Index[i] is where the block Names[i] goes, in blocks.
	Index []int64  `json:"index"`
	Names []string `json:"names"`
}

// Options say what to back up, and where.
type Options struct {
	Store Store
	Key   *Key // nil: not encrypted
	Path  string

	Volume, App, VolumeName string // recorded in Info
	Disk                    string // the disk image
	Name                    string // the backup's name, from NameFormat

	// Freeze, when something may be writing to the disk, stops writes
	// until thaw is called, so the backup is the disk at one moment.
	Freeze func(ctx context.Context) (thaw func() error, err error)
	// Staging is a directory for blocks that change while the backup runs.
	Staging string
	Log     func(string)
}

// Result says what a backup stored.
type Result struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`       // the disk's size
	Blocks    int    `json:"blocks"`     // blocks holding data
	NewBlocks int    `json:"new_blocks"` // of them, not already in the bucket
	NewBytes  int64  `json:"new_bytes"`  // what was uploaded, after compression
}

func prefix(p string) string { return strings.Trim(p, "/") + "/" }

func blockKey(p, name string) string { return prefix(p) + blocksDir + name[:2] + "/" + name }

func backupKey(p, name string) string { return prefix(p) + backupsDir + name + backupExt }

// CheckPath refuses a backup path that is empty or climbs out of the
// bucket's folders.
func CheckPath(p string) error {
	clean := path.Clean("/" + p)
	if strings.Trim(p, "/") == "" || clean != "/"+strings.Trim(p, "/") {
		return fmt.Errorf("%q is not a path in a bucket, like jokku/myapp/data", p)
	}
	return nil
}

// ReadInfo reads what a path holds; nil if it holds nothing yet.
func ReadInfo(ctx context.Context, st Store, p string) (*Info, error) {
	b, err := st.Get(ctx, prefix(p)+infoFile)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var info Info
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, fmt.Errorf("%s%s is not Jokku's: %w", prefix(p), infoFile, err)
	}
	return &info, nil
}

// checkKey makes sure key reads what info says the path holds.
func checkKey(info *Info, key *Key, p string) error {
	switch {
	case info.Format != 1 || info.BlockSize != volume.BlockSize:
		return fmt.Errorf("the backups in %s were made by a newer Jokku (format %d)", p, info.Format)
	case info.Encrypted && key == nil:
		return fmt.Errorf("the backups in %s are encrypted, and this destination doesn't encrypt", p)
	case !info.Encrypted && key != nil:
		return fmt.Errorf("the backups in %s are not encrypted, and this destination encrypts; use another path", p)
	case info.Encrypted && info.KeyID != key.ID():
		return fmt.Errorf("the backups in %s were made with another key (ID %s; this cluster's is %s)", p, info.KeyID, key.ID())
	}
	return nil
}

// List returns the names of the backups in a path, oldest first.
func List(ctx context.Context, st Store, p string) ([]string, error) {
	keys, err := st.List(ctx, prefix(p)+backupsDir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, k := range keys {
		if name, ok := strings.CutSuffix(strings.TrimPrefix(k, prefix(p)+backupsDir), backupExt); ok && !strings.Contains(name, "/") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// ReadManifest reads one backup's manifest.
func ReadManifest(ctx context.Context, st Store, key *Key, p, name string) (*Manifest, error) {
	data, err := st.Get(ctx, backupKey(p, name))
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("there is no backup %s in %s", name, p)
	}
	if err != nil {
		return nil, err
	}
	plain, err := newCodec(key).open(data, "backup "+name)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(plain, &m); err != nil || len(m.Index) != len(m.Names) {
		return nil, fmt.Errorf("backup %s is damaged", name)
	}
	if m.BlockSize != volume.BlockSize {
		return nil, fmt.Errorf("backup %s uses %d-byte blocks, this Jokku %d", name, m.BlockSize, volume.BlockSize)
	}
	return &m, nil
}

// Run backs up a disk: blocks the bucket doesn't have yet are uploaded
// while the app runs, then, with writes frozen, only the blocks that
// changed meanwhile are copied aside, to be uploaded once writes resume.
func Run(ctx context.Context, o Options) (*Result, error) {
	if err := CheckPath(o.Path); err != nil {
		return nil, err
	}
	if o.Log == nil {
		o.Log = func(string) {}
	}
	info, err := ReadInfo(ctx, o.Store, o.Path)
	if err != nil {
		return nil, err
	}
	switch {
	case info == nil:
		info = &Info{Format: 1, BlockSize: volume.BlockSize, Encrypted: o.Key != nil, Volume: o.Volume, App: o.App, Name: o.VolumeName}
		if o.Key != nil {
			info.KeyID = o.Key.ID()
		}
		b, _ := json.MarshalIndent(info, "", "  ")
		if err := o.Store.Put(ctx, prefix(o.Path)+infoFile, append(b, '\n')); err != nil {
			return nil, err
		}
	case info.Volume != o.Volume:
		return nil, fmt.Errorf("%s holds the backups of another volume (%s of %s, ID %s); back this one up to another path", o.Path, info.Name, info.App, info.Volume)
	}
	if err := checkKey(info, o.Key, o.Path); err != nil {
		return nil, err
	}

	// What the latest backup holds is in the bucket already.
	c := newCodec(o.Key)
	known := map[string]bool{}
	names, err := List(ctx, o.Store, o.Path)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if n == o.Name {
			return nil, fmt.Errorf("there is already a backup %s", n)
		}
	}
	if len(names) > 0 {
		last, err := ReadManifest(ctx, o.Store, o.Key, o.Path, names[len(names)-1])
		if err != nil {
			return nil, err
		}
		for _, n := range last.Names {
			known[n] = true
		}
	}

	f, err := os.Open(o.Disk)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	up := newUploader(ctx, cancel, o.Store, c, o.Path)
	var read atomic.Int64
	defer progress(o, f, &read, up)()
	blocks := map[int64]string{}
	size, err := volume.EachBlock(f, func(i int64, b []byte) error {
		read.Store((i + 1) * volume.BlockSize)
		n := c.name(b)
		blocks[i] = n
		if !known[n] {
			known[n] = true
			return up.add(n, append([]byte(nil), b...))
		}
		return nil
	})
	if err != nil {
		cancel(err)
		up.wait()
		return nil, err
	}

	if o.Freeze != nil {
		size, blocks, err = frozenPass(ctx, o, f, c, known, up)
		if err != nil {
			cancel(err)
			up.wait()
			return nil, err
		}
	}
	if err := up.wait(); err != nil {
		return nil, err
	}

	m := Manifest{Version: 1, Name: o.Name, Volume: o.Volume, Created: time.Now().UTC(), Size: size, BlockSize: volume.BlockSize}
	for i := range blocks {
		m.Index = append(m.Index, i)
	}
	sort.Slice(m.Index, func(a, b int) bool { return m.Index[a] < m.Index[b] })
	for _, i := range m.Index {
		m.Names = append(m.Names, blocks[i])
	}
	plain, _ := json.Marshal(m)
	if err := o.Store.Put(ctx, backupKey(o.Path, o.Name), c.seal(plain, "backup "+o.Name)); err != nil {
		return nil, err
	}
	return &Result{Name: o.Name, Size: size, Blocks: len(blocks), NewBlocks: up.count, NewBytes: up.bytes.Load()}, nil
}

// progress logs how far the first pass got every 10 seconds, until the
// returned func is called.
func progress(o Options, f *os.File, read *atomic.Int64, up *uploader) func() {
	st, err := f.Stat()
	if err != nil {
		return func() {}
	}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				o.Log(fmt.Sprintf("Read %d of %d MB; uploaded %d MB", min(read.Load(), st.Size())>>20, st.Size()>>20, up.bytes.Load()>>20))
			case <-stop:
				return
			}
		}
	}()
	return func() { close(stop) }
}

// frozenPass reads the disk again with writes frozen. Blocks that changed
// since the first pass are copied to a staging file before writes resume,
// and uploaded after.
func frozenPass(ctx context.Context, o Options, f *os.File, c *codec, known map[string]bool, up *uploader) (int64, map[int64]string, error) {
	staging, err := os.CreateTemp(o.Staging, "backup-*.staging")
	if err != nil {
		return 0, nil, err
	}
	defer os.Remove(staging.Name())
	defer staging.Close()
	type staged struct {
		name string
		off  int64
		n    int
	}
	var later []staged
	var off int64

	thaw, err := o.Freeze(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("pausing writes: %w", err)
	}
	start := time.Now()
	blocks := map[int64]string{}
	size, err := volume.EachBlock(f, func(i int64, b []byte) error {
		n := c.name(b)
		blocks[i] = n
		if known[n] {
			return nil
		}
		known[n] = true
		if _, err := staging.WriteAt(b, off); err != nil {
			return err
		}
		later = append(later, staged{n, off, len(b)})
		off += int64(len(b))
		return nil
	})
	if terr := thaw(); err == nil && terr != nil {
		err = fmt.Errorf("resuming writes: %w", terr)
	}
	if err != nil {
		return 0, nil, err
	}
	o.Log(fmt.Sprintf("Writes were paused for %s; %d blocks had changed", time.Since(start).Round(time.Millisecond), len(later)))
	for _, s := range later {
		b := make([]byte, s.n)
		if _, err := staging.ReadAt(b, s.off); err != nil {
			return 0, nil, err
		}
		if err := up.add(s.name, b); err != nil {
			return 0, nil, err
		}
	}
	return size, blocks, nil
}

// uploader puts blocks into the bucket, several at a time.
type uploader struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	jobs   chan job
	wg     sync.WaitGroup
	count  int
	bytes  atomic.Int64
}

type job struct {
	name string
	data []byte
}

const workers = 8

func newUploader(ctx context.Context, cancel context.CancelCauseFunc, st Store, c *codec, p string) *uploader {
	u := &uploader{ctx: ctx, cancel: cancel, jobs: make(chan job, workers*2)}
	for range workers {
		u.wg.Add(1)
		go func() {
			defer u.wg.Done()
			for j := range u.jobs {
				if ctx.Err() != nil {
					continue
				}
				sealed := c.seal(j.data, "block "+j.name)
				if err := st.Put(ctx, blockKey(p, j.name), sealed); err != nil {
					cancel(fmt.Errorf("uploading a block: %w", err))
					continue
				}
				u.bytes.Add(int64(len(sealed)))
			}
		}()
	}
	return u
}

func (u *uploader) add(name string, data []byte) error {
	select {
	case u.jobs <- job{name, data}:
		u.count++
		return nil
	case <-u.ctx.Done():
		return context.Cause(u.ctx)
	}
}

// wait finishes the uploads; the error is the first that stopped them.
func (u *uploader) wait() error {
	close(u.jobs)
	u.wg.Wait()
	if u.ctx.Err() != nil {
		return context.Cause(u.ctx)
	}
	return nil
}

// Restore writes backup name of a path to dst, a new disk image, checking
// every block against its name. progress is told the bytes written so far
// and the total.
func Restore(ctx context.Context, st Store, key *Key, p, name, dst string, progress func(done, total int64)) (*Manifest, error) {
	info, err := ReadInfo(ctx, st, p)
	if err != nil {
		return nil, err
	}
	if info == nil {
		return nil, fmt.Errorf("there are no backups in %s", p)
	}
	if err := checkKey(info, key, p); err != nil {
		return nil, err
	}
	m, err := ReadManifest(ctx, st, key, p, name)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := f.Truncate(m.Size); err != nil {
		return nil, err
	}

	// Each distinct block is fetched once, then written wherever it goes.
	where := map[string][]int64{}
	var order []string
	for i, n := range m.Names {
		if len(where[n]) == 0 {
			order = append(order, n)
		}
		where[n] = append(where[n], m.Index[i])
	}
	c := newCodec(key)
	total := int64(len(m.Names)) * volume.BlockSize
	var done atomic.Int64
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	names := make(chan string)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range names {
				if err := restoreBlock(ctx, st, c, p, n, f, where[n]); err != nil {
					cancel(err)
					continue
				}
				d := done.Add(int64(len(where[n])) * volume.BlockSize)
				if progress != nil {
					progress(min(d, total), total)
				}
			}
		}()
	}
	for _, n := range order {
		select {
		case names <- n:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(names)
	wg.Wait()
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	return m, f.Sync()
}

func restoreBlock(ctx context.Context, st Store, c *codec, p, n string, f *os.File, at []int64) error {
	if ctx.Err() != nil {
		return nil
	}
	data, err := st.Get(ctx, blockKey(p, n))
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("block %s is missing from the bucket", n)
	}
	if err != nil {
		return err
	}
	plain, err := c.open(data, "block "+n)
	if err != nil {
		return err
	}
	if len(plain) > volume.BlockSize || c.name(plain) != n {
		return fmt.Errorf("block %s doesn't match its name: the bucket was changed", n)
	}
	for _, i := range at {
		if _, err := f.WriteAt(plain, i*volume.BlockSize); err != nil {
			return err
		}
	}
	return nil
}
