// Package provider talks to upstream LLM APIs.
//
// The gateway speaks the OpenAI wire format on both sides. A Provider takes an
// OpenAI-format request body and returns an OpenAI-format response. A future
// native adapter (for example Anthropic Messages) translates inside Do, so the
// gateway does not change.
package provider

import (
	"bytes"
	"context"
	"fmt"
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	// Set auth last so custom headers cannot replace it.
	if p.apiKey != "" {
		req.Header.Set("X-Api-Key", p.apiKey)
	}
	if p.workspaceID != "" {
		req.Header.Set("Anthropic-Workspace-Id", p.workspaceID)
	}
	return p.client.Do(req)
}
