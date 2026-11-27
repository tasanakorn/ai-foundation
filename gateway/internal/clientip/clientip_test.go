package clientip

import (
	"net/http"
	"testing"
)

func TestResolver(t *testing.T) {
	xff, err := New([]string{"10.0.0.0/8", "192.0.2.7", "fd00::/8"}, "")
	if err != nil {
		t.Fatal(err)
	}
	cf, err := New([]string{"10.0.0.0/8"}, "cf-connecting-ip")
	if err != nil {
		t.Fatal(err)
	}
	none, _ := New(nil, "")
	cases := []struct {
		name   string
		r      *Resolver
		peer   string
		header string
		values []string
		want   string
	}{
		{"nil resolver uses peer", nil, "203.0.113.9:1234", "X-Forwarded-For", []string{"1.1.1.1"}, "203.0.113.9"},
		{"no trusted proxies ignores header", none, "10.1.1.1:1", "X-Forwarded-For", []string{"1.1.1.1"}, "10.1.1.1"},
		{"untrusted peer ignores header", xff, "203.0.113.9:1", "X-Forwarded-For", []string{"1.1.1.1"}, "203.0.113.9"},
		{"trusted peer, one hop", xff, "10.1.1.1:1", "X-Forwarded-For", []string{"198.51.100.4"}, "198.51.100.4"},
		{"rightmost untrusted wins over a spoofed left entry", xff, "10.1.1.1:1", "X-Forwarded-For",
			[]string{"6.6.6.6, 198.51.100.4, 10.2.2.2"}, "198.51.100.4"},
		{"several headers join in order", xff, "10.1.1.1:1", "X-Forwarded-For", []string{"6.6.6.6", "198.51.100.4"}, "198.51.100.4"},
		{"single trusted IP", xff, "192.0.2.7:1", "X-Forwarded-For", []string{"198.51.100.4"}, "198.51.100.4"},
		{"all trusted takes leftmost", xff, "10.1.1.1:1", "X-Forwarded-For", []string{"10.3.3.3, 10.2.2.2"}, "10.3.3.3"},
		{"broken entry stops the walk", xff, "10.1.1.1:1", "X-Forwarded-For", []string{"198.51.100.4, junk, 10.2.2.2"}, "10.2.2.2"},
		{"empty header uses peer", xff, "10.1.1.1:1", "", nil, "10.1.1.1"},
		{"ipv6 peer and entry", xff, "[fd00::1]:443", "X-Forwarded-For", []string{"2001:db8::5"}, "2001:db8::5"},
		{"ipv4-mapped peer", xff, "[::ffff:10.1.1.1]:1", "X-Forwarded-For", []string{"198.51.100.4"}, "198.51.100.4"},
		{"single-value header", cf, "10.1.1.1:1", "Cf-Connecting-Ip", []string{"198.51.100.4"}, "198.51.100.4"},
		{"single-value header ignores XFF", cf, "10.1.1.1:1", "X-Forwarded-For", []string{"198.51.100.4"}, "10.1.1.1"},
		{"single-value header junk", cf, "10.1.1.1:1", "Cf-Connecting-Ip", []string{"1.2.3.4, 5.6.7.8"}, "10.1.1.1"},
		{"unparsable peer is returned as is", xff, "@", "X-Forwarded-For", []string{"1.1.1.1"}, "@"},
	}
	for _, c := range cases {
		r, _ := http.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.peer
		for _, v := range c.values {
			r.Header.Add(c.header, v)
		}
		if got := c.r.IP(r); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestParsePrefix(t *testing.T) {
	for _, ok := range []string{"10.0.0.0/8", "10.1.2.3", " 10.1.2.3 ", "fd00::/8", "::1", "10.1.2.3/8"} {
		if _, err := ParsePrefix(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "10.0.0.0/33", "example.com", "10.0.0"} {
		if _, err := ParsePrefix(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
	p, _ := ParsePrefix("10.1.2.3/8")
	if p.String() != "10.0.0.0/8" {
		t.Errorf("not masked: %s", p)
	}
}
