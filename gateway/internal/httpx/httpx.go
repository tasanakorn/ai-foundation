// Package httpx has small HTTP helpers shared by the public and admin APIs.
package httpx

import (
	"encoding/json"
	"net/http"
	"strings"
)

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes an OpenAI-style error so existing SDKs can parse it.
func WriteError(w http.ResponseWriter, status int, typ, code, msg string) {
	WriteJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": code},
	})
}

// BearerToken reads "Authorization: Bearer <t>", falling back to "x-api-key".
func BearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return ""
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}
