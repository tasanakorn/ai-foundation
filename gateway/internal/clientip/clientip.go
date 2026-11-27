// Package clientip finds the client IP of a request behind trusted reverse proxies
// (spec-0003 section 3, D21). A header is used only when the TCP peer is a trusted proxy.
package clientip

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// DefaultHeader is the header read when none is configured.
const DefaultHeader = "X-Forwarded-For"

// Resolver returns the client IP of a request. The zero value (and nil) uses the TCP peer.
type Resolver struct {
	trusted []netip.Prefix
	header  string
	xff     bool
}

// New parses trusted proxy IPs or CIDRs. An empty list means "trust no proxy".
func New(trusted []string, header string) (*Resolver, error) {
	if header == "" {
		header = DefaultHeader
	}
	r := &Resolver{header: http.CanonicalHeaderKey(header)}
	r.xff = r.header == DefaultHeader
	for _, s := range trusted {
		p, err := ParsePrefix(s)
		if err != nil {
			return nil, err
		}
		r.trusted = append(r.trusted, p)
	}
	return r, nil
}

// ParsePrefix accepts "10.0.0.0/8", "fd00::/8" or a single IP ("10.1.2.3").
func ParsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("trusted proxy %q: %w", s, err)
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("trusted proxy %q: not an IP or CIDR", s)
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// IP is the client IP as a string. It never fails: it falls back to the TCP peer.
func (r *Resolver) IP(req *http.Request) string {
	peer, ok := parseHostPort(req.RemoteAddr)
	if !ok {
		return req.RemoteAddr
	}
	if r == nil || !r.isTrusted(peer) {
		return peer.String()
	}
	if !r.xff {
		if a, ok := parseIP(req.Header.Get(r.header)); ok {
			return a.String()
		}
		return peer.String()
	}
	// X-Forwarded-For: client, proxy1, proxy2. Several headers join in order.
	var hops []string
	for _, v := range req.Header.Values(r.header) {
		hops = append(hops, strings.Split(v, ",")...)
	}
	var leftmost netip.Addr
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := parseIP(hops[i])
		if !ok {
			break // a broken entry: do not trust anything left of it
		}
		if !r.isTrusted(a) {
			return a.String()
		}
		leftmost = a
	}
	if leftmost.IsValid() {
		return leftmost.String()
	}
	return peer.String()
}

func (r *Resolver) isTrusted(a netip.Addr) bool {
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func parseHostPort(s string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = s
	}
	return parseIP(host)
}

func parseIP(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimSuffix(s, "]"), "[")
	if i := strings.IndexByte(s, '%'); i >= 0 { // zone, e.g. fe80::1%eth0
		s = s[:i]
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}
