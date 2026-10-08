package cluster

import (
	"context"
	"io"
	"os"
	"path/filepath"

	"github.com/wes/jokku/internal/types"
)

// LocalPlane is the control plane as the control node's own agent sees it:
// the same calls workers make over HTTPS, made in-process.
type LocalPlane struct {
	C       *Controller
	DataDir string
}

func (p *LocalPlane) State(ctx context.Context, etag string) (*types.NodeState, error) {
	return p.C.State(ctx, p.C.Self, etag)
}

func (p *LocalPlane) Report(ctx context.Context, st *types.NodeStatus) error {
	return p.C.Report(ctx, p.C.Self, st)
}

// Artifact is only needed when an artifact is missing locally, which on the
// control node (where they are built) means it was deleted.
func (p *LocalPlane) Artifact(ctx context.Context, name string, w io.Writer) error {
	f, err := os.Open(filepath.Join(p.DataDir, "artifacts", filepath.Base(name)))
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
