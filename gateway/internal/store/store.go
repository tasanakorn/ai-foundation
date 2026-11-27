// Package store defines the persistence contract. Implementations live in sub-packages.
package store

import (
	"context"
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
	// ErrStale means a catalog change was based on an older revision (spec-0006 section 8.3).
	ErrStale = errors.New("catalog revision changed")
)

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
	// Added in spec-0002. Token fields keep the OpenAI meaning for every format (D20):
	// PromptTokens includes cached and cache-write tokens.
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	Format           string `json:"format"`               // API format the client used: openai or anthropic
	SessionID        string `json:"session_id,omitempty"` // x-claude-code-session-id, when sent
	// Added in spec-0004: the provider account that answered, and upstream calls made.
	Account  string `json:"account"`
	Attempts int    `json:"attempts"`
}

type UsageFilter struct {
	From     time.Time // inclusive; zero = no bound
	To       time.Time // exclusive; zero = no bound
	ID       string    // one request (X-Request-Id)
	KeyID    string
	TenantID string
	AppID    string
	EndUser  string
	Model    string
	Provider string
	Account  string
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
var GroupBy = []string{"none", "day", "model", "provider", "account", "key", "tenant", "app", "end_user"}

// Admin roles. RoleAdmin can change everything; RoleViewer can only read.
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// AdminUser is an admin account. One of them, the local account (D42), has a password;
// the others sign in through OIDC (AdminIdentity).
type AdminUser struct {
	ID           string     `json:"id"`
	Email        string     `json:"email"` // stored lowercase; unique
	Name         string     `json:"name"`
	Role         string     `json:"role"`
	PasswordHash string     `json:"-"` // argon2id PHC string; used only for the local account
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
	DisabledAt   *time.Time `json:"disabled_at,omitempty"`
	// LinkAllowedUntil: until then, the next OIDC login with this email (verified) links the account.
	LinkAllowedUntil *time.Time `json:"link_allowed_until,omitempty"`
	Local            bool       `json:"local"` // the local account; set by auth, not stored
}

// AdminSession is a browser login. Hash is the SHA-256 of the cookie value; the value itself is never stored.
type AdminSession struct {
	Hash      string
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
	UserAgent string
	IP        string
}

// AdminIdentity links an OIDC login (issuer + subject) to an admin account (spec-0001 section 6.3).
type AdminIdentity struct {
	ID          string     `json:"id"`
	UserID      string     `json:"user_id"`
	Provider    string     `json:"provider"` // config name of the IdP when it was linked
	Issuer      string     `json:"issuer"`
	Subject     string     `json:"subject"`
	Email       string     `json:"email"` // as the IdP reported it at the last login
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// OIDCFlow is one OIDC sign-in in progress. IDHash is the SHA-256 of the flow cookie.
type OIDCFlow struct {
	IDHash       string
	Provider     string
	State        string
	Nonce        string
	PKCEVerifier string
	LinkUserID   string // set when a signed-in user links another IdP
	ExpiresAt    time.Time
}

// Catalog is the routing catalog in the database (spec-0006): providers with their
// accounts, models with their targets, and passthrough. Order matters (priority
// policies, spec-0004), so slices keep the order they were written in.
//
// Keys are never stored as values here: KeyRef is a reference such as "env:NAME" or
// "file:/run/secrets/key" (spec-0006 section 4.4).
type Catalog struct {
	Revision    int64             `json:"revision"` // 0 = never written
	Passthrough bool              `json:"passthrough"`
	Providers   []CatalogProvider `json:"providers"`
	Models      []CatalogModel    `json:"models"`
	UpdatedAt   time.Time         `json:"updated_at"`
	UpdatedBy   string            `json:"updated_by"`
}

// Empty reports whether the catalog has no providers and no models.
func (c *Catalog) Empty() bool { return len(c.Providers) == 0 && len(c.Models) == 0 }

type CatalogProvider struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	BaseURL      string            `json:"base_url"`
	Headers      map[string]string `json:"headers"`
	WorkspaceID  string            `json:"workspace_id"`
	OpenAIFormat string            `json:"openai_format"`
	Accounts     []CatalogAccount  `json:"accounts"`
	// Account selection (spec-0004); empty or zero = the default.
	Policy      string        `json:"policy"`
	Sticky      string        `json:"sticky"`
	MaxAttempts int           `json:"max_attempts"`
	Cooldown    time.Duration `json:"cooldown"`
}

// CatalogAccount holds its key as a reference (KeyRef) or encrypted (KeyCipher), never as a value.
type CatalogAccount struct {
	Name   string `json:"name"`
	KeyRef string `json:"key_ref"` // "" (no key), "env:NAME" or "file:/path"
	// Encrypted key: AES-256-GCM (nonce first), the ID of the secret key that sealed it,
	// and the last 4 characters for display. The ciphertext is never sent out.
	KeyCipher []byte `json:"-"`
	KeyID     string `json:"key_id,omitempty"`
	KeyHint   string `json:"key_hint,omitempty"`
	// round_robin weight (nil = default 1) and overrides of the provider's settings.
	Weight      *int              `json:"weight,omitempty"`
	BaseURL     string            `json:"base_url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	WorkspaceID string            `json:"workspace_id,omitempty"`
	Credit      *Credit           `json:"credit,omitempty"` // how the account is paid for; nil = not tracked (task-0010)
}

// Credit is an account's credit type and its cycle (same fields as config.Credit).
type Credit struct {
	Type     string  `json:"type"` // monthly, prepaid or payg
	USD      float64 `json:"usd,omitempty"`
	CycleDay int     `json:"cycle_day,omitempty"`
	Timezone string  `json:"timezone,omitempty"`
}

// CreditEntry is one line of a prepaid account's ledger: a top-up (USD above 0) or a
// correction (either sign). The ledger is append-only and lives only in the database;
// it is not part of the catalog. Accounts are matched by provider and account name.
type CreditEntry struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	Account   string    `json:"account"`
	USD       float64   `json:"usd"`
	Note      string    `json:"note"`
	At        time.Time `json:"at"` // when the money arrived; spend counts from the first entry
	CreatedAt time.Time `json:"created_at"`
	Actor     string    `json:"actor"` // admin user ID, "master_key" or "cli"
}

type CatalogModel struct {
	Name    string          `json:"name"`
	Formats []string        `json:"formats"`
	Targets []CatalogTarget `json:"targets"`
	// Target selection (spec-0005); empty or zero = the default.
	Policy      string `json:"policy"`
	Sticky      string `json:"sticky"`
	MaxAttempts int    `json:"max_attempts"`
}

type CatalogTarget struct {
	Provider string   `json:"provider"`
	Upstream string   `json:"upstream"`
	Price    *Price   `json:"price,omitempty"`
	Weight   *int     `json:"weight,omitempty"` // round_robin; nil = 1
	Formats  []string `json:"formats"`          // empty = what the provider type serves
}

// Price is in USD per 1M tokens (same fields as config.Price).
type Price struct {
	Input       float64 `json:"input"`
	Output      float64 `json:"output"`
	CachedInput float64 `json:"cached_input"`
	CacheWrite  float64 `json:"cache_write,omitempty"`
}

// CatalogChange is one committed catalog revision (the audit trail, spec-0006 section 4.3).
type CatalogChange struct {
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	Actor     string    `json:"actor"` // admin user ID, "master_key", "file" or "seed"
	Summary   string    `json:"summary"`
}

// AnyRevision makes UpdateCatalog skip the revision check.
const AnyRevision int64 = -1

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

	CreateAdminUser(ctx context.Context, u *AdminUser) error // ErrConflict when the email exists
	GetAdminUser(ctx context.Context, id string) (*AdminUser, error)
	GetAdminUserByEmail(ctx context.Context, email string) (*AdminUser, error)
	ListAdminUsers(ctx context.Context) ([]AdminUser, error)
	UpdateAdminUser(ctx context.Context, u *AdminUser) error // name, role, password_hash, disabled_at, updated_at
	DeleteAdminUser(ctx context.Context, id string) error
	CountActiveAdmins(ctx context.Context) (int, error)
	SetAdminLastLogin(ctx context.Context, id string, at time.Time) error

	CreateAdminSession(ctx context.Context, s *AdminSession) error
	GetAdminSession(ctx context.Context, hash string) (*AdminSession, error) // ErrNotFound when missing or expired
	DeleteAdminSession(ctx context.Context, hash string) error
	DeleteAdminSessions(ctx context.Context, userID, exceptHash string) error
	DeleteExpiredAdminSessions(ctx context.Context, now time.Time) error

	// AllowAdminLink sets (until != nil) or clears the one-time link allowance.
	AllowAdminLink(ctx context.Context, userID string, until *time.Time) error
	// ClaimAdminLink clears the allowance and reports whether it was still open at now (one use).
	ClaimAdminLink(ctx context.Context, userID string, now time.Time) (bool, error)
	CreateAdminIdentity(ctx context.Context, i *AdminIdentity) error // ErrConflict when issuer + subject is linked
	GetAdminIdentity(ctx context.Context, issuer, subject string) (*AdminIdentity, error)
	ListAdminIdentities(ctx context.Context, userID string) ([]AdminIdentity, error)
	DeleteAdminIdentity(ctx context.Context, userID, id string) error // ErrNotFound when not that user's
	SetAdminIdentityLogin(ctx context.Context, id, email string, at time.Time) error

	// CreateOIDCFlow stores a flow and deletes expired ones.
	CreateOIDCFlow(ctx context.Context, f *OIDCFlow) error
	// TakeOIDCFlow deletes and returns a flow that has not expired (one use).
	TakeOIDCFlow(ctx context.Context, idHash string, now time.Time) (*OIDCFlow, error)

	// GetCatalog returns the whole catalog as one consistent read.
	GetCatalog(ctx context.Context) (*Catalog, error)
	CatalogRevision(ctx context.Context) (int64, error)
	// UpdateCatalog changes the catalog in one transaction. It locks the catalog, checks
	// that the revision is still ifRevision (else ErrStale; AnyRevision skips the check),
	// and calls fn with the current catalog. fn edits it in place and returns an error to
	// abort (for example a failed validation): then nothing is written. Otherwise the
	// whole catalog is written, the revision goes up by one and a change row is added.
	UpdateCatalog(ctx context.Context, ifRevision int64, actor, summary string, fn func(*Catalog) error) (*Catalog, error)
	ListCatalogChanges(ctx context.Context, limit int) ([]CatalogChange, error)

	AddCreditEntry(ctx context.Context, e *CreditEntry) error
	// ListCreditEntries returns the ledger of one account (both set) or of all
	// accounts (both empty), newest first.
	ListCreditEntries(ctx context.Context, provider, account string) ([]CreditEntry, error)

	Close() error
}
