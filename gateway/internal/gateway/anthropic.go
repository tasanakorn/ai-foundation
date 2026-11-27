package gateway

// Anthropic Messages API passthrough (spec-0002 section 6).
//
// Clients such as Claude Code and the Anthropic SDKs set their base URL to
// https://<gateway>/anthropic. The body goes upstream byte for byte, except the
// value of the top-level "model" field when the alias differs from the upstream
// name, because any other rewrite breaks prompt caching and preserved thinking.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"ai-foundation/gateway/internal/httpx"
	"ai-foundation/gateway/internal/id"
	"ai-foundation/gateway/internal/keys"
	"ai-foundation/gateway/internal/metering"
	"ai-foundation/gateway/internal/provider"
	"ai-foundation/gateway/internal/store"
)

// SessionHeader is the coding-session ID that Claude Code sends; it is stored on usage records.
const SessionHeader = "X-Claude-Code-Session-Id"

// anthropicRoutes registers the Anthropic API under pfx ("" = at the root, for the dedicated listener).
func (h *Handler) anthropicRoutes(mux *http.ServeMux, pfx string) {
	mux.Handle("POST "+pfx+"/v1/messages", h.auth(formatAnthropic, h.messages("/messages")))
	mux.Handle("POST "+pfx+"/v1/messages/count_tokens", h.auth(formatAnthropic, h.messages("/messages/count_tokens")))
	mux.Handle("GET "+pfx+"/v1/models", h.auth(formatAnthropic, h.listAnthropicModels))
	mux.HandleFunc(pfx+"/", func(w http.ResponseWriter, r *http.Request) {
		msg := "not found: " + r.Method + " " + r.URL.Path
		if strings.HasPrefix(strings.TrimPrefix(r.URL.Path, pfx), "/v1/v1/") {
			msg += `; the base URL must not end in /v1 (use "` + pfx + `", the client adds /v1)`
		}
		writeAnthropicError(w, http.StatusNotFound, msg)
	})
}

func (h *Handler) listAnthropicModels(w http.ResponseWriter, r *http.Request) {
	k := keyFrom(r.Context())
	data := []map[string]any{}
	var first, last any
	for _, m := range h.router.Models() {
		if !keys.Allows(k, m.ID) || !slices.Contains(m.Formats, formatAnthropic) {
			continue
		}
		data = append(data, map[string]any{"type": "model", "id": m.ID, "display_name": m.ID, "created_at": "1970-01-01T00:00:00Z"})
		if first == nil {
			first = m.ID
		}
		last = m.ID
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": data, "has_more": false, "first_id": first, "last_id": last})
}

// prepareAnthropic parses only what routing needs. p.body stays the client's bytes.
func (h *Handler) prepareAnthropic(k *store.Key, body []byte) (*prepared, *apiError) {
	p := prepared{size: len(body), body: body}
	if err := json.Unmarshal(body, &p.req); err != nil || p.req == nil {
		return nil, badRequest("invalid_json", "request body must be a JSON object")
	}
	p.model = stringField(p.req, "model")
	if p.model == "" {
		return nil, badRequest("missing_model", "model: field required")
	}
	var ok bool
	if p.route, ok = h.router.Resolve(p.model); !ok || !p.route.Serves(formatAnthropic) {
		return nil, &apiError{http.StatusNotFound, "invalid_request_error", "model_not_found", "model " + p.model + " does not exist"}
	}
	if n, ok := p.route.Provider.(provider.Native); !ok || n.NativeFormat() != formatAnthropic {
		return nil, &apiError{http.StatusNotFound, "invalid_request_error", "model_not_found", "model " + p.model + " is not served in the Anthropic format"}
	}
	if !keys.Allows(k, p.model) {
		return nil, &apiError{http.StatusForbidden, "invalid_request_error", "model_not_allowed", "this key may not use model " + p.model}
	}
	p.stream = boolField(p.req, "stream")
	return &p, nil
}

func (h *Handler) messages(endpoint string) http.HandlerFunc {
	countTokens := endpoint == "/messages/count_tokens"
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		k := keyFrom(r.Context())
		reqID := id.New("req_")
		w.Header().Set("X-Request-Id", reqID)

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request body too large")
				return
			}
			writeAnthropicError(w, http.StatusBadRequest, "cannot read request body")
			return
		}
		p, aerr := h.prepareAnthropic(k, body)
		if aerr != nil {
			aerr.writeAs(w, formatAnthropic) // client mistakes are not metered
			return
		}

		rec := store.UsageRecord{
			ID: reqID, CreatedAt: start.UTC(), KeyID: k.ID, TenantID: k.TenantID, AppID: k.AppID,
			EndUser: r.Header.Get(EndUserHeader), Endpoint: endpoint, Format: formatAnthropic,
			SessionID: truncate(r.Header.Get(SessionHeader), 128),
		}
		setRoute := func(p *prepared) {
			rec.Model, rec.Provider, rec.UpstreamModel, rec.Stream = p.model, p.route.Provider.Name(), p.route.Upstream, p.stream
		}
		setRoute(p)
		defer func() {
			rec.LatencyMS = time.Since(start).Milliseconds()
			h.rec.Record(rec)
			h.log.Info("request", "id", reqID, "key", k.ID, "format", formatAnthropic, "endpoint", endpoint, "model", rec.Model,
				"provider", rec.Provider, "status", rec.StatusCode, "stream", rec.Stream, "tokens", rec.TotalTokens,
				"cached", rec.CachedTokens, "estimated", rec.Estimated, "latency_ms", rec.LatencyMS)
		}()

		// count_tokens is free and changes nothing, so it skips the hook.
		if h.hook != nil && !countTokens {
			if p, aerr = h.runHook(r.Context(), k, &rec, endpoint, body, p, formatAnthropic); aerr != nil {
				rec.StatusCode, rec.Error = aerr.status, aerr.code+": "+aerr.msg
				aerr.writeAs(w, formatAnthropic)
				return
			}
			setRoute(p)
		}

		out := p.body
		if p.route.Upstream != p.model {
			if out, err = setModel(p.body, p.route.Upstream); err != nil {
				rec.StatusCode, rec.Error = http.StatusBadRequest, "set model: "+err.Error()
				writeAnthropicError(w, http.StatusBadRequest, "request body must be a JSON object")
				return
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
		defer cancel()
		native := p.route.Provider.(provider.Native)
		resp, err := native.Native(ctx, http.MethodPost, endpoint, r.URL.RawQuery, forwardHeaders(r.Header), out)
		if err != nil {
			rec.StatusCode, rec.Error = http.StatusBadGateway, err.Error()
			switch {
			case r.Context().Err() != nil:
				rec.StatusCode = 499 // client closed request
				return
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				rec.StatusCode = http.StatusGatewayTimeout
			}
			writeAnthropicError(w, rec.StatusCode, "upstream provider request failed")
			return
		}
		defer resp.Body.Close()

		copyResponseHeaders(w.Header(), resp.Header)
		isSSE := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
		if resp.StatusCode == http.StatusOK && isSSE {
			h.pipeAnthropicStream(r.Context(), w, resp, &rec, p)
			return
		}
		h.pipeAnthropicBody(w, resp, &rec, p, countTokens)
	}
}

func (h *Handler) pipeAnthropicBody(w http.ResponseWriter, resp *http.Response, rec *store.UsageRecord, p *prepared, countTokens bool) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	rec.StatusCode = resp.StatusCode
	if err != nil {
		rec.StatusCode, rec.Error = http.StatusBadGateway, err.Error()
		writeAnthropicError(w, http.StatusBadGateway, "failed reading upstream response")
		return
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)

	switch {
	case resp.StatusCode >= 400:
		rec.Error = truncate(string(data), maxErrorText)
	case countTokens:
		// Not billed by Anthropic: logged with zero tokens and cost (spec-0002 section 6.4.7).
	default:
		var msg struct {
			Usage   *metering.AnthropicUsage `json:"usage"`
			Content json.RawMessage          `json:"content"`
		}
		if json.Unmarshal(data, &msg) == nil && msg.Usage != nil {
			metering.ApplyAnthropic(rec, *msg.Usage, p.route.Price)
			return
		}
		metering.Estimate(rec, p.size, rawLen(msg.Content), p.route.Price)
	}
}

// anthropicEvent holds the fields of a stream event that metering reads.
type anthropicEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Usage *metering.AnthropicUsage `json:"usage"`
	} `json:"message"`
	Usage *metering.AnthropicUsage `json:"usage"`
	Delta json.RawMessage          `json:"delta"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// pipeAnthropicStream sends each line as it arrives and flushes after each event.
// It reads usage from message_start and message_delta without changing any byte.
func (h *Handler) pipeAnthropicStream(ctx context.Context, w http.ResponseWriter, resp *http.Response, rec *store.UsageRecord, p *prepared) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rec.StatusCode = http.StatusOK
	flusher, _ := w.(http.Flusher)

	var usage metering.AnthropicUsage
	gotUsage := false
	outBytes := 0
	br := bufio.NewReaderSize(resp.Body, 32<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				rec.StatusCode, rec.Error = 499, "client disconnected"
				break
			}
			trimmed := bytes.TrimSpace(line)
			switch {
			case len(trimmed) == 0: // end of one SSE event
				if flusher != nil {
					flusher.Flush()
				}
			case bytes.HasPrefix(trimmed, []byte("data:")):
				var ev anthropicEvent
				if json.Unmarshal(bytes.TrimSpace(trimmed[5:]), &ev) != nil {
					break
				}
				switch ev.Type {
				case "message_start":
					if ev.Message != nil && ev.Message.Usage != nil {
						usage, gotUsage = *ev.Message.Usage, true
					}
				case "message_delta":
					if ev.Usage != nil {
						usage.Merge(*ev.Usage)
						gotUsage = true
					}
				case "content_block_delta":
					outBytes += rawLen(ev.Delta)
				case "error":
					if ev.Error != nil {
						rec.Error = truncate(ev.Error.Type+": "+ev.Error.Message, maxErrorText)
					}
				}
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				if ctx.Err() != nil {
					rec.StatusCode, rec.Error = 499, "client disconnected"
				} else {
					rec.Error = "upstream stream error: " + err.Error()
				}
			}
			break
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	if gotUsage {
		metering.ApplyAnthropic(rec, usage, p.route.Price)
	} else {
		metering.Estimate(rec, p.size, outBytes, p.route.Price)
	}
}

// forwardHeaders picks the client headers that go upstream (spec-0002 section 6.3.2):
// anthropic-*, user-agent and x-claude-code-*. The client's Authorization and x-api-key
// carry the aif key and never leave the gateway. anthropic-workspace-id is also dropped:
// a client must not pick a workspace in the gateway's Anthropic org.
func forwardHeaders(in http.Header) http.Header {
	out := http.Header{}
	for k, vs := range in {
		lk := strings.ToLower(k)
		switch {
		case lk == "anthropic-workspace-id":
		case strings.HasPrefix(lk, "anthropic-"), lk == "user-agent", strings.HasPrefix(lk, "x-claude-code-"):
			out[k] = append([]string(nil), vs...)
		}
	}
	return out
}

// hopHeaders are per-connection headers that a proxy must not copy (RFC 9110 section 7.6.1).
var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Transfer-Encoding": true,
	"Te": true, "Trailer": true, "Upgrade": true, "Content-Length": true,
}

// copyResponseHeaders copies upstream headers unchanged (request-id, retry-after, x-should-retry,
// anthropic-ratelimit-*, content-type, ...). Content-Length is dropped: Go sets it or streams.
func copyResponseHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

// writeAnthropicError writes the Anthropic error envelope, which Anthropic SDKs and Claude Code parse.
func writeAnthropicError(w http.ResponseWriter, status int, msg string) {
	httpx.WriteJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": anthropicErrorType(status), "message": msg},
	})
}

func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusPaymentRequired:
		return "billing_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

// setModel replaces the value of the top-level "model" field and keeps every other byte.
// With duplicate keys it replaces the last one, the one encoding/json reads.
func setModel(body []byte, model string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	start, end := -1, -1
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		keyEnd := int(dec.InputOffset())
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if key, _ := t.(string); key == "model" {
			start, end = keyEnd, int(dec.InputOffset())
			for start < end && (body[start] == ':' || body[start] == ' ' || body[start] == '\t' || body[start] == '\n' || body[start] == '\r') {
				start++
			}
		}
	}
	if start < 0 {
		return nil, errors.New("no model field")
	}
	var val bytes.Buffer
	enc := json.NewEncoder(&val)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(model); err != nil {
		return nil, err
	}
	v := bytes.TrimRight(val.Bytes(), "\n")
	out := make([]byte, 0, len(body)-(end-start)+len(v))
	out = append(out, body[:start]...)
	out = append(out, v...)
	return append(out, body[end:]...), nil
}
