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
	"log/slog"
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

// BuildOptions are shared by every provider Build makes.
type BuildOptions struct {
	Client   *http.Client
	Observer Observer     // optional: counts upstream attempts
	Log      *slog.Logger // optional: failed attempts
	Sync     StateSync    // optional: shares cooldowns with the other nodes (D40)
}

// Build creates one Pool per provider, keyed by name. Each account becomes one client
// of the provider's type inside the pool (spec-0004 section 9). cfgs must be normalized
// (config.NormalizeCatalog); a provider without accounts uses its api_key as one account.
func Build(cfgs []config.Provider, o BuildOptions) (map[string]Provider, error) {
	out := make(map[string]Provider, len(cfgs))
	for _, c := range cfgs {
		if len(c.Accounts) == 0 { // a hand-built config (tests): api_key is the one account
			c.Accounts = []config.Account{{Name: config.DefaultAccount, APIKey: c.APIKey}}
		}
		members := make([]*member, 0, len(c.Accounts))
		for _, a := range c.Accounts {
			ac := c // the account's view: its key, and its overrides on top of the provider
			ac.APIKey, ac.Accounts = a.APIKey, nil
			if a.BaseURL != "" {
				ac.BaseURL = a.BaseURL
			}
			if a.WorkspaceID != "" {
				ac.WorkspaceID = a.WorkspaceID
			}
			if len(a.Headers) > 0 {
				ac.Headers = make(map[string]string, len(c.Headers)+len(a.Headers))
				for k, v := range c.Headers {
					ac.Headers[k] = v
				}
				for k, v := range a.Headers {
					ac.Headers[k] = v
				}
			}
			var p Provider
			switch c.Type {
			case config.TypeOpenAI, config.TypeOpenRouter:
				p = NewOpenAI(ac, o.Client)
			case config.TypeAnthropic:
				p = NewAnthropic(ac, o.Client)
			default:
				return nil, fmt.Errorf("provider %q: unsupported type %q", c.Name, c.Type)
			}
			members = append(members, &member{name: a.Name, p: p, weight: a.WeightOr(), credit: a.Credit})
		}
		pool := newPool(c, members, o)
		if c.Type == config.TypeAnthropic {
			out[c.Name] = &NativePool{pool}
		} else {
			out[c.Name] = pool
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
