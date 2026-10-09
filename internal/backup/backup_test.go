package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wes/jokku/internal/volume"
)

const mb = volume.BlockSize

// disk writes a sparse disk image of size MiB with data at the given blocks.
func disk(t *testing.T, path string, size int, blocks map[int64][]byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Truncate(int64(size) * mb)
	for i, b := range blocks {
		if _, err := f.WriteAt(b, i*mb); err != nil {
			t.Fatal(err)
		}
	}
}

func block(fill byte) []byte { return bytes.Repeat([]byte{fill}, mb) }

func random() []byte {
	b := make([]byte, mb)
	rand.Read(b)
	return b
}

func run(t *testing.T, st Store, key *Key, src, name string, freeze func(context.Context) (func() error, error)) *Result {
	t.Helper()
	res, err := Run(context.Background(), Options{
		Store: st, Key: key, Path: "jokku/shop/data", Volume: "vol1", App: "shop", VolumeName: "data",
		Disk: src, Name: name, Freeze: freeze, Staging: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("backup %s: %v", name, err)
	}
	return res
}

func restore(t *testing.T, st Store, key *Key, name string) []byte {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "restored.ext4")
	if _, err := Restore(context.Background(), st, key, "jokku/shop/data", name, dst, nil); err != nil {
		t.Fatalf("restore %s: %v", name, err)
	}
	b, _ := os.ReadFile(dst)
	return b
}

func TestBackupsAreIncrementalAndRestore(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "encrypted"}[encrypted], func(t *testing.T) {
			var key *Key
			if encrypted {
				key = NewKey()
			}
			st := Dir(t.TempDir())
			src := filepath.Join(t.TempDir(), "disk.ext4")
			a, b := random(), random()
			disk(t, src, 64, map[int64][]byte{0: a, 1: block(0), 5: b, 9: a})

			first := run(t, st, key, src, "2026-10-09T10-00-00Z", nil)
			// Blocks 0 and 9 are the same, and block 1 is all zeros.
			if first.Blocks != 3 || first.NewBlocks != 2 || first.Size != 64*mb {
				t.Fatalf("first backup: %+v", first)
			}
			want, _ := os.ReadFile(src)

			// Only what changed is uploaded the next time.
			c := random()
			disk(t, src, 64, map[int64][]byte{0: a, 5: c, 9: a})
			second := run(t, st, key, src, "2026-10-09T10-15-00Z", nil)
			if second.NewBlocks != 1 {
				t.Fatalf("second backup uploaded %d blocks, want 1", second.NewBlocks)
			}

			if got := restore(t, st, key, "2026-10-09T10-00-00Z"); !bytes.Equal(got, want) {
				t.Error("the first backup does not restore the first disk")
			}
			if got, now := restore(t, st, key, "2026-10-09T10-15-00Z"), must(os.ReadFile(src)); !bytes.Equal(got, now) {
				t.Error("the second backup does not restore the second disk")
			}
			names, err := List(context.Background(), st, "jokku/shop/data")
			if err != nil || strings.Join(names, ",") != "2026-10-09T10-00-00Z,2026-10-09T10-15-00Z" {
				t.Errorf("List = %v, %v", names, err)
			}

			// Encrypted blocks give nothing away: not their contents, and not
			// their SHA-256 either.
			objs, _ := st.List(context.Background(), "jokku/shop/data/blocks/")
			plainName := newCodec(nil).name(a)
			for _, o := range objs {
				k := o.Key
				data := must(st.Get(context.Background(), k))
				if encrypted && (strings.HasSuffix(k, plainName) || bytes.Contains(data, a[:64])) {
					t.Errorf("%s gives away its contents", k)
				}
			}
		})
	}
}

func TestFreezeCapturesTheDiskAtOneMoment(t *testing.T) {
	st := Dir(t.TempDir())
	src := filepath.Join(t.TempDir(), "disk.ext4")
	disk(t, src, 16, map[int64][]byte{0: block(1), 1: block(2)})
	// The app writes during the first pass; the frozen pass catches it.
	late := random()
	froze := false
	freeze := func(context.Context) (func() error, error) {
		froze = true
		f, _ := os.OpenFile(src, os.O_RDWR, 0)
		f.WriteAt(late, 3*mb)
		f.Close()
		return func() error { return nil }, nil
	}
	res := run(t, st, nil, src, "2026-10-09T10-00-00Z", freeze)
	if !froze || res.Blocks != 3 {
		t.Fatalf("froze %v, %+v", froze, res)
	}
	if got := restore(t, st, nil, "2026-10-09T10-00-00Z"); !bytes.Equal(got[3*mb:4*mb], late) {
		t.Error("a write made before the freeze is missing")
	}
}

func TestBackupsRefuseWhatTheyCannotRead(t *testing.T) {
	ctx := context.Background()
	st := Dir(t.TempDir())
	src := filepath.Join(t.TempDir(), "disk.ext4")
	disk(t, src, 4, map[int64][]byte{0: random()})
	key := NewKey()
	run(t, st, key, src, "2026-10-09T10-00-00Z", nil)

	other := NewKey()
	dst := filepath.Join(t.TempDir(), "x")
	if _, err := Restore(ctx, st, other, "jokku/shop/data", "2026-10-09T10-00-00Z", dst, nil); err == nil || !strings.Contains(err.Error(), "another key") {
		t.Errorf("restore with another key: %v", err)
	}
	if _, err := Restore(ctx, st, nil, "jokku/shop/data", "2026-10-09T10-00-00Z", dst, nil); err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("restore without a key: %v", err)
	}
	// Another volume can't back up into the same path.
	_, err := Run(ctx, Options{Store: st, Key: key, Path: "jokku/shop/data", Volume: "vol2", Disk: src, Name: "2026-10-09T11-00-00Z", Staging: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "another volume") {
		t.Errorf("another volume in the same path: %v", err)
	}

	// A changed block is caught.
	objs, _ := st.List(ctx, "jokku/shop/data/blocks/")
	data := must(st.Get(ctx, objs[0].Key))
	data[len(data)-1] ^= 1
	st.Put(ctx, objs[0].Key, data)
	if _, err := Restore(ctx, st, key, "jokku/shop/data", "2026-10-09T10-00-00Z", dst, nil); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Errorf("restore of a changed block: %v", err)
	}

	for _, p := range []string{"", "/", "a/../b", "../x"} {
		if CheckPath(p) == nil {
			t.Errorf("CheckPath(%q) passed", p)
		}
	}
}

func TestKeys(t *testing.T) {
	k := NewKey()
	back, err := ParseKey("  " + k.String() + "\n")
	if err != nil || back.ID() != k.ID() || back.String() != k.String() {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := ParseKey("jbk1_short"); err == nil {
		t.Error("a short key parsed")
	}
	if NewKey().ID() == k.ID() {
		t.Error("two keys share an ID")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
