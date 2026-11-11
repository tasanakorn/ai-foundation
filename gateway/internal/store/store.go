// Package store defines the persistence contract. Implementations live in sub-packages.
package store

import (
	"context"
	"errors"
	"time"
)

var ErrNotFound = errors.New("not found")

// Key is a gateway API key. The secret is never stored; only its SHA-256 hash.
//
// TenantID and AppID are free-form owner labels. They let one gateway serve
// many tenants, many apps per tenant, and (via UsageRecord.EndUser) many end users per app.
type Key struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Prefix        string            `json:"prefix"`
	Hash          string            `json:"-"`
	TenantID      string            `json:"tenant_id"`
	AppID         string            `json:"app_id"`
	AllowedModels []string          `json:"allowed_models"`
	Metadata      map[string]string `json:"metadata"`
	CreatedAt     time.Time         `json:"created_at"`
	ExpiresAt     *time.Time        `json:"expires_at,omitempty"`
	RevokedAt     *time.Time        `json:"revoked_at,omitempty"`
}

// Active reports whether the key can be used at time now.
func (k *Key) Active(now time.Time) bool {
	if k.RevokedAt != nil {
		return false
	}
	return k.ExpiresAt == nil || now.Before(*k.ExpiresAt)
}

type KeyFilter struct {
	TenantID       string
	AppID          string
	IncludeRevoked bool
	Limit          int
	Offset         int
}

// UsageRecord is one metered request. ID equals the request ID returned in X-Request-Id.
type UsageRecord struct {
	ID               string    `json:"id"`
	CreatedAt        time.Time `json:"created_at"`
	KeyID            string    `json:"key_id"`
	TenantID         string    `json:"tenant_id"`
	AppID            string    `json:"app_id"`
	EndUser          string    `json:"end_user"`
	Endpoint         string    `json:"endpoint"`
	Model            string    `json:"model"`
	Provider         string    `json:"provider"`
	UpstreamModel    string    `json:"upstream_model"`
	Stream           bool      `json:"stream"`
	StatusCode       int       `json:"status_code"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	CachedTokens     int64     `json:"cached_tokens"`
	ReasoningTokens  int64     `json:"reasoning_tokens"`
	CostUSD          float64   `json:"cost_usd"`
	Estimated        bool      `json:"estimated"`
	LatencyMS        int64     `json:"latency_ms"`
	Error            string    `json:"error,omitempty"`
}

type UsageFilter struct {
	From     time.Time // inclusive; zero = no bound
	To       time.Time // exclusive; zero = no bound
	KeyID    string
	TenantID string
	AppID    string
	EndUser  string
	Model    string
	Limit    int
	Offset   int
}

// UsageSummary is one row of an aggregated usage query.
type UsageSummary struct {
	Group            string  `json:"group"`
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// GroupBy values accepted by SummarizeUsage.
var GroupBy = []string{"none", "day", "model", "provider", "key", "tenant", "app", "end_user"}

// Store is the persistence contract. Add new methods here; keep them small and generic.
type Store interface {
	CreateKey(ctx context.Context, k *Key) error
	GetKey(ctx context.Context, id string) (*Key, error)
	GetKeyByHash(ctx context.Context, hash string) (*Key, error)
	ListKeys(ctx context.Context, f KeyFilter) ([]Key, error)
	RevokeKey(ctx context.Context, id string, at time.Time) error

	InsertUsage(ctx context.Context, recs []UsageRecord) error
	ListUsage(ctx context.Context, f UsageFilter) ([]UsageRecord, error)
	SummarizeUsage(ctx context.Context, f UsageFilter, groupBy string) ([]UsageSummary, error)

	Close() error
}
