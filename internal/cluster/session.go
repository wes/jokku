package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wes/jokku/internal/session"
	"github.com/wes/jokku/internal/store"
)

// OpenSession opens a session with an instance's guest agent, through its
// node's agent (jokku enter, volume copies). The caller sends the request
// first.
func (c *Controller) OpenSession(ctx context.Context, in store.Instance) (io.ReadWriteCloser, error) {
	node, err := c.Store.Node(ctx, in.Node)
	if err != nil {
		return nil, err
	}
	if !node.Ready(time.Now()) {
		return nil, fmt.Errorf("%s runs on %s, which is down", in.Name(), in.Node)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+c.agentAddr(*node)+"/v1/instances/"+in.ID+"/session", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+node.AgentToken)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", session.Upgrade)
	resp, err := agentClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reaching %s: %w", in.Node, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("%s on %s: %s", in.Name(), in.Node, strings.TrimSpace(string(b)))
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, errors.New("the node did not open the session")
	}
	return rwc, nil
}
