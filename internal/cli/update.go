package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/wes/jokku/internal/client"
	"github.com/wes/jokku/internal/types"
	"github.com/wes/jokku/internal/update"
	"github.com/wes/jokku/internal/version"
)

var updateCommand = &Command{
	Name: "update", Help: "Update jokku on this server to the latest release, then the cluster's other nodes (asks first)",
	Local: true, serverOnly: true, Run: runUpdate,
	Flags: []Flag{
		{Name: "yes", Short: "y", Help: "Don't ask for confirmation"},
		{Name: "version", Value: "VERSION", Help: "Update to a specific release, e.g. v0.0.3"},
		{Name: "this-node", Help: "Update only this server, not the cluster's other nodes"},
	},
}

// runUpdate checks for a newer release, asks, then runs that release's
// update.sh, which backs up, swaps the binary, runs "jokku setup" and rolls
// back on failure. One update path, whichever way it is started. On the
// control node, the other nodes follow, one at a time.
func runUpdate(c *Context) error {
	if os.Geteuid() != 0 {
		return errors.New("updating needs root: sudo jokku update")
	}
	target := c.String("version")
	if target == "" {
		var err error
		if target, err = update.Latest(c); err != nil {
			return fmt.Errorf("checking for a new release: %w", err)
		}
	}
	current := version.Version
	// Only the control node answers for the cluster; a worker's daemon
	// refuses, and then this server is all there is to update.
	api := client.New(localTarget(), os.Getenv("JOKKU_ACTOR"))
	var others []types.NodeView
	if !c.Bool("this-node") {
		if st, err := api.ClusterStatus(c); err == nil {
			for _, n := range st.Nodes {
				if n.Name != st.Control {
					others = append(others, n)
				}
			}
		}
	}
	if target == current {
		c.Step("Jokku is up to date (%s)", current)
		for _, n := range others {
			if version.Older(n.Version, current) {
				return updateNodes(c, api)
			}
		}
		return nil
	}
	c.Step("Jokku %s is available (this server runs %s)", target, current)
	c.Info("Release notes: https://github.com/%s/releases/tag/%s", version.Repo, target)
	if len(others) > 0 {
		c.Info("Then the cluster's other nodes update, one at a time: %s", nodeNames(others))
	}
	if !c.Bool("yes") {
		ok, err := c.ask(fmt.Sprintf("Update jokku from %s to %s? [y/N] ", current, target))
		if err != nil {
			return err
		}
		if !ok {
			c.Step("Update cancelled")
			return nil
		}
	}

	script, err := update.Script(c, target)
	if err != nil {
		return fmt.Errorf("downloading the %s updater: %w", target, err)
	}
	cmd := exec.CommandContext(c, "sh", "-s")
	cmd.Stdin = strings.NewReader(string(script))
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	cmd.Env = append(os.Environ(), "JOKKU_VERSION="+target)
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return &exitError{code: exit.ExitCode()} // update.sh already explained
		}
		return err
	}
	if len(others) == 0 {
		return nil
	}
	fmt.Fprintln(c.Stdout)
	return updateNodes(c, api)
}

// localTarget is this server's own daemon.
func localTarget() client.Target {
	if s := os.Getenv("JOKKU_SOCKET"); s != "" {
		return client.Target{Socket: s}
	}
	return client.Target{Socket: client.DefaultSocket}
}

func nodeNames(nodes []types.NodeView) string {
	var names []string
	for i, n := range nodes {
		if i == 5 {
			names = append(names, fmt.Sprintf("and %d more", len(nodes)-i))
			break
		}
		names = append(names, n.Name)
	}
	return strings.Join(names, ", ")
}

// updatePoll is how often updateNodes checks on the nodes. (A variable so
// tests can shorten it.)
var updatePoll = 2 * time.Second

// updateNodes asks the control node to bring the other nodes to its version,
// then follows along until each is updated, has failed, or has to wait.
func updateNodes(c *Context, api *client.Client) error {
	// The daemon may still be starting after its own update.
	var err error
	for deadline := time.Now().Add(30 * time.Second); ; {
		if err = api.UpdateNodes(c); err == nil || time.Now().After(deadline) || c.Err() != nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return fmt.Errorf("updating the other nodes: %w", err)
	}
	c.Step("Updating the cluster's other nodes, one at a time")
	c.Info("Ctrl-C stops watching, not the update; jokku nodes:list shows how it goes")
	shown := map[string]string{} // node -> the state last shown
	started := map[string]time.Time{}
	for {
		st, err := api.ClusterStatus(c)
		if err != nil {
			return err
		}
		busy := false
		for _, n := range st.Nodes {
			if n.Name == st.Control {
				continue
			}
			state := ""
			if n.Update != nil {
				state = n.Update.State
				if state == types.UpdateWaiting && n.Status == "down" {
					state = "down"
				}
			}
			busy = busy || state == types.UpdateRunning || state == types.UpdateWaiting
			if state == shown[n.Name] {
				continue
			}
			was := shown[n.Name]
			shown[n.Name] = state
			switch state {
			case types.UpdateRunning:
				started[n.Name] = time.Now()
				c.Info("%s: updating from %s to %s", n.Name, n.Version, n.Update.To)
			case types.UpdateFailed:
				c.Warn("%s: the update failed: %s", n.Name, n.Update.Error)
			case types.UpdateByHand:
				c.Warn("%s runs %s, which can't update itself: run sudo jokku update on %s", n.Name, n.Version, n.Name)
			case "down":
				c.Info("%s is down: it updates when it is back", n.Name)
			case "":
				if was == types.UpdateRunning || was == types.UpdateWaiting {
					took := ""
					if t, ok := started[n.Name]; ok {
						took = fmt.Sprintf(" (%s)", time.Since(t).Round(time.Second))
					}
					c.Info("%s: updated to %s%s", n.Name, n.Version, took)
				}
			}
		}
		if !busy {
			return updateSummary(c, st)
		}
		select {
		case <-c.Done():
			return c.Err()
		case <-time.After(updatePoll):
		}
	}
}

// updateSummary says where the cluster's update ended up.
func updateSummary(c *Context, st *types.ClusterStatus) error {
	behind := 0
	for _, n := range st.Nodes {
		if n.Update != nil {
			behind++
		}
		if n.Update != nil && n.Update.State == types.UpdateFailed {
			c.Warn("The update stopped at %s; the nodes after it wait. Once it is fixed, run sudo jokku update again to go on.", n.Name)
			return &exitError{code: 1}
		}
	}
	if behind == 0 {
		fmt.Fprintf(c.Stdout, "\n=====> All %d nodes run %s\n", len(st.Nodes), st.Version)
	} else {
		fmt.Fprintf(c.Stdout, "\n=====> %d of %d nodes run %s\n", len(st.Nodes)-behind, len(st.Nodes), st.Version)
	}
	return nil
}

// ask prints a yes/no question and reads the answer from the terminal.
func (c *Context) ask(question string) (bool, error) {
	f, ok := c.Stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return false, fmt.Errorf("%s needs confirmation; pass --yes to run it non-interactively", c.Cmd.Name)
	}
	fmt.Fprint(c.Stdout, question)
	answer, _ := bufio.NewReader(c.Stdin).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}
