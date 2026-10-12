package proxy

import (
	"fmt"
	"sort"

	// Caddy's layer4 app, with only the modules streams use.
	_ "github.com/mholt/caddy-l4/layer4"
	_ "github.com/mholt/caddy-l4/modules/l4close"
	_ "github.com/mholt/caddy-l4/modules/l4postgres"
	_ "github.com/mholt/caddy-l4/modules/l4proxy"
	_ "github.com/mholt/caddy-l4/modules/l4subroute"
	_ "github.com/mholt/caddy-l4/modules/l4tls"

	"github.com/wes/jokku/internal/types"
)

// Streams
//
// A stream is a TCP route (types.ProxyStream): Caddy's layer4 app, one
// server per port. On a shared port the proxy terminates TLS with the
// certificate for the name the client asks for and sends the decrypted
// connection on over the mesh, which is encrypted itself. Connections that
// match nothing (no TLS, an unknown name, an address not let in) are
// closed. Open connections outlive config reloads: a deploy elsewhere
// doesn't cut a database session.

// streamServers renders the layer4 servers for streams, and the hosts
// whose certificates they need: from ACME, and from the proxy's own CA.
func streamServers(streams []types.ProxyStream) (servers map[string]any, acme, internal []string) {
	byPort := map[int][]types.ProxyStream{}
	for _, s := range streams {
		byPort[s.Port] = append(byPort[s.Port], s)
	}
	ports := make([]int, 0, len(byPort))
	for p := range byPort {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	issuer := map[string]bool{} // host -> from ACME; a name shared by ports is one certificate
	servers = map[string]any{}
	for _, port := range ports {
		ss := byPort[port]
		sort.Slice(ss, func(i, j int) bool { return ss[i].Host < ss[j].Host })
		var routes []any
		// The API keeps one protocol to a port, and a plain stream alone on
		// its own.
		switch ss[0].Protocol {
		case types.StreamPlain:
			routes = append(routes, streamRoute(ss[0], nil, nil))
		case types.StreamPostgres, types.StreamTLS:
			var named []any
			for _, s := range ss {
				// PG17's direct TLS insists the server picks its ALPN.
				var alpn []string
				if s.Protocol == types.StreamPostgres {
					alpn = []string{"postgresql"}
				}
				named = append(named, streamRoute(s, map[string]any{"sni": []string{s.Host}}, alpn))
				issuer[s.Host] = issuer[s.Host] || s.ACME
			}
			if ss[0].Protocol == types.StreamPostgres {
				// A classic client asks for TLS first (an SSLRequest): the
				// proxy says yes, then routes the handshake that follows by
				// name, like a direct one.
				routes = append(routes, map[string]any{
					"match": []any{map[string]any{"postgres": map[string]any{"tls": "enabled"}}},
					"handle": []any{
						map[string]any{"handler": "postgres_tls"},
						map[string]any{"handler": "subroute", "routes": named},
					},
				})
			}
			routes = append(routes, named...)
		}
		servers[fmt.Sprintf("tcp-%d", port)] = map[string]any{
			"listen": []string{fmt.Sprintf(":%d", port)},
			"routes": routes,
		}
	}
	for host, fromACME := range issuer {
		if fromACME {
			acme = append(acme, host)
		} else {
			internal = append(internal, host)
		}
	}
	sort.Strings(acme)
	sort.Strings(internal)
	return servers, acme, internal
}

// streamRoute matches s's connections (by TLS name when sni is set, and by
// client address when s lets in only some) and sends them to its upstreams,
// terminating TLS first when sni is set.
func streamRoute(s types.ProxyStream, sni map[string]any, alpn []string) map[string]any {
	match := map[string]any{}
	if sni != nil {
		match["tls"] = sni
	}
	if len(s.Allow) > 0 {
		match["remote_ip"] = map[string]any{"ranges": s.Allow}
	}
	var handle []any
	if sni != nil {
		tls := map[string]any{"handler": "tls"}
		if alpn != nil {
			tls["connection_policies"] = []any{map[string]any{"alpn": alpn}}
		}
		handle = append(handle, tls)
	}
	if len(s.Upstreams) == 0 {
		handle = append(handle, map[string]any{"handler": "close"})
	} else {
		// Each upstream on its own: several dials in one would get every
		// connection at once.
		ups := make([]any, len(s.Upstreams))
		for i, u := range s.Upstreams {
			ups[i] = map[string]any{"dial": []string{u}}
		}
		handle = append(handle, map[string]any{"handler": "proxy", "upstreams": ups})
	}
	route := map[string]any{"handle": handle}
	if len(match) > 0 {
		route["match"] = []any{match}
	}
	return route
}
