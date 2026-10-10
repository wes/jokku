package proxy

import (
	"bytes"
	"html/template"
	"net/http"
)

// page is one of the login pages: a form, a message, or the hand-off from
// the login domain back to an app's domain.
type page struct {
	Title   string
	App     string // what is being logged in to
	Action  string // where the form posts
	RD      string // where to go after logging in
	Users   bool   // ask for a user name, not just a password
	User    string
	Code    bool   // the second step: a TOTP code
	Pending string // the first step, signed
	Error   string
	Message string
	Link    string
	// LinkText labels Link; "Continue" without it.
	LinkText string
	// Handoff posts HandoffCode to an app domain's callback.
	Handoff     string
	HandoffCode string
}

func (p page) render(w http.ResponseWriter, status int) error {
	var b bytes.Buffer
	if err := pageTmpl.Execute(&b, p); err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; form-action http: https:; frame-ancestors 'none'")
	w.WriteHeader(status)
	_, err := w.Write(b.Bytes())
	return err
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{if .App}}{{.App}} · {{end}}{{.Title}}</title>
<style>
:root {
  --bg: #f8f3ea; --card: #fffaf3; --line: #e4d9c8; --fg: #1f1812; --muted: #6b5c4c;
  --accent: #a9580f; --accent-strong: #8c470a; --on-accent: #fffaf3; --bad: #b42318; --bad-soft: #fbe9e6;
  color-scheme: light dark;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #16120e; --card: #1c1712; --line: #2f2720; --fg: #efe6d8; --muted: #a99a86;
    --accent: #e8963f; --accent-strong: #f3ae62; --on-accent: #1c130a; --bad: #f07a6a; --bad-soft: #2e1813;
  }
}
* { box-sizing: border-box; }
body {
  margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 24px 16px;
  background: var(--bg); color: var(--fg);
  font: 16px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
}
main { width: 100%; max-width: 380px; }
.card { background: var(--card); border: 1px solid var(--line); border-radius: 14px; padding: 28px; }
h1 { font-size: 20px; line-height: 1.3; margin: 0 0 4px; }
.sub { color: var(--muted); margin: 0 0 20px; font-size: 14px; }
label { display: block; font-size: 14px; font-weight: 600; margin: 14px 0 6px; }
input[type=text], input[type=password] {
  width: 100%; padding: 10px 12px; font: inherit; color: var(--fg); background: var(--bg);
  border: 1px solid var(--line); border-radius: 8px;
}
input:focus { outline: 2px solid var(--accent); outline-offset: 1px; border-color: transparent; }
.code { letter-spacing: 0.3em; font-variant-numeric: tabular-nums; text-align: center; font-size: 20px; }
button, .button {
  display: block; width: 100%; margin-top: 20px; padding: 11px; font: inherit; font-weight: 600; text-align: center;
  color: var(--on-accent); background: var(--accent); border: 0; border-radius: 8px; cursor: pointer; text-decoration: none;
}
button:hover, .button:hover { background: var(--accent-strong); }
.error { background: var(--bad-soft); color: var(--bad); border-radius: 8px; padding: 10px 12px; font-size: 14px; margin: 0 0 4px; }
footer { text-align: center; color: var(--muted); font-size: 12px; margin-top: 16px; }
</style>
</head>
<body>
<main>
<div class="card">
{{- if .Handoff}}
  <h1>Logging you in…</h1>
  <form id="handoff" method="post" action="{{.Handoff}}">
    <input type="hidden" name="code" value="{{.HandoffCode}}">
    <input type="hidden" name="rd" value="{{.RD}}">
    <noscript><button type="submit">Continue</button></noscript>
  </form>
  <script>document.getElementById("handoff").submit()</script>
{{- else}}
  <h1>{{if .Message}}{{.Title}}{{else if .App}}Log in to {{.App}}{{else}}Log in{{end}}</h1>
  {{- if .Code}}<p class="sub">Enter the code from your authenticator app.</p>
  {{- else if .Action}}<p class="sub">{{if .Users}}This site is private.{{else}}This site needs a password.{{end}}</p>{{end}}
  {{- if .Error}}<p class="error" role="alert">{{.Error}}</p>{{end}}
  {{- if .Message}}<p>{{.Message}}</p>{{end}}
  {{- if .Link}}<a class="button" href="{{.Link}}">{{if .LinkText}}{{.LinkText}}{{else}}Continue{{end}}</a>{{end}}
  {{- if and .Action (not .Message)}}
  <form method="post" action="{{.Action}}">
    <input type="hidden" name="rd" value="{{.RD}}">
    {{- if .Code}}
    <input type="hidden" name="pending" value="{{.Pending}}">
    <label for="code">Code</label>
    <input class="code" type="text" id="code" name="code" inputmode="numeric" pattern="[0-9 ]*" autocomplete="one-time-code" maxlength="7" required autofocus>
    {{- else if .Users}}
    <label for="user">User name</label>
    <input type="text" id="user" name="user" value="{{.User}}" autocomplete="username" autocapitalize="none" spellcheck="false" required {{if not .User}}autofocus{{end}}>
    <label for="password">Password</label>
    <input type="password" id="password" name="password" autocomplete="current-password" required {{if .User}}autofocus{{end}}>
    {{- else}}
    <label for="password">Password</label>
    <input type="password" id="password" name="password" autocomplete="current-password" required autofocus>
    {{- end}}
    <button type="submit">{{if .Code}}Verify{{else}}Log in{{end}}</button>
  </form>
  {{- end}}
{{- end}}
</div>
<footer>Protected by Jokku</footer>
</main>
</body>
</html>
`))

// unreachablePage is shown when an app doesn't answer: its instances are
// restarting, or the nodes behind an edge are offline. Caddy fills in the
// host.
const unreachablePage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>Can't reach this site right now</title>
<style>
:root { --bg: #f8f3ea; --fg: #1f1812; --muted: #6b5c4c; color-scheme: light dark; }
@media (prefers-color-scheme: dark) { :root { --bg: #16120e; --fg: #efe6d8; --muted: #a99a86; } }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 24px 16px; box-sizing: border-box;
  background: var(--bg); color: var(--fg); font: 16px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif; }
main { max-width: 440px; }
h1 { font-size: 20px; margin: 0 0 8px; }
p { color: var(--muted); margin: 0; }
</style>
</head>
<body>
<main>
<h1>{http.request.host} can't be reached right now</h1>
<p>The server behind it isn't answering. It may be restarting or offline; try again in a minute.</p>
</main>
</body>
</html>
`
