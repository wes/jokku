package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wes/jokku/internal/types"
)

// Updating jokku itself
//
// The control node updates the cluster one node at a time by putting an
// update order in a node's desired state. The agent starts the update
// outside jokku's own service (it stops and restarts jokku) and first notes
// the attempt, so that it tries each one once and can tell afterwards how it
// went: back on the new version, it worked; back on the old one with no
// update running, update.sh failed and put the old version back.

// Updater updates jokku on this node to another release.
type Updater interface {
	// Start begins the update in the background.
	Start(ctx context.Context, version string) error
	// Running reports whether an update is under way.
	Running(ctx context.Context) bool
}

// updateAttempt is the last update order this node acted on.
type updateAttempt struct {
	Attempt string `json:"attempt"`
	Version string `json:"version"`
	Error   string `json:"error,omitempty"` // set once it is known to have failed
}

func (a *Agent) updatePath() string { return filepath.Join(a.DataDir, "update.json") }

func (a *Agent) loadUpdate() (*updateAttempt, error) {
	b, err := os.ReadFile(a.updatePath())
	if err != nil {
		return nil, err
	}
	var u updateAttempt
	return &u, json.Unmarshal(b, &u)
}

func (a *Agent) saveUpdate(u updateAttempt) error {
	b, err := json.Marshal(u)
	if err != nil {
		return err
	}
	tmp := a.updatePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.updatePath())
}

// maybeUpdate carries out an update order, unless it already has.
func (a *Agent) maybeUpdate(ctx context.Context, o *types.UpdateOrder) {
	if a.Updater == nil || o == nil || o.Version == a.Version || !a.startingUpdate.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer a.startingUpdate.Store(false)
		if last, err := a.loadUpdate(); err == nil && last.Attempt == o.Attempt {
			return // done once; the report says how it went
		}
		if a.Updater.Running(ctx) {
			return // one started by hand: wait for it
		}
		// Noted first: if the update fails and puts this version back, the
		// agent that comes back must not try it again.
		u := updateAttempt{Attempt: o.Attempt, Version: o.Version}
		if err := a.saveUpdate(u); err != nil {
			a.logOnce("update", err)
			return
		}
		a.Log.Info("updating jokku", "from", a.Version, "to", o.Version)
		if err := a.Updater.Start(ctx, o.Version); err != nil {
			a.Log.Error("updating jokku", "to", o.Version, "err", err)
			u.Error = err.Error()
			if err := a.saveUpdate(u); err != nil {
				a.logOnce("update", err)
			}
			a.reportSoon()
		}
	}()
}

// updateFailure says how the last update failed, or nil if it worked, is
// still running or there was none.
func (a *Agent) updateFailure() *types.UpdateFailure {
	if a.Updater == nil {
		return nil
	}
	u, err := a.loadUpdate()
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		a.logOnce("update", err)
		return nil
	case u.Version == a.Version:
		os.Remove(a.updatePath()) // it worked
		return nil
	}
	if u.Error == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		running := a.Updater.Running(ctx)
		cancel()
		if running || a.startingUpdate.Load() {
			return nil
		}
		u.Error = fmt.Sprintf("update.sh failed, and jokku is still on %s (see journalctl -u jokku-update)", a.Version)
		if err := a.saveUpdate(*u); err != nil {
			a.logOnce("update", err)
		}
	}
	return &types.UpdateFailure{Attempt: u.Attempt, Error: u.Error}
}
