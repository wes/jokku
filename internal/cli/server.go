package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/wes/jokku/internal/client"
	"github.com/wes/jokku/internal/daemon"
	"github.com/wes/jokku/internal/setup"
	"github.com/wes/jokku/internal/types"
	"github.com/wes/jokku/internal/version"
)

// Commands that run on the server itself, invoked by systemd, sshd or git.
var serverCommands = []*Command{
	{Name: "daemon", Help: "Run the jokku server (started by systemd)", Local: true, serverOnly: true, Flags: []Flag{
		{Name: "data-dir", Value: "DIR", Help: "State directory (default /var/lib/jokku)"},
		{Name: "socket", Value: "PATH", Help: "API socket (default " + client.DefaultSocket + ")"},
		{Name: "git-dir", Value: "DIR", Help: "Bare git repos (default <data-dir>/git)"},
		{Name: "git-user", Value: "USER", Help: "Account sshd runs git pushes as (default jokku; empty for the current user)"},
		{Name: "authorized-keys", Value: "PATH", Help: "authorized_keys to manage (default ~<git-user>/.ssh/authorized_keys; - to disable)"},
		{Name: "cluster-cidr", Value: "CIDR", Help: "Cluster network, an IPv4 /16 (default 10.210.0.0/16)"},
		{Name: "node-name", Value: "NAME", Help: "This node's name (default the hostname)"},
	}, Run: runDaemon},
	{Name: "setup", Help: "Prepare this server for jokku and (re)start it; run by install.sh and update.sh", Local: true, serverOnly: true,
		Run: func(c *Context) error { return setup.Run(c, c.Stdout) }},
	{Name: "ssh-command", Hidden: true, Local: true, serverOnly: true,
		Flags: []Flag{{Name: "key-name", Value: "NAME", Help: "Name of the SSH key that authenticated"}}, Run: runSSHCommand},
	{Name: "git-hook", Hidden: true, Local: true, serverOnly: true, Args: "<app>", MinArgs: 1, Run: runGitHook},
	{Name: "api:dial-stdio", Hidden: true, Local: true, Run: runDialStdio},
}

func serverSocket() string {
	if s := os.Getenv("JOKKU_SOCKET"); s != "" {
		return s
	}
	return client.DefaultSocket
}

func runDaemon(c *Context) error {
	cfg := daemon.Config{
		DataDir:        daemon.DefaultDataDir,
		Socket:         client.DefaultSocket,
		GitUser:        daemon.DefaultUser,
		ClusterCIDR:    daemon.DefaultClusterCIDR,
		GitDir:         c.String("git-dir"),
		AuthorizedKeys: c.String("authorized-keys"),
		NodeName:       c.String("node-name"),
	}
	for flag, dst := range map[string]*string{"data-dir": &cfg.DataDir, "socket": &cfg.Socket, "cluster-cidr": &cfg.ClusterCIDR} {
		if c.Bool(flag) {
			*dst = c.String(flag)
		}
	}
	if c.Bool("git-user") {
		cfg.GitUser = c.String("git-user")
	}
	ctx, stop := signal.NotifyContext(c, os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return daemon.Run(ctx, cfg, log)
}

// runSSHCommand is the forced command for every key in the jokku user's
// authorized_keys. It dispatches on SSH_ORIGINAL_COMMAND: git transport, the
// remote CLI's API tunnel, or a CLI command to run here.
func runSSHCommand(c *Context) error {
	keyName := c.String("key-name")
	os.Setenv("JOKKU_ACTOR", keyName)
	args, err := splitWords(os.Getenv("SSH_ORIGINAL_COMMAND"))
	if err != nil {
		return err
	}
	if len(args) > 0 && path.Base(args[0]) == "jokku" {
		args = args[1:]
	}
	if len(args) == 0 {
		args = []string{"help"}
	}

	switch args[0] {
	case "git-receive-pack", "git-upload-pack":
		if len(args) != 2 {
			return fmt.Errorf("%s expects exactly one repository argument", args[0])
		}
		return gitTransport(c, keyName, args[0], args[1])
	case "api:dial-stdio":
		return runDialStdio(c)
	}
	if cmd, ok := commands[args[0]]; ok && cmd.serverOnly {
		return fmt.Errorf("%s cannot be run over ssh", args[0])
	}
	if code := Main(c, args, c.IO); code != 0 {
		return &exitError{code: code}
	}
	return nil
}

// gitTransport serves "git push jokku@host:app" and "git clone jokku@host:app".
// The git protocol owns stdout, so every message goes to stderr.
func gitTransport(c *Context, keyName, service, repo string) error {
	app := strings.TrimSuffix(strings.Trim(repo, "/"), ".git")
	api := client.New(client.Target{Socket: serverSocket()}, keyName)
	if _, err := api.App(c, app); client.IsNotFound(err) && service == "git-receive-pack" {
		// Pushing to a new app creates it, as in Dokku.
		fmt.Fprintf(c.Stderr, "%sCreating app %s\n", stepPrefix, app)
		if _, err := api.CreateApp(c, app); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	repoPath, err := api.EnsureGitRepo(c, app)
	if err != nil {
		return err
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	sub := strings.TrimPrefix(service, "git-") // receive-pack | upload-pack
	return syscall.Exec(git, []string{"git", sub, repoPath}, os.Environ())
}

// runGitHook is the pre-receive hook of every app repo. For a push to the
// deploy branch it archives the pushed commit and streams the deploy back to
// the pusher; a failed deploy rejects the push.
func runGitHook(c *Context) error {
	app := c.Args[0]
	api := client.New(client.Target{Socket: serverSocket()}, os.Getenv("JOKKU_ACTOR"))
	git, err := api.Properties(c, app, "git")
	if err != nil {
		return err
	}
	branch := git.Computed["deploy-branch"]

	sc := bufio.NewScanner(c.Stdin)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 3 {
			continue
		}
		rev, ref := fields[1], fields[2]
		if strings.Trim(rev, "0") == "" {
			continue // branch deletion
		}
		if ref != "refs/heads/"+branch {
			pushed := strings.TrimPrefix(ref, "refs/heads/")
			fmt.Fprintf(c.Stdout, "%sPushed %s, but %s deploys from %s. Nothing was deployed.\n", errorPrefix, pushed, app, branch)
			fmt.Fprintf(c.Stdout, "%sDeploy with: git push jokku <branch>:%s (or: jokku git:set %s deploy-branch %s)\n",
				errorPrefix, branch, app, pushed)
			continue
		}
		if err := deployRev(c, api, app, rev); err != nil {
			return err
		}
	}
	return sc.Err()
}

func deployRev(c *Context, api *client.Client, app, rev string) error {
	archive := exec.CommandContext(c, "git", "archive", "--format=tar", rev)
	archive.Stderr = c.Stderr
	tarball, err := archive.StdoutPipe()
	if err != nil {
		return err
	}
	if err := archive.Start(); err != nil {
		return err
	}
	err = api.Deploy(c, app, "git", rev, tarball, func(e types.Event) {
		fmt.Fprintln(c.Stdout, e.Message)
	})
	io.Copy(io.Discard, tarball) // let git archive finish if the deploy bailed early
	if werr := archive.Wait(); err == nil && werr != nil {
		err = fmt.Errorf("git archive %s: %w", rev, werr)
	}
	return err
}

// runDialStdio pipes stdin/stdout to the local API socket. The remote CLI
// runs it over ssh to talk HTTP to the API.
func runDialStdio(c *Context) error {
	conn, err := net.Dial("unix", serverSocket())
	if err != nil {
		return fmt.Errorf("cannot reach the jokku daemon: %w", err)
	}
	defer conn.Close()
	go func() {
		io.Copy(conn, c.Stdin)
		if uc, ok := conn.(*net.UnixConn); ok {
			uc.CloseWrite()
		}
	}()
	_, err = io.Copy(c.Stdout, conn)
	return err
}

func runVersion(c *Context) error {
	fmt.Fprintf(c.Stdout, "jokku version %s (%s)\n", version.Version, version.Commit)
	target, _, err := resolveTarget()
	if err != nil {
		return nil // no server configured; the client version is all there is
	}
	ctx, cancel := context.WithTimeout(c, 10*time.Second)
	defer cancel()
	v, err := client.New(target, "").Version(ctx)
	if err != nil {
		c.Warn("%v", err)
		return nil
	}
	fmt.Fprintf(c.Stdout, "server version %s (%s) at %s\n", v.Version, v.Commit, target)
	return nil
}

// splitWords splits a command line the way a POSIX shell would for simple
// cases: whitespace-separated words, single quotes, double quotes and
// backslash escapes. sshd hands forced commands the raw string, so this is
// what turns `config:set app "A=b c"` back into arguments.
func splitWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("unterminated single quote in command")
			}
			cur.WriteString(s[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case ch == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte(`"\$`+"`", s[i+1]) >= 0 {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errors.New("unterminated double quote in command")
			}
			inWord = true
		case ch == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case ch == ' ' || ch == '\t' || ch == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(ch)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}
