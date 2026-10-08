package cli

import (
	"fmt"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/wes/jokku/internal/tui"
	"github.com/wes/jokku/internal/types"
)

var nodesCommands = []*Command{
	{Name: "nodes:list", Help: "List the machines in the cluster", Run: nodesList},
	{Name: "nodes:report", Help: "Display node information", Args: "[<node>]", MaxArgs: 1, AnyFlags: true, Run: nodesReport},
	{Name: "nodes:set", Help: "Change a node setting: schedulable or ingress", Args: "<node> <schedulable|ingress> <true|false>", MinArgs: 3, Run: nodesSet},
	{Name: "nodes:drain", Help: "Move a node's instances to other nodes and stop scheduling onto it", Args: "<node>", MinArgs: 1, Run: nodesDrain(true)},
	{Name: "nodes:undrain", Help: "Make a drained node accept instances again", Args: "<node>", MinArgs: 1, Run: nodesDrain(false)},
	{Name: "nodes:remove", Help: "Remove a node from the cluster (drain it first)", Args: "<node>", MinArgs: 1,
		Flags: []Flag{{Name: "force", Help: "Remove it even if instances still run there; they are started elsewhere"}}, Run: nodesRemove},
}

var clusterCommands = []*Command{
	{Name: "cluster:join-command", Help: "Print the command that adds a server to this cluster", Flags: []Flag{
		{Name: "ttl", Value: "DURATION", Help: "How long the command works (default 1h)"},
		{Name: "reusable", Help: "Let it add any number of servers until it expires (for autoscaling)"},
	}, Run: clusterJoinCommand},
	{Name: "cluster:report", Help: "Display cluster information", AnyFlags: true, Run: clusterReport},
	{Name: "events", Help: "List recent events: deploys, crashes, nodes", App: OptionalApp,
		Flags: []Flag{{Name: "num", Short: "n", Value: "N", Help: "How many (default 30)"}}, Run: events},
}

func nodesList(c *Context) error {
	st, err := c.API.ClusterStatus(c)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tROLE\tSTATUS\tADDRESS\tMESH IP\tCPUS\tMEMORY\tINSTANCES\tVERSION")
	for _, n := range st.Nodes {
		status := n.Status
		if n.CanRun != "" && status == "ready" {
			status = "ready (no microVMs)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%d\t%s\n",
			n.Name, n.Role, status, n.Address, n.MeshIP, n.CPUs, formatMemory(n.MemoryMB), n.Instances, n.Version)
	}
	return tw.Flush()
}

func nodesReport(c *Context) error {
	st, err := c.API.ClusterStatus(c)
	if err != nil {
		return err
	}
	found := false
	for _, n := range st.Nodes {
		if len(c.Args) == 1 && n.Name != c.Args[0] {
			continue
		}
		found = true
		m := n.Metrics
		canRun := "yes"
		if n.CanRun != "" {
			canRun = "no: " + n.CanRun
		}
		err := c.report(n.Name+" node information", []row{
			{"node-role", "Node role", n.Role},
			{"node-status", "Node status", n.Status},
			{"node-version", "Node version", n.Version},
			{"node-address", "Node address", n.Address},
			{"node-mesh-ip", "Node mesh ip", n.MeshIP},
			{"node-subnet", "Node subnet", n.Subnet},
			{"node-arch", "Node arch", n.Arch},
			{"node-runs-microvms", "Node runs microVMs", canRun},
			{"node-cpus", "Node cpus", fmt.Sprintf("%d (%.0f%% busy, load %.2f)", n.CPUs, m.CPUPercent, m.Load1)},
			{"node-memory", "Node memory", fmt.Sprintf("%s (%s used, %s for instances)", formatMemory(n.MemoryMB), formatMemory(m.MemoryUsedMB), formatMemory(n.AllocatedMB))},
			{"node-disk", "Node disk", fmt.Sprintf("%s free of %s", formatMemory(m.DiskFreeMB), formatMemory(m.DiskMB))},
			{"node-instances", "Node instances", strconv.Itoa(n.Instances)},
			{"node-schedulable", "Node schedulable", yesNo(n.Schedulable)},
			{"node-ingress", "Node ingress", yesNo(n.Ingress)},
			{"node-last-seen", "Node last seen", ago(n.LastSeen)},
		})
		if err != nil {
			return err
		}
	}
	if !found && len(c.Args) == 1 {
		return fmt.Errorf("Node %s does not exist", c.Args[0])
	}
	return nil
}

func nodesSet(c *Context) error {
	name, key, value := c.Args[0], c.Args[1], c.Args[2]
	if key != "schedulable" && key != "ingress" {
		return usageErr("Unknown node setting %q, expected schedulable or ingress", key)
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		return usageErr("Value must be true or false")
	}
	if _, err := c.API.SetNodeFlag(c, name, key, b); err != nil {
		return err
	}
	c.Step("Set %s %s to %s", name, key, yesNo(b))
	return nil
}

func nodesDrain(drain bool) func(*Context) error {
	return func(c *Context) error {
		if _, err := c.API.SetNodeFlag(c, c.Args[0], "draining", drain); err != nil {
			return err
		}
		if drain {
			c.Step("Draining %s: its instances are moving to other nodes", c.Args[0])
			c.Info("Follow along with: jokku top (or jokku events)")
		} else {
			c.Step("%s is accepting instances again", c.Args[0])
		}
		return nil
	}
}

func nodesRemove(c *Context) error {
	if err := c.API.RemoveNode(c, c.Args[0], c.Bool("force")); err != nil {
		return err
	}
	c.Step("Removed %s from the cluster", c.Args[0])
	c.Info("Its jokku service stops its VMs and goes idle; uninstall it or reuse the server.")
	return nil
}

func clusterJoinCommand(c *Context) error {
	req := types.CreateJoinTokenRequest{Reusable: c.Bool("reusable")}
	if c.Bool("ttl") {
		d, err := time.ParseDuration(c.String("ttl"))
		if err != nil || d <= 0 {
			return usageErr("Invalid --ttl %q, expected e.g. 30m or 24h", c.String("ttl"))
		}
		req.TTLSeconds = int(d.Seconds())
	}
	tok, err := c.API.CreateJoinToken(c, req)
	if err != nil {
		return err
	}
	uses := "once"
	if req.Reusable {
		uses = "any number of times"
	}
	c.Header("Run this on the new server (works %s, until %s):", uses, tok.ExpiresAt.Local().Format("15:04 MST"))
	fmt.Fprintln(c.Stdout, tok.Command)
	return nil
}

func clusterReport(c *Context) error {
	st, err := c.API.ClusterStatus(c)
	if err != nil {
		return err
	}
	var cpus, mem, alloc int
	for _, n := range st.Nodes {
		cpus += n.CPUs
		mem += n.MemoryMB
		alloc += n.AllocatedMB
	}
	return c.report("Cluster information", []row{
		{"cluster-control", "Cluster control node", st.Control},
		{"cluster-version", "Cluster version", st.Version},
		{"cluster-nodes", "Cluster nodes", fmt.Sprintf("%d (%d ready)", st.Totals.Nodes, st.Totals.NodesReady)},
		{"cluster-cpus", "Cluster cpus", strconv.Itoa(cpus)},
		{"cluster-memory", "Cluster memory", fmt.Sprintf("%s (%s for instances)", formatMemory(mem), formatMemory(alloc))},
		{"cluster-apps", "Cluster apps", strconv.Itoa(st.Totals.Apps)},
		{"cluster-instances", "Cluster instances", fmt.Sprintf("%d (%d healthy)", st.Totals.Instances, st.Totals.InstancesHealthy)},
	})
}

func events(c *Context) error {
	n := 30
	if c.Bool("num") {
		var err error
		if n, err = strconv.Atoi(c.String("num")); err != nil || n <= 0 {
			return usageErr("Invalid --num %q", c.String("num"))
		}
	}
	evs, err := c.API.Events(c, c.App, n)
	if err != nil {
		return err
	}
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		who := e.App
		if who == "" {
			who = e.Node
		}
		fmt.Fprintf(c.Stdout, "%s  %-8s %-14s %s\n", e.At.Local().Format("2006-01-02 15:04:05"), e.Kind, who, e.Message)
	}
	return nil
}

// ago renders a time as "12s ago".
func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Local().Format("2006-01-02")
}

var topCommand = &Command{
	Name: "top", Help: "Watch the cluster, apps and machines live (over ssh: ssh -t jokku@server top)",
	Run: func(c *Context) error { return tui.Run(c, c.API, c.Stdin, c.Stdout) },
}
