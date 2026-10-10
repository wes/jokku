package vm

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"time"
)

// NoVMs is the runtime of a node that runs no microVMs: an edge, which only
// routes traffic. Its one job is the node's mesh address, which sits on a
// dummy interface (where a VM host has its bridge) as the source of its
// traffic over the mesh.
type NoVMs struct {
	Reason  string       // why apps don't run here
	Device  string       // e.g. jokku0
	Address netip.Prefix // the mesh address, in its subnet
}

func (n *NoVMs) Available() error { return errors.New(n.Reason) }

func (n *NoVMs) EnsureNetwork(ctx context.Context) error {
	if run(ctx, "ip", "link", "show", n.Device) != nil {
		if err := run(ctx, "ip", "link", "add", n.Device, "type", "dummy"); err != nil {
			return err
		}
	}
	if err := run(ctx, "ip", "addr", "replace", n.Address.String(), "dev", n.Device); err != nil {
		return err
	}
	return run(ctx, "ip", "link", "set", n.Device, "up")
}

func (n *NoVMs) err() error { return errors.New("this node runs no microVMs: " + n.Reason) }

func (n *NoVMs) Start(context.Context, Spec) error                 { return n.err() }
func (n *NoVMs) Stop(context.Context, string, time.Duration) error { return nil }
func (n *NoVMs) Remove(context.Context, string) error              { return nil }
func (n *NoVMs) Units(context.Context) (map[string]string, error)  { return map[string]string{}, nil }
func (n *NoVMs) Usage(context.Context, []string) (map[string]Usage, error) {
	return map[string]Usage{}, nil
}
func (n *NoVMs) CheckTCP(string) bool                              { return false }
func (n *NoVMs) CreateVolume(context.Context, string, int) error   { return n.err() }
func (n *NoVMs) GrowVolume(context.Context, string, int) error     { return n.err() }
func (n *NoVMs) Attached(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }

func (n *NoVMs) Session(context.Context, string) (io.ReadWriteCloser, string, error) {
	return nil, "", n.err()
}
