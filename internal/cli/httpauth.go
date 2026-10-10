package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
	"rsc.io/qr"

	"github.com/wes/jokku/internal/types"
)

var httpAuthCommands = []*Command{
	{Name: "http-auth:enable", Help: "Put a login in front of an app: the cluster's users (all, or those listed), or with --password one shared password",
		App: NeedsApp, Args: "[<user>...]", MaxArgs: -1, Flags: []Flag{
			{Name: "password", Help: "Ask for one password (or PIN) instead of users; it's read from stdin without a terminal"},
		}, Run: httpAuthEnable},
	{Name: "http-auth:disable", Help: "Remove the login in front of an app", App: NeedsApp, Run: httpAuthDisable},
	{Name: "http-auth:set-password", Help: "Change an app's shared password", App: NeedsApp, Run: httpAuthSetPassword},
	{Name: "http-auth:add-allowed-ip", Help: "Let an address (or CIDR) in without logging in", App: NeedsApp, Args: "<ip|cidr>", MinArgs: 1,
		Run: httpAuthList("allowed IPs", func(s *types.AuthSettings) *[]string { return &s.AllowIPs }, func(p *types.AuthPatch, v *[]string) { p.AllowIPs = v }, true)},
	{Name: "http-auth:remove-allowed-ip", Help: "Stop letting an address in without logging in", App: NeedsApp, Args: "<ip|cidr>", MinArgs: 1,
		Run: httpAuthList("allowed IPs", func(s *types.AuthSettings) *[]string { return &s.AllowIPs }, func(p *types.AuthPatch, v *[]string) { p.AllowIPs = v }, false)},
	{Name: "http-auth:add-bypass-path", Help: "Let a path in without logging in (end it with * for everything below, e.g. /api/webhook/*)",
		App: NeedsApp, Args: "<path>", MinArgs: 1,
		Run: httpAuthList("bypass paths", func(s *types.AuthSettings) *[]string { return &s.BypassPaths }, func(p *types.AuthPatch, v *[]string) { p.BypassPaths = v }, true)},
	{Name: "http-auth:remove-bypass-path", Help: "Make a path need a login again", App: NeedsApp, Args: "<path>", MinArgs: 1,
		Run: httpAuthList("bypass paths", func(s *types.AuthSettings) *[]string { return &s.BypassPaths }, func(p *types.AuthPatch, v *[]string) { p.BypassPaths = v }, false)},
	{Name: "http-auth:set", Help: "Set a login setting for every app: login-domain (one login page for all apps) or session-days",
		App: AppOrGlobal, Args: "<login-domain|session-days> [<value>]", MinArgs: 1, MaxArgs: 2, Run: httpAuthSet},
	{Name: "http-auth:report", Help: "Display an app's login, or with --global the login settings", App: AppOrGlobal, AnyFlags: true, Run: httpAuthReport},
	{Name: "http-auth:share", Help: "Make a link that lets anyone holding it into an app, until it expires", App: NeedsApp, Flags: []Flag{
		{Name: "expires", Value: "DURATION", Help: "How long it works (default 24h; e.g. 30m, 7d)"},
		{Name: "note", Value: "TEXT", Help: "A note to remember who it's for"},
	}, Run: httpAuthShare},
	{Name: "http-auth:shares", Help: "List an app's share links", App: NeedsApp, Run: httpAuthShares},
	{Name: "http-auth:unshare", Help: "Revoke a share link", App: NeedsApp, Args: "<id>", MinArgs: 1, Run: httpAuthUnshare},
	{Name: "http-auth:users:add", Help: "Add a user who can log in to apps", Args: "<name>", MinArgs: 1, Flags: []Flag{
		{Name: "totp", Help: "Also ask for a code from an authenticator app"},
	}, Run: httpAuthUsersAdd},
	{Name: "http-auth:users:list", Help: "List the users", Run: httpAuthUsersList},
	{Name: "http-auth:users:passwd", Help: "Change a user's password (ending their sessions)", Args: "<name>", MinArgs: 1, Run: httpAuthUsersPasswd},
	{Name: "http-auth:users:totp", Help: "Give a user a new authenticator code (or --off to remove it)", Args: "<name>", MinArgs: 1,
		Flags: []Flag{{Name: "off", Help: "Log in with the password alone"}}, Run: httpAuthUsersTOTP},
	{Name: "http-auth:users:remove", Help: "Remove a user", Args: "<name>", MinArgs: 1, Run: httpAuthUsersRemove},
}

func init() {
	namespaceHelp["http-auth"] = "Put a login in front of apps: a shared password, or users with an authenticator code"
}

func httpAuthEnable(c *Context) error {
	p := types.AuthPatch{Mode: types.AuthUsers}
	if c.Bool("password") {
		if len(c.Args) > 0 {
			return usageErr("--password is one shared password; users log in with their own")
		}
		pw, err := newSecret(c, "Password for "+c.App)
		if err != nil {
			return err
		}
		p.Mode, p.Password = types.AuthPassword, pw
	} else {
		users := append([]string{}, c.Args...)
		p.Users = &users
	}
	st, err := c.API.PatchAuth(c, c.App, p)
	if err != nil {
		return err
	}
	if st.Mode == types.AuthPassword {
		c.Step("%s now asks for a password", c.App)
		return nil
	}
	who := "any user"
	if len(st.Users) > 0 {
		who = strings.Join(st.Users, ", ")
	}
	c.Step("%s now asks for a login: %s", c.App, who)
	if users, err := c.API.AuthUsers(c); err == nil && len(users) == 0 {
		c.Info("There are no users yet. Add one with: jokku http-auth:users:add <name>")
	}
	return nil
}

func httpAuthDisable(c *Context) error {
	if _, err := c.API.PatchAuth(c, c.App, types.AuthPatch{Mode: "off"}); err != nil {
		return err
	}
	c.Step("%s no longer asks for a login", c.App)
	return nil
}

func httpAuthSetPassword(c *Context) error {
	pw, err := newSecret(c, "New password for "+c.App)
	if err != nil {
		return err
	}
	if _, err := c.API.PatchAuth(c, c.App, types.AuthPatch{Password: pw}); err != nil {
		return err
	}
	c.Step("Changed the password of %s; everyone logged in with the old one is logged out", c.App)
	return nil
}

// httpAuthList adds a value to (or removes one from) one of an app's login
// lists.
func httpAuthList(what string, get func(*types.AuthSettings) *[]string, set func(*types.AuthPatch, *[]string), add bool) func(*Context) error {
	return func(c *Context) error {
		st, err := c.API.Auth(c, c.App)
		if err != nil {
			return err
		}
		list, v := *get(st), c.Args[0]
		switch {
		case add && slices.Contains(list, v):
			c.Step("%s already has %s", c.App, v)
			return nil
		case add:
			list = append(list, v)
		case !slices.Contains(list, v):
			return fmt.Errorf("%s isn't in the %s of %s", v, what, c.App)
		default:
			list = slices.DeleteFunc(list, func(s string) bool { return s == v })
		}
		var p types.AuthPatch
		set(&p, &list)
		st, err = c.API.PatchAuth(c, c.App, p)
		if err != nil {
			return err
		}
		c.Step("The %s of %s are now: %s", what, c.App, orNone(*get(st)))
		if st.Mode == "off" {
			c.Info("%s has no login yet; turn one on with: jokku http-auth:enable %s", c.App, c.App)
		}
		return nil
	}
}

func httpAuthSet(c *Context) error {
	if !c.Global() {
		return usageErr("login-domain and session-days are settings for every app: use --global")
	}
	key, value := c.Args[0], ""
	if len(c.Args) == 2 {
		value = c.Args[1]
	}
	var p types.AuthPatch
	switch key {
	case "login-domain":
		p.LoginDomain = &value
	case "session-days":
		if value == "" {
			value = "30"
		}
		n, err := strconv.Atoi(value)
		if err != nil {
			return usageErr("session-days is a number of days")
		}
		p.SessionDays = n
	default:
		return usageErr("Unknown setting %q, expected login-domain or session-days", key)
	}
	if _, err := c.API.PatchAuth(c, "", p); err != nil {
		return err
	}
	if value == "" {
		c.Step("Unsetting %s", key)
	} else {
		c.Step("Setting %s to %s", key, value)
	}
	if key == "login-domain" && value != "" {
		c.Info("Point %s at your ingress nodes (or edges): apps whose login is the users log in there, once for all of them", value)
	}
	return nil
}

func httpAuthReport(c *Context) error {
	if c.Global() {
		st, err := c.API.Auth(c, "")
		if err != nil {
			return err
		}
		return c.report("Global login settings", []row{
			{"http-auth-login-domain", "Login domain", orNone(splitNonEmpty(st.LoginDomain))},
			{"http-auth-session-days", "Session days", strconv.Itoa(st.SessionDays)},
		})
	}
	st, err := c.API.Auth(c, c.App)
	if err != nil {
		return err
	}
	users := "every user"
	if len(st.Users) > 0 {
		users = strings.Join(st.Users, ", ")
	}
	if st.Mode != types.AuthUsers {
		users = "-"
	}
	return c.report(c.App+" login information", []row{
		{"http-auth-mode", "Login", st.Mode},
		{"http-auth-users", "Users", users},
		{"http-auth-allowed-ips", "Allowed IPs", orNone(st.AllowIPs)},
		{"http-auth-bypass-paths", "Bypass paths", orNone(st.BypassPaths)},
		{"http-auth-shares", "Share links", strconv.Itoa(st.Shares)},
	})
}

func httpAuthShare(c *Context) error {
	ttl := 24 * time.Hour
	if c.Bool("expires") {
		d, err := parseLongDuration(c.String("expires"))
		if err != nil {
			return usageErr("Invalid --expires %q, expected e.g. 30m, 24h or 7d", c.String("expires"))
		}
		ttl = d
	}
	sh, err := c.API.CreateAuthShare(c, c.App, types.CreateShareRequest{TTLSeconds: int(ttl.Seconds()), Note: c.String("note")})
	if err != nil {
		return err
	}
	c.Header("Anyone with this link can use %s until %s (revoke it with: jokku http-auth:unshare %s %s):",
		c.App, sh.ExpiresAt.Local().Format("Jan 2 15:04 MST"), c.App, sh.ID)
	fmt.Fprintln(c.Stdout, sh.URL)
	return nil
}

// parseLongDuration is time.ParseDuration plus days: 7d.
func parseLongDuration(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n <= 0 {
			return 0, errors.New("invalid")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, errors.New("invalid")
	}
	return d, nil
}

func httpAuthShares(c *Context) error {
	list, err := c.API.AuthShares(c, c.App)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintf(c.Stdout, "%s has no share links. Make one with: jokku http-auth:share %s\n", c.App, c.App)
		return nil
	}
	tw := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tEXPIRES\tCREATED\tNOTE")
	for _, s := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.ID, s.ExpiresAt.Local().Format("2006-01-02 15:04"), ago(s.CreatedAt), s.Note)
	}
	return tw.Flush()
}

func httpAuthUnshare(c *Context) error {
	if err := c.API.DeleteAuthShare(c, c.App, c.Args[0]); err != nil {
		return err
	}
	c.Step("Revoked share link %s of %s", c.Args[0], c.App)
	return nil
}

func httpAuthUsersAdd(c *Context) error {
	name := c.Args[0]
	pw, err := newSecret(c, "Password for "+name)
	if err != nil {
		return err
	}
	req := types.AuthUserRequest{Name: name, Password: pw}
	if c.Bool("totp") {
		t := true
		req.TOTP = &t
	}
	u, err := c.API.CreateAuthUser(c, req)
	if err != nil {
		return err
	}
	c.Step("Added user %s", name)
	showTOTP(c, u)
	c.Info("Let them into an app with: jokku http-auth:enable <app> %s", name)
	return nil
}

func httpAuthUsersList(c *Context) error {
	users, err := c.API.AuthUsers(c)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		fmt.Fprintln(c.Stdout, "No users yet. Add one with: jokku http-auth:users:add <name>")
		return nil
	}
	tw := tabwriter.NewWriter(c.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tAUTHENTICATOR\tCREATED")
	for _, u := range users {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", u.Name, map[bool]string{true: "yes", false: "no"}[u.TOTP], ago(u.CreatedAt))
	}
	return tw.Flush()
}

func httpAuthUsersPasswd(c *Context) error {
	pw, err := newSecret(c, "New password for "+c.Args[0])
	if err != nil {
		return err
	}
	if _, err := c.API.PatchAuthUser(c, c.Args[0], types.AuthUserRequest{Password: pw}); err != nil {
		return err
	}
	c.Step("Changed the password of %s; their sessions have ended", c.Args[0])
	return nil
}

func httpAuthUsersTOTP(c *Context) error {
	on := !c.Bool("off")
	u, err := c.API.PatchAuthUser(c, c.Args[0], types.AuthUserRequest{TOTP: &on})
	if err != nil {
		return err
	}
	if !on {
		c.Step("%s now logs in with a password alone", c.Args[0])
		return nil
	}
	c.Step("Gave %s a new authenticator code; the old one no longer works", c.Args[0])
	showTOTP(c, u)
	return nil
}

func httpAuthUsersRemove(c *Context) error {
	if err := c.API.DeleteAuthUser(c, c.Args[0]); err != nil {
		return err
	}
	c.Step("Removed user %s", c.Args[0])
	return nil
}

// showTOTP prints a new TOTP secret: a QR code to scan on a terminal, and
// the secret to type in.
func showTOTP(c *Context, u *types.AuthUserResult) {
	if u.TOTPSecret == "" {
		return
	}
	c.Info("Scan this with an authenticator app (or enter the key below). It is shown only once.")
	if f, ok := c.Stdout.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		if code, err := qr.Encode(u.TOTPURI, qr.M); err == nil {
			fmt.Fprintln(c.Stdout)
			printQR(c.Stdout, code)
			fmt.Fprintln(c.Stdout)
		}
	}
	c.Info("Key: %s", u.TOTPSecret)
	c.Info("URI: %s", u.TOTPURI)
}

// printQR draws a QR code with half blocks, two modules per character,
// dark on light whatever the terminal's colors, with a quiet zone.
func printQR(w io.Writer, code *qr.Code) {
	const quiet = 2
	for y := -quiet; y < code.Size+quiet; y += 2 {
		var b strings.Builder
		b.WriteString("       ")
		for x := -quiet; x < code.Size+quiet; x++ {
			top, bottom := code.Black(x, y), code.Black(x, y+1)
			fg, bg := "97", "107" // white
			if top {
				fg = "30"
			}
			if bottom {
				bg = "40"
			}
			b.WriteString("\x1b[" + fg + ";" + bg + "m▀")
		}
		b.WriteString("\x1b[0m")
		fmt.Fprintln(w, b.String())
	}
}

// newSecret asks for a password twice on a terminal, or reads it from
// stdin.
func newSecret(c *Context, prompt string) (string, error) {
	pw, err := readSecret(c, prompt+": ")
	if err != nil {
		return "", err
	}
	if pw == "" {
		return "", errors.New("no password given")
	}
	if f, ok := c.Stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		again, err := readSecret(c, "Again: ")
		if err != nil {
			return "", err
		}
		if again != pw {
			return "", errors.New("the passwords don't match")
		}
	}
	return pw, nil
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, " ")
}

func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}
