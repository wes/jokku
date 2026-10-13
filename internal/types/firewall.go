package types

import "time"

// The firewall: every proxy a visitor reaches first (an edge, or a node
// for apps reached directly) bans an address that asks for too many pages
// that don't exist, the way vulnerability scanners do, and slows one that
// sends too many requests a second. Each proxy keeps its own counts, and
// tells the control node what it did.

// FeatureFirewall is reported by agents whose proxy has the firewall
// (ProxyState.Firewall), from v0.8.14.
const FeatureFirewall = "firewall"

// ProxyFirewall is the firewall's rules, the same on every proxy.
type ProxyFirewall struct {
	NotFound  int `json:"not_found"`  // pages not found within Window that ban an address
	WindowSec int `json:"window_sec"` // seconds
	BanSec    int `json:"ban_sec"`    // how long a ban lasts, in seconds
	Rate      int `json:"rate"`       // requests a second an address may make; 0 for no limit
	Burst     int `json:"burst"`      // and at once
	// Allow is the addresses (CIDRs) never banned or slowed, besides
	// private ones, which never are.
	Allow []string `json:"allow,omitempty"`
	// Unbanned lifts these addresses' bans, each when a proxy first sees it
	// (firewall:unban).
	Unbanned map[string]time.Time `json:"unbanned,omitempty"`
}

// The firewall's defaults (firewall:set changes them).
const (
	DefaultFirewallNotFound = 20
	DefaultFirewallWindow   = 10 * time.Minute
	DefaultFirewallBan      = time.Hour
	DefaultFirewallRate     = 50
	DefaultFirewallBurst    = 300
)

// What the firewall did to a request, in its access log line
// (Request.Firewall).
const (
	FirewallBanned  = "banned"  // the address is banned: 403
	FirewallLimited = "limited" // too many requests a second: 429
)

// FirewallReport is what a proxy tells the control node every few
// seconds: the bans it has now, what it did since it last said, and the
// addresses getting close to a ban.
type FirewallReport struct {
	Bans     []FirewallBan             `json:"bans"`
	Counts   map[string]FirewallCounts `json:"counts,omitempty"`   // by app; "" for hosts no app serves
	Limited  map[string]FirewallSlowed `json:"limited,omitempty"`  // by address
	Watching []FirewallSuspect         `json:"watching,omitempty"` // the most pages not found first
}

// FirewallCounts is what a proxy did with an app's requests.
type FirewallCounts struct {
	Requests uint64 `json:"requests"`  // let through
	Blocked  uint64 `json:"blocked"`   // from banned addresses (403)
	Limited  uint64 `json:"limited"`   // over the rate (429)
	NotFound uint64 `json:"not_found"` // let through and not found, counting toward a ban
	Bans     uint64 `json:"bans"`      // addresses banned
}

func (c *FirewallCounts) Add(o FirewallCounts) {
	c.Requests += o.Requests
	c.Blocked += o.Blocked
	c.Limited += o.Limited
	c.NotFound += o.NotFound
	c.Bans += o.Bans
}

// FirewallBan is an address a proxy refuses until Until.
type FirewallBan struct {
	Addr    string    `json:"addr"` // an IPv4 address, or an IPv6 /64
	App     string    `json:"app,omitempty"`
	Host    string    `json:"host,omitempty"`
	Paths   []string  `json:"paths,omitempty"` // the last pages not found before the ban
	At      time.Time `json:"at"`
	Until   time.Time `json:"until"`
	Blocked uint64    `json:"blocked"` // requests refused since
	// Nodes are the proxies that banned it (filled in by the control node).
	Nodes []string `json:"nodes,omitempty"`
}

// FirewallSlowed is an address that went over the rate.
type FirewallSlowed struct {
	App     string    `json:"app,omitempty"`
	Limited uint64    `json:"limited"` // requests answered 429
	Last    time.Time `json:"last"`
}

// FirewallSuspect is an address getting pages not found, not (yet) as
// many as a ban takes.
type FirewallSuspect struct {
	Addr     string    `json:"addr"`
	App      string    `json:"app,omitempty"`
	NotFound int       `json:"not_found"` // within the window
	Last     time.Time `json:"last"`
	Node     string    `json:"node,omitempty"`
}

// Firewall is what the control node knows of the firewall
// (firewall:report, jokku top).
type Firewall struct {
	Settings FirewallSettings  `json:"settings"`
	Bans     []FirewallBan     `json:"bans"` // newest first
	Watching []FirewallSuspect `json:"watching,omitempty"`
	// Slowed is the addresses answered 429 in the last hour, the most first.
	Slowed []FirewallSlowedAddr `json:"slowed,omitempty"`
	// Minute, Hour and Day add up every proxy's counts and every app's.
	Minute FirewallCounts `json:"minute"`
	Hour   FirewallCounts `json:"hour"`
	Day    FirewallCounts `json:"day"`
	Sites  []FirewallSite `json:"sites,omitempty"`
	// Nodes are the proxies that report, and Missing those a visitor can
	// reach that run a release without the firewall.
	Nodes   []string `json:"nodes,omitempty"`
	Missing []string `json:"missing,omitempty"`
}

type FirewallSlowedAddr struct {
	Addr string `json:"addr"`
	FirewallSlowed
}

// FirewallSite is one app's counts.
type FirewallSite struct {
	App    string         `json:"app"` // "" for hosts no app serves
	Minute FirewallCounts `json:"minute"`
	Hour   FirewallCounts `json:"hour"`
	Day    FirewallCounts `json:"day"`
}

// FirewallSettings is the firewall's rules, and where it's off.
type FirewallSettings struct {
	Enabled    bool     `json:"enabled"`
	NotFound   int      `json:"not_found"`
	Window     string   `json:"window"` // a duration: 10m
	Ban        string   `json:"ban"`
	Rate       int      `json:"rate"`
	Burst      int      `json:"burst"`
	Allow      []string `json:"allow,omitempty"`
	OffForApps []string `json:"off_for_apps,omitempty"`
}

// FirewallPatch changes the firewall: for every app, or (Enabled only)
// one.
type FirewallPatch struct {
	Enabled  *bool     `json:"enabled,omitempty"`
	NotFound *int      `json:"not_found,omitempty"`
	Window   *string   `json:"window,omitempty"`
	Ban      *string   `json:"ban,omitempty"`
	Rate     *int      `json:"rate,omitempty"`
	Burst    *int      `json:"burst,omitempty"`
	Allow    *[]string `json:"allow,omitempty"`
}

// FirewallUnban lifts an address's bans.
type FirewallUnban struct {
	Addr string `json:"addr"`
}

// FirewallUnbanned says whether the address had a ban.
type FirewallUnbanned struct {
	Banned bool `json:"banned"`
}
