// Package version holds build metadata, overridden at link time:
//
//	go build -ldflags "-X github.com/wes/jokku/internal/version.Version=v0.1.0"
package version

import (
	"strconv"
	"strings"
)

var (
	Version = "0.0.0-dev"
	Commit  = "unknown"
)

// Repo is the GitHub repository releases and install scripts come from.
const Repo = "wes/jokku"

// Release reports whether v names a published release, like v0.8.5, rather
// than a development build (v0.8.5-3-g1a2b3c4, 0.0.0-dev).
func Release(v string) bool {
	_, ok := parse(v)
	return ok
}

// Older reports whether release a came before release b. It is false unless
// both are releases.
func Older(a, b string) bool {
	x, ok1 := parse(a)
	y, ok2 := parse(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	rest, ok := strings.CutPrefix(v, "v")
	parts := strings.Split(rest, ".")
	if !ok || len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p != strconv.Itoa(n) {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
