package cli

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wes/jokku/internal/client"
	"github.com/wes/jokku/internal/types"
)

var (
	sizeFlag      = Flag{Name: "size", Value: "SIZE", Help: "Volume size: 512m, 10g, 1t (default 10g; disk is used as data is written)"}
	volumeProcess = Flag{Name: "process-type", Value: "TYPE", Help: "The process type whose instance mounts it (default web)"}
)

var storageCommands = []*Command{
	{Name: "storage:create", Help: "Create a volume: a disk for an app's data that survives deploys and moves with its instance", App: NeedsApp,
		Args: "<name>", MinArgs: 1, Flags: []Flag{sizeFlag, {Name: "type", Value: "TYPE", Help: "local (a disk on the node the instance runs on; the default)"}},
		Run: storageCreate},
	{Name: "storage:mount", Help: "Mount a volume in an app's instance, creating it if needed; applied with a restart", App: NeedsApp,
		Args: "<name>:<path>", MinArgs: 1, Flags: []Flag{volumeProcess, sizeFlag, noRestartFlag}, Run: storageMount},
	{Name: "storage:unmount", Help: "Unmount a volume (it keeps its data); applied with a restart", App: NeedsApp,
		Args: "<name>[:<path>]", MinArgs: 1, Flags: []Flag{volumeProcess, noRestartFlag}, Run: storageUnmount},
	{Name: "storage:list", Help: "List an app's volumes", App: NeedsApp, Run: storageList},
	{Name: "storage:report", Help: "Display volume information", App: OptionalApp, AnyFlags: true, Run: storageReport},
	{Name: "storage:resize", Help: "Grow a volume; applied with a restart", App: NeedsApp, Args: "<name> <size>", MinArgs: 2,
		Flags: []Flag{noRestartFlag}, Run: storageResize},
	{Name: "storage:move", Help: "Move a volume, and the instance using it, to another node", App: NeedsApp, Args: "<name> <node>", MinArgs: 2,
		Flags: []Flag{{Name: "detach", Help: "Return right away instead of following the move"}}, Run: storageMove},
	{Name: "storage:destroy", Help: "Delete a volume and its data", App: NeedsApp, Args: "<name>", MinArgs: 1, Flags: []Flag{forceFlag}, Run: storageDestroy},
	{Name: "storage:export", Help: "Write a volume's files to stdout as a .tar.gz (the app pauses for a moment)", App: NeedsApp, Args: "<name>", MinArgs: 1,
		Flags: []Flag{{Name: "live", Help: "Don't pause the app; files being written may be caught mid-write"}}, Run: storageExport},
	{Name: "storage:import", Help: "Restore a volume's files from a .tar or .tar.gz on stdin, then restart the process using it", App: NeedsApp, Args: "<name>", MinArgs: 1,
		Flags: []Flag{
			{Name: "clear", Help: "Delete the volume's files first, so it holds exactly the archive"},
			{Name: "keep-owners", Help: "Keep the archive's numeric owners (by default, files belong to the volume's owner)"},
		}, Run: storageImport},
	{Name: "storage:ensure-directory", Hidden: true, MaxArgs: -1, Local: true, Run: func(*Context) error {
		return errors.New("Jokku volumes are disks, not host directories: mount one with jokku storage:mount <app> <name>:<path>, which creates it")
	}},
}

func storageCreate(c *Context) error {
	req := types.CreateVolumeRequest{Name: c.Args[0], Type: c.String("type")}
	if c.Bool("size") {
		mb, err := parseSize(c.String("size"))
		if err != nil {
			return err
		}
		req.SizeMB = mb
	}
	v, err := c.API.CreateVolume(c, c.App, req)
	if err != nil {
		return err
	}
	c.Step("Created volume %s (%s, %s)", v.Name, v.Type, formatMemory(v.SizeMB))
	return nil
}

// mountArg splits "<name>:<path>".
func mountArg(arg string, needPath bool) (string, string, error) {
	name, path, ok := strings.Cut(arg, ":")
	if strings.HasPrefix(name, "/") {
		return "", "", usageErr("Jokku volumes are named disks, not host directories: use <name>:<path>, like data:%s", path)
	}
	if !ok && needPath {
		return "", "", usageErr("Expected <name>:<path>, like data:/app/storage")
	}
	return name, path, nil
}

func storageMount(c *Context) error {
	name, path, err := mountArg(c.Args[0], true)
	if err != nil {
		return err
	}
	created := false
	if _, err := c.API.Volume(c, c.App, name); client.IsNotFound(err) {
		req := types.CreateVolumeRequest{Name: name}
		if c.Bool("size") {
			if req.SizeMB, err = parseSize(c.String("size")); err != nil {
				return err
			}
		}
		if _, err := c.API.CreateVolume(c, c.App, req); err != nil {
			return err
		}
		created = true
	} else if err != nil {
		return err
	}
	m := types.VolumeMount{ProcessType: c.String("process-type"), Path: path}
	v, err := c.API.MountVolume(c, c.App, name, m)
	if err != nil {
		if created {
			c.API.DestroyVolume(c, c.App, name) // it never held data
		}
		return err
	}
	if created {
		c.Step("Created volume %s (%s, %s)", v.Name, v.Type, formatMemory(v.SizeMB))
	}
	for _, m := range v.Mounts {
		c.Step("Mounted volume %s at %s in %s", v.Name, m.Path, m.ProcessType)
	}
	if c.Bool("no-restart") {
		return nil
	}
	return c.restartIfDeployed()
}

func storageUnmount(c *Context) error {
	name, path, err := mountArg(c.Args[0], false)
	if err != nil {
		return err
	}
	if _, err := c.API.UnmountVolume(c, c.App, name, types.VolumeMount{ProcessType: c.String("process-type"), Path: path}); err != nil {
		return err
	}
	c.Step("Unmounted volume %s (its data is kept; delete it with jokku storage:destroy %s %s)", name, c.App, name)
	if c.Bool("no-restart") {
		return nil
	}
	return c.restartIfDeployed()
}

func storageList(c *Context) error {
	vols, err := c.API.Volumes(c, c.App)
	if err != nil {
		return err
	}
	c.Header("%s volumes", c.App)
	if len(vols) == 0 {
		c.Info("none (create one with jokku storage:mount %s <name>:<path>)", c.App)
		return nil
	}
	tw := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTYPE\tSIZE\tUSED\tNODE\tSTATUS\tMOUNTS")
	for _, v := range vols {
		node := v.Node
		if node == "" {
			node = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			v.Name, v.Type, formatMemory(v.SizeMB), formatMemory(v.UsedMB), node, volumeStatus(v), mounts(v))
	}
	return tw.Flush()
}

func volumeStatus(v types.Volume) string {
	if v.Move == nil {
		return v.Status
	}
	what := map[string]string{
		types.VolumeCopying:  fmt.Sprintf("copying, %s sent", formatMemory(v.Move.CopiedMB)),
		types.VolumeSynced:   "stopping the app for the final copy",
		types.VolumeReceived: "finishing",
	}[v.Move.State]
	if v.Move.Error != "" {
		what += "; retrying after: " + v.Move.Error
	}
	return fmt.Sprintf("moving to %s (%s)", v.Move.To, what)
}

func mounts(v types.Volume) string {
	if len(v.Mounts) == 0 {
		return "-"
	}
	var out []string
	for _, m := range v.Mounts {
		out = append(out, m.ProcessType+":"+m.Path)
	}
	return strings.Join(out, ", ")
}

func storageReport(c *Context) error {
	return forEachApp(c, func(a *types.App) error {
		vols, err := c.API.Volumes(c, a.Name)
		if err != nil {
			return err
		}
		var mountList, names []string
		var rows []row
		for _, v := range vols {
			names = append(names, v.Name)
			for _, m := range v.Mounts {
				mountList = append(mountList, fmt.Sprintf("%s %s:%s", m.ProcessType, v.Name, m.Path))
			}
			where := "not placed yet"
			if v.Node != "" {
				where = "on " + v.Node
			}
			rows = append(rows, row{"storage-" + v.Name, "Storage " + v.Name,
				fmt.Sprintf("%s, %s (%s used), %s, %s", v.Type, formatMemory(v.SizeMB), formatMemory(v.UsedMB), where, volumeStatus(v))})
			if v.Disk != "" {
				rows = append(rows, row{"storage-" + v.Name + "-disk", "Storage " + v.Name + " disk", v.Node + ":" + v.Disk})
			}
		}
		return c.report(a.Name+" storage information", append([]row{
			{"storage-mounts", "Storage mounts", strings.Join(mountList, ", ")},
			{"storage-volumes", "Storage volumes", strings.Join(names, " ")},
		}, rows...))
	})
}

func storageResize(c *Context) error {
	mb, err := parseSize(c.Args[1])
	if err != nil {
		return err
	}
	v, err := c.API.ResizeVolume(c, c.App, c.Args[0], mb)
	if err != nil {
		return err
	}
	c.Step("Volume %s is now %s; its instance picks that up when it restarts", v.Name, formatMemory(v.SizeMB))
	if c.Bool("no-restart") || len(v.Mounts) == 0 {
		return nil
	}
	return c.restartIfDeployed()
}

func storageMove(c *Context) error {
	name, node := c.Args[0], c.Args[1]
	v, err := c.API.MoveVolume(c, c.App, name, node)
	if err != nil {
		return err
	}
	if v.Move == nil {
		c.Step("Volume %s will be created on %s", name, node)
		return nil
	}
	c.Step("Moving volume %s from %s to %s", name, v.Node, node)
	if c.Bool("detach") {
		c.Info("Follow along with: jokku storage:list %s", c.App)
		return nil
	}
	c.Info("The app keeps running while the disk is copied, then stops briefly for the final copy")
	last := ""
	for {
		time.Sleep(time.Second)
		v, err = c.API.Volume(c, c.App, name)
		if err != nil {
			return err
		}
		if v.Move == nil {
			if v.Node != node {
				return fmt.Errorf("The move was called off; volume %s stays on %s (see jokku events %s)", name, v.Node, c.App)
			}
			c.Step("Volume %s is on %s", name, node)
			return nil
		}
		if s := v.Move.State; s != last {
			last = s
			c.Info("%s", volumeStatus(*v))
		}
	}
}

func storageDestroy(c *Context) error {
	name := c.Args[0]
	if _, err := c.API.Volume(c, c.App, name); err != nil {
		return err
	}
	if !c.Bool("force") {
		if err := c.confirm(fmt.Sprintf("delete volume %s of %s and all the data on it", name, c.App), name); err != nil {
			return err
		}
	}
	c.Step("Destroying volume %s", name)
	return c.API.DestroyVolume(c, c.App, name)
}

func storageExport(c *Context) error {
	if isTerminal(c.Stdout) {
		return fmt.Errorf("storage:export writes a .tar.gz to stdout; send it to a file: jokku storage:export %s %s > %s.tar.gz (over ssh, without -t)", c.App, c.Args[0], c.Args[0])
	}
	return c.API.ExportVolume(c, c.App, c.Args[0], c.Bool("live"), c.Stdout)
}

func storageImport(c *Context) error {
	if isTerminal(c.Stdin) {
		return fmt.Errorf("storage:import reads a .tar or .tar.gz from stdin: jokku storage:import %s %s < %s.tar.gz (over ssh, without -t)", c.App, c.Args[0], c.Args[0])
	}
	c.Step("Restoring volume %s of %s", c.Args[0], c.App)
	if err := c.API.ImportVolume(c, c.App, c.Args[0], c.Bool("clear"), c.Bool("keep-owners"), c.Stdin); err != nil {
		return err
	}
	c.Step("Restored; the process using it was restarted")
	return nil
}

func parseSize(s string) (int, error) {
	mb, err := parseMemory(s)
	if err != nil {
		return 0, usageErr("Invalid size %q, expected e.g. 512m, 10g or 1t", s)
	}
	return mb, nil
}
