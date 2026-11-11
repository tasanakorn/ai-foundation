// Package gateway is the public, OpenAI-compatible API.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"ai-foundation/gateway/internal/hook"
	"ai-foundation/gateway/internal/httpx"
	"ai-foundation/gateway/internal/id"
	"ai-foundation/gateway/internal/keys"
	"ai-foundation/gateway/internal/metering"
	"ai-foundation/gateway/internal/router"
	"ai-foundation/gateway/internal/store"
)

// EndUserHeader lets an app tag a request with its own end-user ID.
// It wins over the OpenAI "user" body field.
const EndUserHeader = "X-AIF-End-User"

const (
	maxResponseBytes = 64 << 20
	maxErrorText     = 512
)

type Handler struct {
	router  *router.Router
	keys    *keys.Service
	rec     *metering.Recorder
	hook    hook.Hook
	ready   func() error
	log     *slog.Logger
	maxBody int64
	timeout time.Duration
}

type Options struct {
	Router          *router.Router
	Keys            *keys.Service
	Recorder        *metering.Recorder
	Hook            hook.Hook    // optional pre-request hook, already wrapped with hook.Guard
	Ready           func() error // optional; a non-nil error makes /healthz answer 503
	Log             *slog.Logger
	MaxBodyBytes    int64
	UpstreamTimeout time.Duration
}

func New(o Options) *Handler {
	return &Handler{router: o.Router, keys: o.Keys, rec: o.Recorder, hook: o.Hook, ready: o.Ready, log: o.Log,
		maxBody: o.MaxBodyBytes, timeout: o.UpstreamTimeout}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if h.ready != nil {
			if err := h.ready(); err != nil {
				httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": err.Error()})
				return
			}
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("GET /v1/models", h.auth(h.listModels))
	mux.Handle("POST /v1/chat/completions", h.auth(h.forward("/chat/completions")))
	mux.Handle("POST /v1/completions", h.auth(h.forward("/completions")))
	mux.Handle("POST /v1/embeddings", h.auth(h.forward("/embeddings")))
	return mux
}

// ---- auth ----

type ctxKey struct{}

func keyFrom(ctx context.Context) *store.Key { return ctx.Value(ctxKey{}).(*store.Key) }

func (h *Handler) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k, err := h.keys.Authenticate(r.Context(), httpx.BearerToken(r))
		switch {
		case errors.Is(err, keys.ErrInvalid):
			httpx.WriteError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "invalid or missing API key")
			return
		case err != nil:
			h.log.Error("key lookup failed", "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, "server_error", "internal_error", "key lookup failed")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, k)))
	})
}

// ---- models ----

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	k := keyFrom(r.Context())
	data := []map[string]any{}
	for _, m := range h.router.Models() {
		if keys.Allows(k, m.ID) {
			data = append(data, map[string]any{"id": m.ID, "object": "model", "created": 0, "owned_by": m.Provider})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// ---- proxy ----

type apiError struct {
	status         int
	typ, code, msg string
}

func (e *apiError) write(w http.ResponseWriter) { httpx.WriteError(w, e.status, e.typ, e.code, e.msg) }

func badRequest(code, msg string) *apiError {
	return &apiError{http.StatusBadRequest, "invalid_request_error", code, msg}
}

// prepared is a request body after parsing, routing and the key's allow-list check.
type prepared struct {
	req    map[string]json.RawMessage // top level only; unknown fields pass through untouched
	model  string
	route  router.Route
	stream bool
	size   int // body bytes, for the token estimate when usage is missing
}

func (h *Handler) prepare(k *store.Key, body []byte) (*prepared, *apiError) {
	p := prepared{size: len(body)}
	if err := json.Unmarshal(body, &p.req); err != nil || p.req == nil {
		return nil, badRequest("invalid_json", "request body must be a JSON object")
	}
	p.model = stringField(p.req, "model")
	if p.model == "" {
		return nil, badRequest("missing_model", "model is required")
	}
	var ok bool
	if p.route, ok = h.router.Resolve(p.model); !ok {
		return nil, &apiError{http.StatusNotFound, "invalid_request_error", "model_not_found", "model " + p.model + " does not exist"}
	}
	if !keys.Allows(k, p.model) {
		return nil, &apiError{http.StatusForbidden, "invalid_request_error", "model_not_allowed", "this key may not use model " + p.model}
	}
	p.stream = boolField(p.req, "stream")
	return &p, nil
}

func (h *Handler) forward(endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		k := keyFrom(r.Context())
		reqID := id.New("req_")
		w.Header().Set("X-Request-Id", reqID)

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				httpx.WriteError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large", "request body too large")
				return
			}
			httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "bad_body", "cannot read request body")
			return
		}
		p, aerr := h.prepare(k, body)
		if aerr != nil {
			aerr.write(w) // client mistakes are not metered
			return
		}

		rec := store.UsageRecord{
			ID: reqID, CreatedAt: start.UTC(), KeyID: k.ID, TenantID: k.TenantID, AppID: k.AppID,
			EndUser: r.Header.Get(EndUserHeader), Endpoint: endpoint,
		}
		if rec.EndUser == "" {
			rec.EndUser = stringField(p.req, "user")
		}
		setRoute := func(p *prepared) {
			rec.Model, rec.Provider, rec.UpstreamModel, rec.Stream = p.model, p.route.Provider.Name(), p.route.Upstream, p.stream
		}
		setRoute(p)
		defer func() {
			rec.LatencyMS = time.Since(start).Milliseconds()
			h.rec.Record(rec)
			h.log.Info("request", "id", reqID, "key", k.ID, "model", rec.Model, "provider", rec.Provider,
				"status", rec.StatusCode, "stream", rec.Stream, "tokens", rec.TotalTokens, "estimated", rec.Estimated,
				"latency_ms", rec.LatencyMS)
		}()

		if h.hook != nil {
			if p, aerr = h.runHook(r.Context(), k, &rec, endpoint, body, p); aerr != nil {
				rec.StatusCode, rec.Error = aerr.status, aerr.code+": "+aerr.msg
				aerr.write(w)
				return
			}
			setRoute(p)
		}

		p.req["model"], _ = json.Marshal(p.route.Upstream)
		if p.stream && endpoint != "/embeddings" {
			injectIncludeUsage(p.req)
		}
		out, _ := json.Marshal(p.req)

		ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
		defer cancel()
		resp, err := p.route.Provider.Do(ctx, endpoint, out)
		if err != nil {
			rec.StatusCode, rec.Error = http.StatusBadGateway, err.Error()
			switch {
			case r.Context().Err() != nil:
				rec.StatusCode = 499 // client closed request
				return
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				rec.StatusCode = http.StatusGatewayTimeout
			}
			httpx.WriteError(w, rec.StatusCode, "upstream_error", "upstream_unavailable", "upstream provider request failed")
			return
		}
		defer resp.Body.Close()

		isSSE := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
		if p.stream && resp.StatusCode == http.StatusOK && isSSE {
			h.pipeStream(r.Context(), w, resp, &rec, p.route, p.size)
		} else {
			h.pipeBody(w, resp, &rec, p.route, p.size)
		}
	}
}

// runHook asks the pre-request hook. It returns the request to send: the same one,
// or the hook's replacement (parsed, routed and allow-list checked again).
func (h *Handler) runHook(ctx context.Context, k *store.Key, rec *store.UsageRecord, endpoint string, body []byte, p *prepared) (*prepared, *apiError) {
	d, err := h.hook.PreRequest(ctx, &hook.Request{
		RequestID: rec.ID,
		Key:       hook.Key{ID: k.ID, TenantID: k.TenantID, AppID: k.AppID, Metadata: k.Metadata},
		EndUser:   rec.EndUser, Endpoint: endpoint,
		Model: p.model, Provider: rec.Provider, UpstreamModel: rec.UpstreamModel, Stream: p.stream,
		Body: body,
	})
	if err != nil {
		return nil, &apiError{http.StatusServiceUnavailable, "server_error", "hook_unavailable", "request policy check is unavailable, try again later"}
	}
	switch d.Action {
	case hook.Deny:
		return nil, &apiError{d.Status, "invalid_request_error", d.Code, d.Message}
	case hook.Modify:
		return h.prepare(k, d.Body)
	}
	return p, nil
}

type choice struct {
	Text    json.RawMessage `json:"text"`
	Message *delta          `json:"message"`
	Delta   *delta          `json:"delta"`
}

type delta struct {
	Content          json.RawMessage `json:"content"`
	ReasoningContent json.RawMessage `json:"reasoning_content"`
	Reasoning        json.RawMessage `json:"reasoning"`
	ToolCalls        json.RawMessage `json:"tool_calls"`
}

type responseShape struct {
	Usage   *metering.Usage `json:"usage"`
	Choices []choice        `json:"choices"`
}

// outputBytes is a rough size of generated output, used only when usage is missing.
func (c responseShape) outputBytes() int {
	n := 0
	for _, ch := range c.Choices {
		n += rawLen(ch.Text)
		for _, d := range []*delta{ch.Message, ch.Delta} {
			if d != nil {
				n += rawLen(d.Content) + rawLen(d.ReasoningContent) + rawLen(d.Reasoning) + rawLen(d.ToolCalls)
			}
		}
	}
	return n
}

func (h *Handler) pipeBody(w http.ResponseWriter, resp *http.Response, rec *store.UsageRecord, route router.Route, promptBytes int) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	rec.StatusCode = resp.StatusCode
	if err != nil {
		rec.StatusCode, rec.Error = http.StatusBadGateway, err.Error()
		httpx.WriteError(w, http.StatusBadGateway, "upstream_error", "upstream_read_failed", "failed reading upstream response")
		return
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)

	if resp.StatusCode >= 400 {
		rec.Error = truncate(string(data), maxErrorText)
		return
	}
	var shape responseShape
	if json.Unmarshal(data, &shape) == nil && shape.Usage != nil {
		metering.Apply(rec, shape.Usage, route.Price)
		return
	}
	metering.Estimate(rec, promptBytes, shape.outputBytes(), route.Price)
}

func (h *Handler) pipeStream(ctx context.Context, w http.ResponseWriter, resp *http.Response, rec *store.UsageRecord, route router.Route, promptBytes int) {
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rec.StatusCode = http.StatusOK
	flusher, _ := w.(http.Flusher)

	var usage *metering.Usage
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
				payload := bytes.TrimSpace(trimmed[5:])
				if !bytes.Equal(payload, []byte("[DONE]")) {
					var chunk responseShape
					if json.Unmarshal(payload, &chunk) == nil {
						if chunk.Usage != nil {
							usage = chunk.Usage
						}
						outBytes += chunk.outputBytes()
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
	if usage != nil {
		metering.Apply(rec, usage, route.Price)
	} else {
		metering.Estimate(rec, promptBytes, outBytes, route.Price)
	}
}

// injectIncludeUsage asks the upstream to send a final usage chunk in streams.
func injectIncludeUsage(req map[string]json.RawMessage) {
	so := map[string]json.RawMessage{}
	if raw, ok := req["stream_options"]; ok {
		if json.Unmarshal(raw, &so) != nil || so == nil {
			so = map[string]json.RawMessage{}
		}
	}
	so["include_usage"] = json.RawMessage("true")
	req["stream_options"], _ = json.Marshal(so)
}

func stringField(m map[string]json.RawMessage, k string) string {
	var s string
	if raw, ok := m[k]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

func boolField(m map[string]json.RawMessage, k string) bool {
	var b bool
	if raw, ok := m[k]; ok {
		_ = json.Unmarshal(raw, &b)
	}
	return b
}

func rawLen(r json.RawMessage) int {
	if len(r) == 0 || bytes.Equal(r, []byte("null")) {
		return 0
	}
	return len(r)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
