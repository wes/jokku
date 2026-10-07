package cli

import (
	"fmt"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/wes/jokku/internal/types"
)

var nodesCommands = []*Command{
	{Name: "nodes:list", Help: "List the machines in the cluster", Run: nodesList},
	{Name: "nodes:report", Help: "Display node information", Args: "[<node>]", MaxArgs: 1, AnyFlags: true, Run: nodesReport},
	{Name: "nodes:set", Help: "Change a node setting: schedulable or ingress", Args: "<node> <schedulable|ingress> <true|false>", MinArgs: 3, Run: nodesSet},
	{Name: "nodes:drain", Help: "Stop scheduling onto a node and move its instances elsewhere", Args: "<node>", MinArgs: 1, Run: nodesDrain(true)},
	{Name: "nodes:undrain", Help: "Make a drained node schedulable again", Args: "<node>", MinArgs: 1, Run: nodesDrain(false)},
}

func nodesList(c *Context) error {
	nodes, err := c.API.Nodes(c)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tROLE\tSTATUS\tMESH IP\tADDRESS\tCPUS\tMEMORY\tSCHEDULABLE\tINGRESS")
	for _, n := range nodes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
			n.Name, n.Role, n.Status, n.MeshIP, n.Address, n.CPUs, formatMemory(n.MemoryMB), yesNo(n.Schedulable), yesNo(n.Ingress))
	}
	return tw.Flush()
}

func nodesReport(c *Context) error {
	var nodes []types.Node
	if len(c.Args) == 1 {
		n, err := c.API.Node(c, c.Args[0])
		if err != nil {
			return err
		}
		nodes = []types.Node{*n}
	} else {
		var err error
		if nodes, err = c.API.Nodes(c); err != nil {
			return err
		}
	}
	for _, n := range nodes {
		err := c.report(n.Name+" node information", []row{
			{"node-role", "Node role", n.Role},
			{"node-status", "Node status", n.Status},
			{"node-address", "Node address", n.Address},
			{"node-mesh-ip", "Node mesh ip", n.MeshIP},
			{"node-subnet", "Node subnet", n.Subnet},
			{"node-arch", "Node arch", n.Arch},
			{"node-cpus", "Node cpus", strconv.Itoa(n.CPUs)},
			{"node-memory", "Node memory", formatMemory(n.MemoryMB)},
			{"node-schedulable", "Node schedulable", yesNo(n.Schedulable)},
			{"node-ingress", "Node ingress", yesNo(n.Ingress)},
			{"node-last-seen", "Node last seen", n.LastSeen.Local().Format(time.RFC3339)},
		})
		if err != nil {
			return err
		}
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
			c.Step("Draining %s", c.Args[0])
		} else {
			c.Step("%s is accepting instances again", c.Args[0])
		}
		return nil
	}
}
