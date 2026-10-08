package build

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// DefaultPort is $PORT when the image EXPOSEs no TCP port.
const DefaultPort = 5000

var (
	// An EXPOSE step in the image history: "EXPOSE map[3000/tcp:{}]" from
	// BuildKit, "/bin/sh -c #(nop)  EXPOSE 3000" from the classic builder.
	exposeStep = regexp.MustCompile(`^(?:/bin/sh -c #\(nop\)\s+)?EXPOSE\s+(.*)$`)
	portSpec   = regexp.MustCompile(`(\d+)(?:/([a-z]+))?`)
)

// Port picks $PORT for an image and says where it came from. An image
// inherits its base image's EXPOSEd ports (nginx's 80, say), so when it
// exposes several, the ones from the newest EXPOSE step win: normally the
// app's own Dockerfile. Without EXPOSE, the image's PORT variable counts.
func Port(cfg *v1.ConfigFile) (int, string) {
	exposed := map[int]bool{}
	for spec := range cfg.Config.ExposedPorts {
		for _, n := range tcpPorts(spec) {
			exposed[n] = true
		}
	}
	if len(exposed) == 0 {
		for _, kv := range cfg.Config.Env {
			if v, ok := strings.CutPrefix(kv, "PORT="); ok && validPort(v) {
				n, _ := strconv.Atoi(v)
				return n, "from the image's PORT variable"
			}
		}
		return DefaultPort, "the default, as the image EXPOSEs no port"
	}
	ports := sortedPorts(exposed)
	if len(ports) > 1 {
		for i := len(cfg.History) - 1; i >= 0; i-- {
			m := exposeStep.FindStringSubmatch(cfg.History[i].CreatedBy)
			if m == nil {
				continue
			}
			newest := map[int]bool{}
			for _, n := range tcpPorts(m[1]) {
				if exposed[n] {
					newest[n] = true
				}
			}
			if len(newest) > 0 {
				ports = sortedPorts(newest)
				break
			}
		}
	}
	list := make([]string, len(ports))
	for i, n := range ports {
		list[i] = strconv.Itoa(n)
	}
	if len(ports) > 1 {
		return ports[0], "the lowest of EXPOSE " + strings.Join(list, " ") + " (config:set PORT to choose another)"
	}
	return ports[0], "from EXPOSE " + list[0]
}

// tcpPorts returns the TCP ports in s, which holds port specs like "80",
// "3000/tcp" or "53/udp".
func tcpPorts(s string) []int {
	var out []int
	for _, m := range portSpec.FindAllStringSubmatch(s, -1) {
		if (m[2] == "" || m[2] == "tcp") && validPort(m[1]) {
			n, _ := strconv.Atoi(m[1])
			out = append(out, n)
		}
	}
	return out
}

func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && n < 65536
}

func sortedPorts(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}
