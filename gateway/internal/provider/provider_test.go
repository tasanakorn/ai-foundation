package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"ai-foundation/gateway/internal/config"
)

func TestAnthropicCompat(t *testing.T) {
	var got *http.Request
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x"}`)
	}))
	defer srv.Close()

	ps, err := Build([]config.Provider{{
		Name: "anthropic", Type: config.TypeAnthropic, BaseURL: srv.URL + "/v1", APIKey: "sk-ant-test",
		WorkspaceID: "wrkspc_1", OpenAIFormat: config.OpenAIFormatCompat,
		Headers: map[string]string{"X-Api-Key": "must-not-win", "X-Extra": "1"},
	}}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	p := ps["anthropic"]
	if _, ok := p.(*Anthropic); !ok || p.Name() != "anthropic" {
		t.Fatalf("Build gave %T %q", p, p.Name())
	}

	const body = `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}]}`
	resp, err := p.Do(context.Background(), "/chat/completions", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got.Method != http.MethodPost || got.URL.Path != "/v1/chat/completions" {
		t.Fatalf("request %s %s", got.Method, got.URL.Path)
	}
	if gotBody != body {
		t.Fatalf("body changed: %s", gotBody)
	}
	for h, want := range map[string]string{
		"X-Api-Key":              "sk-ant-test",
		"Anthropic-Workspace-Id": "wrkspc_1",
		"Content-Type":           "application/json",
		"X-Extra":                "1",
		"Authorization":          "",
	} {
		if v := got.Header.Get(h); v != want {
			t.Errorf("header %s = %q, want %q", h, v, want)
		}
	}
}

func TestAnthropicNoWorkspaceHeader(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header }))
	defer srv.Close()

	p := NewAnthropic(config.Provider{Name: "a", BaseURL: srv.URL, APIKey: "k"}, srv.Client())
	resp, err := p.Do(context.Background(), "/chat/completions", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, ok := got["Anthropic-Workspace-Id"]; ok {
		t.Fatal("workspace header sent without workspace_id")
	}
}
