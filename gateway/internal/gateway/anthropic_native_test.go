package gateway_test

import (
	"encoding/json"
	"strings"
	"testing"

	"ai-foundation/gateway/internal/config"
)

// withNative switches provider "claude" (from withAnthropic) to openai_format native.
func withNative(c *config.Config) {
	for i := range c.Providers {
		if c.Providers[i].Name == "claude" {
			c.Providers[i].OpenAIFormat = config.OpenAIFormatNative
		}
	}
}

func TestAnthropicNativeChatCompletions(t *testing.T) {
	t.Parallel()
	f := newFakeAnthropic(t)
	close(f.release) // streams send all events at once
	e := newEnv(t, withAnthropic(f), withNative)
	secret, _ := e.createKey(`{}`)

	code, out := e.do(e.gw.URL, "POST", "/v1/chat/completions", secret,
		`{"model":"sonnet","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"max_tokens":64}`)
	if code != 200 {
		t.Fatalf("got %d %s", code, out)
	}
	var resp struct {
		Object  string
		Choices []struct {
			Message      struct{ Role, Content string }
			FinishReason string `json:"finish_reason"`
		}
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			PromptTokensDetails struct {
				CachedTokens     int64 `json:"cached_tokens"`
				CacheWriteTokens int64 `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
		}
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil || resp.Object != "chat.completion" || len(resp.Choices) != 1 ||
		resp.Choices[0].Message.Content != "hi" || resp.Usage.PromptTokens != 1150 ||
		resp.Usage.PromptTokensDetails.CachedTokens != 1000 || resp.Usage.PromptTokensDetails.CacheWriteTokens != 50 {
		t.Fatalf("response: %s", out)
	}

	up := f.last(t)
	var body map[string]any
	_ = json.Unmarshal([]byte(up.body), &body)
	if up.path != "/v1/messages" || up.header.Get("Anthropic-Version") != "2023-06-01" || up.header.Get("X-Api-Key") != "anth-secret" ||
		body["model"] != "claude-up-1" || body["max_tokens"] != float64(64) || body["cache_control"] == nil || body["system"] == nil {
		t.Fatalf("upstream got %s %v %s", up.path, up.header, up.body)
	}

	// Cache reads and writes are metered and priced (D20): as on /anthropic.
	r := e.usage()[0]
	if r.Format != "openai" || r.Endpoint != "/chat/completions" || r.PromptTokens != 1150 || r.CachedTokens != 1000 ||
		r.CacheWriteTokens != 50 || r.CompletionTokens != 20 || r.Estimated || !near(r.CostUSD, 725e-6) {
		t.Fatalf("record: %+v", r)
	}

	// A stream comes back as OpenAI chunks, with usage at the end.
	code, out = e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"sonnet","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 || !strings.Contains(out, `"object":"chat.completion.chunk"`) || !strings.Contains(out, `"content":"Hello"`) ||
		!strings.Contains(out, `"finish_reason":"stop"`) || !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("stream: %d %s", code, out)
	}
	r = e.usage()[0]
	if !r.Stream || r.PromptTokens != 210 || r.CachedTokens != 200 || r.CompletionTokens != 30 || r.ReasoningTokens != 12 || r.Estimated {
		t.Fatalf("stream record: %+v", r)
	}

	// Upstream errors keep their status, in the OpenAI error shape.
	code, out = e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"claude-rate","messages":[{"role":"user","content":"hi"}]}`)
	if code != 429 || !strings.Contains(out, `"message":"slow down"`) || !strings.Contains(out, `"type":"rate_limit_error"`) {
		t.Fatalf("rate limit: %d %s", code, out)
	}

	// A field the gateway cannot carry: a clear 400, and the upstream never sees it.
	seen := func() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.seen) }
	n := seen()
	code, out = e.do(e.gw.URL, "POST", "/v1/chat/completions", secret, `{"model":"sonnet","n":2,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 400 || !strings.Contains(out, "unsupported_parameter") || seen() != n {
		t.Fatalf("n=2: %d %s", code, out)
	}
	code, out = e.do(e.gw.URL, "POST", "/v1/embeddings", secret, `{"model":"sonnet","input":"x"}`)
	if code != 400 || !strings.Contains(out, "unsupported_endpoint") {
		t.Fatalf("embeddings: %d %s", code, out)
	}
}
