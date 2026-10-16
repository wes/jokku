package cli

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wes/jokku/internal/nettest"
	"github.com/wes/jokku/internal/types"
)

var networkCommands = []*Command{
	{Name: "network:test", Help: "Test the links between the nodes: each carries as much as it can for a few seconds, in turn, and is pinged",
		Flags: []Flag{{Name: "seconds", Value: "N", Help: "How long each link is loaded (default 3)"}}, Run: networkTest},
	{Name: "network:report", Help: "Show the latest network test, and how it compares with the one before", Run: networkReport},
}

func init() {
	namespaceHelp["network"] = "Test the network between the nodes"
}

func networkTest(c *Context) error {
	var req types.NetTestRequest
	if s := c.String("seconds"); s != "" {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return usageErr("--seconds takes a number, like 3")
		}
		req.Seconds = v
	}
	p, err := c.API.StartNetTest(c, req)
	if err != nil {
		return err
	}
	c.Step("Testing %d links between %s", p.Total, strings.Join(p.Test.Nodes, ", "))
	for _, name := range sortedKeys(p.Test.Skipped) {
		c.Info("left out %s: %s", name, p.Test.Skipped[name])
	}
	shown := 0
	for {
		time.Sleep(time.Second)
		nt, err := c.API.NetTests(c)
		if err != nil {
			return err
		}
		var links []types.NetLink
		if nt.Running != nil {
			links = nt.Running.Test.Links
		} else if len(nt.Tests) > 0 && nt.Tests[0].Started.Equal(p.Started) {
			links = nt.Tests[0].Links
		}
		for _, l := range links[min(shown, len(links)):] {
			c.Info("%s", linkLine(l))
		}
		shown = max(shown, len(links))
		if nt.Running == nil {
			fmt.Fprintln(c.Stdout)
			return printNetTests(c, nt)
		}
	}
}

func linkLine(l types.NetLink) string {
	s := fmt.Sprintf("%-28s %6.0f Mbit/s  %6.2f ms", l.From+" → "+l.To, l.Mbps, l.Ping.Avg)
	if lost := l.Ping.Sent - l.Ping.Received; lost > 0 {
		s += fmt.Sprintf("  %d of %d pings lost", lost, l.Ping.Sent)
	}
	if l.Error != "" {
		s += "  " + l.Error
	}
	return s
}

func networkReport(c *Context) error {
	nt, err := c.API.NetTests(c)
	if err != nil {
		return err
	}
	if nt.Running != nil {
		c.Info("A test is running: %d of %d links done", nt.Running.Done, nt.Running.Total)
	}
	return printNetTests(c, nt)
}

// printNetTests prints the latest finished test, compared with the one
// before.
func printNetTests(c *Context, nt *types.NetTests) error {
	if len(nt.Tests) == 0 {
		c.Info("No network test yet: jokku network:test")
		return nil
	}
	t := nt.Tests[0]
	r := nettest.Read(&t)
	c.Header("Network test %s (%s; %d nodes, %d links over the mesh, %gs each)", ago(t.Finished),
		t.Finished.Sub(t.Started).Round(time.Second), len(t.Nodes), len(t.Links), t.Seconds)
	if r.Mbps > 0 {
		c.Info("A link here usually carries %.0f Mbit/s and answers in %.2f ms.", r.Mbps, r.RTT)
	}
	alike := true
	for _, n := range r.Nodes {
		if n.Grade != nettest.Good {
			c.Info("%s %s.", n.Name, strings.Join(n.Problems, "; "))
			alike = false
		}
	}
	if alike {
		c.Info("Every node does about the same.")
	}
	for _, name := range sortedKeys(t.Skipped) {
		c.Info("Left out %s: %s.", name, t.Skipped[name])
	}

	links := map[string]types.NetLink{}
	for _, l := range t.Links {
		links[l.From+">"+l.To] = l
	}
	matrix := func(title string, cell func(types.NetLink) string) {
		fmt.Fprintln(c.Stdout)
		tw := tabwriter.NewWriter(c.Stdout, 0, 4, 3, ' ', tabwriter.AlignRight)
		fmt.Fprintf(tw, "%s\t%s\t\n", title, strings.Join(t.Nodes, "\t"))
		for _, from := range t.Nodes {
			cells := []string{from}
			for _, to := range t.Nodes {
				l, ok := links[from+">"+to]
				switch {
				case from == to:
					cells = append(cells, "·")
				case !ok:
					cells = append(cells, "-")
				default:
					cells = append(cells, cell(l))
				}
			}
			fmt.Fprintf(tw, "%s\t\n", strings.Join(cells, "\t"))
		}
		tw.Flush()
	}
	matrix("Mbit/s from \\ to", func(l types.NetLink) string {
		if l.Mbps == 0 {
			return "failed"
		}
		return fmt.Sprintf("%.0f", l.Mbps) + gradeMark(r.Grade(l))
	})
	matrix("ms from \\ to", func(l types.NetLink) string {
		if l.Ping.Received == 0 {
			return "no answer"
		}
		s := fmt.Sprintf("%.2f", l.Ping.Avg)
		if lost := l.Ping.Sent - l.Ping.Received; lost > 0 {
			s += fmt.Sprintf(" (%d lost)", lost)
		}
		return s
	})

	var prev map[string]nettest.Node
	if len(nt.Tests) > 1 {
		prev = map[string]nettest.Node{}
		for _, n := range nettest.Read(&nt.Tests[1]).Nodes {
			prev[n.Name] = n
		}
	}
	fmt.Fprintln(c.Stdout)
	tw := tabwriter.NewWriter(c.Stdout, 0, 4, 3, ' ', 0)
	head := "NODE\tSENDS\tRECEIVES\tROUND TRIP\tPINGS LOST"
	if prev != nil {
		head += "\tVS. THE TEST " + strings.ToUpper(ago(nt.Tests[1].Finished))
	}
	fmt.Fprintln(tw, head)
	for _, n := range r.Nodes {
		line := fmt.Sprintf("%s\t%.0f Mbit/s\t%.0f Mbit/s\t%.2f ms\t%d of %d", n.Name, n.Out, n.In, n.RTT, n.Lost, n.Sent)
		if prev != nil {
			if p, ok := prev[n.Name]; ok && p.Out+p.In > 0 && n.Out+n.In > 0 {
				line += fmt.Sprintf("\t%+.0f%% speed, %+.2f ms", (n.Out+n.In-p.Out-p.In)/(p.Out+p.In)*100, n.RTT-p.RTT)
			} else {
				line += "\t-"
			}
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()

	var failed []string
	for _, l := range t.Links {
		if l.Error != "" {
			failed = append(failed, fmt.Sprintf("%s → %s: %s", l.From, l.To, l.Error))
		}
	}
	if len(failed) > 0 {
		fmt.Fprintln(c.Stdout)
		c.Header("Problems")
		for _, f := range failed {
			c.Info("%s", f)
		}
	}
	return nil
}

// gradeMark marks a link that stands out, for terminals without color.
func gradeMark(g nettest.Grade) string {
	switch g {
	case nettest.Bad:
		return " !!"
	case nettest.Warn:
		return " !"
	}
	return ""
}
