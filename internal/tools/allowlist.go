package tools

import (
	"net/url"
	"strings"
)

// hostSet is http_request's compiled allowlist: the SSRF guard. An entry
// may be a bare hostname ("example.com" — any port permitted) or a
// host:port ("example.com:8443" — only that port). Matching is
// case-insensitive on the hostname. An empty set denies every host, so a
// deployment must opt hosts in before http_request can reach anything.
type hostSet struct {
	// anyPort holds lowercased hostnames permitted on any port.
	anyPort map[string]struct{}
	// hostPort holds lowercased "host:port" entries permitted only on that
	// exact port.
	hostPort map[string]struct{}
}

// newHostSet compiles an allowlist. Blank entries are ignored; every other
// entry is lowercased and sorted into the any-port or exact-port index by
// whether it carries a port.
func newHostSet(entries []string) hostSet {
	s := hostSet{anyPort: map[string]struct{}{}, hostPort: map[string]struct{}{}}
	for _, e := range entries {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if host, port, ok := splitHostPort(e); ok && port != "" {
			s.hostPort[host+":"+port] = struct{}{}
		} else {
			s.anyPort[e] = struct{}{}
		}
	}
	return s
}

// permit reports whether u's host is allowed, returning the host (host or
// host:port form used for the error) so a denial can name it without
// echoing the full URL. An entry with an explicit port matches only that
// port; a bare-hostname entry matches any port.
func (s hostSet) permit(u *url.URL) (string, bool) {
	hostname := strings.ToLower(u.Hostname())
	port := u.Port()
	display := hostname
	if port != "" {
		display = hostname + ":" + port
	}
	if _, ok := s.anyPort[hostname]; ok {
		return display, true
	}
	if port != "" {
		if _, ok := s.hostPort[hostname+":"+port]; ok {
			return display, true
		}
	}
	return display, false
}

// splitHostPort splits "host:port"; ok is false when there is no colon.
// It tolerates the absence of a port and does not require brackets for
// bare hostnames (IPv6 literals in the allowlist are out of scope).
func splitHostPort(e string) (host, port string, ok bool) {
	i := strings.LastIndex(e, ":")
	if i < 0 {
		return e, "", false
	}
	return e[:i], e[i+1:], true
}
