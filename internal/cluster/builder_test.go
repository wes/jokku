package cluster_test

import (
	"archive/tar"
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/wes/jokku/internal/types"
)

// An image app runs its image: builder:image deploys it, ps:rebuild pulls it
// again, and pushes are refused until the app builds from git again.
func TestImageApps(t *testing.T) {
	h := newHarness(t)
	ctx := h.ctx
	if _, err := h.st.CreateApp(ctx, "cache"); err != nil {
		t.Fatal(err)
	}
	h.st.SetProperty(ctx, "cache", "checks", "wait-to-retire", "0")
	discard := func(types.Event) {}
	if err := h.client.DeployImage(ctx, "cache", "redis:7", discard); err != nil {
		t.Fatal(err)
	}
	if b, _ := h.st.Properties(ctx, "cache", "builder"); b["type"] != "image" || b["image"] != "redis:7" {
		t.Fatalf("after an image deploy, builder = %v", b)
	}
	h.ps("cache", "rebuild")
	if got := h.builder.builds(); !slices.Equal(got, []string{"redis:7", "redis:7"}) {
		t.Errorf("ps:rebuild of an image app: built %v", got)
	}

	push := func(files map[string]string) error {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, body := range files {
			tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
			tw.Write([]byte(body))
		}
		tw.Close()
		return h.client.Deploy(ctx, "cache", "git", "abc1234", &buf, discard)
	}
	repo := map[string]string{"Dockerfile": "FROM scratch\nCOPY . /app\n"}
	if err := push(repo); err == nil || !strings.Contains(err.Error(), "cache runs the image redis:7, so pushes don't deploy it") {
		t.Fatalf("push to an image app: %v", err)
	}

	// Back to Dockerfile builds. Nothing has been pushed, so there is nothing
	// to rebuild: the image deploys' sources aren't the app's.
	if err := h.client.SetBuilder(ctx, "cache", "dockerfile", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.client.PS(ctx, "cache", "rebuild", discard); err == nil || !strings.Contains(err.Error(), "no source to rebuild") {
		t.Errorf("ps:rebuild before any push: %v", err)
	}
	if err := push(repo); err != nil {
		t.Fatal(err)
	}
	h.ps("cache", "rebuild")
	if got := h.builder.builds(); !slices.Equal(got, []string{"redis:7", "redis:7", "build", "build"}) {
		t.Errorf("after switching back: built %v", got)
	}
	if b, _ := h.st.Properties(ctx, "cache", "builder"); b["type"] != "dockerfile" || b["image"] != "" {
		t.Errorf("builder = %v", b)
	}
}
