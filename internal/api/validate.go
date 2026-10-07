package api

import (
	"regexp"
	"strings"
)

var (
	// App names become hostnames (<app>.example.com), so they are DNS labels.
	appNameRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	configKeyRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	processTypeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	domainLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

func validAppName(name string) error {
	if !appNameRe.MatchString(name) {
		return badRequest("App name %q is invalid: use lowercase letters, digits and dashes (max 63, no leading or trailing dash)", name)
	}
	return nil
}

func validConfigKey(key string) error {
	if !configKeyRe.MatchString(key) {
		return badRequest("Config key %q is invalid: use letters, digits and underscores, not starting with a digit", key)
	}
	return nil
}

func validProcessType(p string) error {
	if !processTypeRe.MatchString(p) {
		return badRequest("Process type %q is invalid", p)
	}
	return nil
}

// normalizeDomain lowercases a domain and checks it is a hostname, optionally
// with a leading "*." wildcard.
func normalizeDomain(d string) (string, error) {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	host := strings.TrimPrefix(d, "*.")
	if len(d) > 253 || host == "" {
		return "", badRequest("Domain %q is invalid", d)
	}
	for _, label := range strings.Split(host, ".") {
		if !domainLabelRe.MatchString(label) {
			return "", badRequest("Domain %q is invalid", d)
		}
	}
	return d, nil
}

func normalizeDomains(ds []string) ([]string, error) {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		n, err := normalizeDomain(d)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}
