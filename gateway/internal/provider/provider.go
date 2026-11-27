// Package provider talks to upstream LLM APIs.
//
// A Provider takes an OpenAI-format request body and returns an OpenAI-format
// response (Do). A provider that also implements Native serves its own API format
// as a passthrough (the Anthropic Messages API under /anthropic).
package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"ai-foundation/gateway/internal/config"
)

type Provider interface {
	Name() string
	// Do sends body to endpoint ("/chat/completions", "/embeddings", ...).
	// The caller closes the response body.
	Do(ctx context.Context, endpoint string, body []byte) (*http.Response, error)
}

// Native is a provider that also serves its own API format unchanged (passthrough),
// such as the Anthropic Messages API (spec-0002 section 6).
type Native interface {
	Provider
	// NativeFormat is the API format it serves natively ("anthropic").
	NativeFormat() string
	// Native sends method base_url+path?query with body as is. header holds the client
	// headers the gateway chose to forward; the provider adds its own auth.
	Native(ctx context.Context, method, path, query string, header http.Header, body []byte) (*http.Response, error)
}

// NewHTTPClient returns a pooled client shared by all providers.
// There is no client-wide timeout: streams can be long. Callers set a context deadline.
func NewHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}}
}

// Build creates providers from config, keyed by name.
func Build(cfgs []config.Provider, client *http.Client) (map[string]Provider, error) {
	out := make(map[string]Provider, len(cfgs))
	for _, c := range cfgs {
		switch c.Type {
		case config.TypeOpenAI, config.TypeOpenRouter:
			out[c.Name] = NewOpenAI(c, client)
		case config.TypeAnthropic:
			out[c.Name] = NewAnthropic(c, client)
		default:
			return nil, fmt.Errorf("provider %q: unsupported type %q", c.Name, c.Type)
		}
	}
	return out, nil
}

// OpenAI is any OpenAI-compatible API: OpenAI, OpenRouter, Groq, Together, vLLM, Ollama, LM Studio, ...
type OpenAI struct {
	name    string
	baseURL string
	apiKey  string
	headers map[string]string
	client  *http.Client
}

func NewOpenAI(c config.Provider, client *http.Client) *OpenAI {
	return &OpenAI{name: c.Name, baseURL: c.BaseURL, apiKey: c.APIKey, headers: c.Headers, client: client}
}

func (p *OpenAI) Name() string { return p.name }

func (p *OpenAI) Do(ctx context.Context, endpoint string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	return p.client.Do(req)
}

// Anthropic is the Claude API (spec-0002 section 5). With openai_format "compat", OpenAI-format
// requests go to Anthropic's OpenAI-compatible endpoint (base_url + endpoint), so the body passes
// through as for any OpenAI-compatible provider. Auth uses x-api-key, never Authorization.
type Anthropic struct {
	name        string
	baseURL     string
	apiKey      string
	workspaceID string
	headers     map[string]string
	client      *http.Client
}

func NewAnthropic(c config.Provider, client *http.Client) *Anthropic {
	return &Anthropic{name: c.Name, baseURL: c.BaseURL, apiKey: c.APIKey, workspaceID: c.WorkspaceID, headers: c.Headers, client: client}
}

func (p *Anthropic) Name() string { return p.name }

func (p *Anthropic) Do(ctx context.Context, endpoint string, body []byte) (*http.Response, error) {
	return p.Native(ctx, http.MethodPost, endpoint, "", nil, body)
}

func (p *Anthropic) NativeFormat() string { return config.FormatAnthropic }

// Native sends a Messages API request. body goes out byte for byte.
func (p *Anthropic) Native(ctx context.Context, method, path, query string, header http.Header, body []byte) (*http.Response, error) {
	u := p.baseURL + path
	if query != "" {
		u += "?" + query
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		req.Header[k] = append([]string(nil), vs...)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	// Set auth last so neither client nor custom headers can replace it.
	req.Header.Del("Authorization")
	if p.apiKey != "" {
		req.Header.Set("X-Api-Key", p.apiKey)
	} else {
		req.Header.Del("X-Api-Key")
	}
	if p.workspaceID != "" {
		req.Header.Set("Anthropic-Workspace-Id", p.workspaceID)
	}
	return p.client.Do(req)
}
