package admin

import (
	"net/http"
	"strings"

	"ai-foundation/gateway/internal/httpx"
)

// PlaygroundKeyHeader carries the gateway API key (sk-aif-…) the playground calls with.
const PlaygroundKeyHeader = "X-AIF-Key"

// playground runs a /v1 request in-process for a signed-in admin, with a real API key.
// It goes through the same path as any client: key auth, allow-list, hook, upstream,
// streaming and metering. Same origin as the UI, so no CORS on the public port.
func (h *Handler) playground(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get(PlaygroundKeyHeader))
	if key == "" {
		httpx.WriteError(w, http.StatusBadRequest, "invalid_request_error", "missing_api_key",
			"send a gateway API key (sk-aif-…) in the "+PlaygroundKeyHeader+" header")
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/v1/" + strings.TrimPrefix(r.URL.Path, "/admin/v1/playground/")
	r2.URL.RawPath = ""
	r2.RequestURI = ""
	// Only the API key reaches the public handler: never the admin cookie or master key.
	r2.Header.Del("Cookie")
	r2.Header.Del("X-Api-Key")
	r2.Header.Del(PlaygroundKeyHeader)
	r2.Header.Set("Authorization", "Bearer "+key)
	h.public.ServeHTTP(w, r2)
}
