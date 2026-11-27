package gateway

import (
	"net/http"
	"testing"
)

func TestSetModel(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"model":"a","x":1}`, `{"model":"b/<&>","x":1}`},
		{"{ \"model\" :\n\t \"a\" , \"x\":1}", "{ \"model\" :\n\t \"b/<&>\" , \"x\":1}"},
		{`{"messages":[{"model":"a"}],"model":"a"}`, `{"messages":[{"model":"a"}],"model":"b/<&>"}`},
		{`{"model":"a","model":"a"}`, `{"model":"a","model":"b/<&>"}`}, // last wins, as in encoding/json
	}
	for _, c := range cases {
		got, err := setModel([]byte(c.in), "b/<&>")
		if err != nil || string(got) != c.want {
			t.Errorf("setModel(%s) = %s, %v; want %s", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{`[1]`, `{"x":1}`, `{"model":`} {
		if _, err := setModel([]byte(bad), "b"); err == nil {
			t.Errorf("setModel(%s): expected error", bad)
		}
	}
}

func TestForwardHeaders(t *testing.T) {
	in := http.Header{}
	for k, v := range map[string]string{
		"Authorization": "Bearer aif", "X-Api-Key": "aif", "Anthropic-Version": "2023-06-01",
		"Anthropic-Beta": "a,b", "Anthropic-Workspace-Id": "w", "User-Agent": "claude-cli/1",
		"X-Claude-Code-Session-Id": "s", "Cookie": "c", "X-Forwarded-For": "1.2.3.4", "Accept-Encoding": "gzip",
	} {
		in.Set(k, v)
	}
	out := forwardHeaders(in)
	if len(out) != 4 || out.Get("Anthropic-Beta") != "a,b" || out.Get("User-Agent") != "claude-cli/1" ||
		out.Get("Anthropic-Version") == "" || out.Get("X-Claude-Code-Session-Id") != "s" {
		t.Fatalf("forwardHeaders = %v", out)
	}
}

func TestAnthropicErrorType(t *testing.T) {
	for status, want := range map[int]string{400: "invalid_request_error", 401: "authentication_error", 403: "permission_error",
		404: "not_found_error", 413: "request_too_large", 429: "rate_limit_error", 502: "api_error", 529: "overloaded_error"} {
		if got := anthropicErrorType(status); got != want {
			t.Errorf("%d: %s, want %s", status, got, want)
		}
	}
}
