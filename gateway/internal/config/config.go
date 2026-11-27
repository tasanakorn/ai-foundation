// Package config loads and validates the gateway configuration file.
//
// Infra settings live in the file. Providers and models (the catalog) are in the file
// today; spec-0006 moves them to the database, where the file seeds them.
// Keys and usage live in the database (they change at runtime).
package config

import (
	"encoding/base64"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"ai-foundation/gateway/internal/clientip"
)

type Config struct {
	Server      Server     `yaml:"server"`
	Admin       Admin      `yaml:"admin"`
	Database    Database   `yaml:"database"`
	Metering    Metering   `yaml:"metering"`
	NATS        NATS       `yaml:"nats"`
	Providers   []Provider `yaml:"providers"`
	Models      []Model    `yaml:"models"`
	Passthrough bool       `yaml:"passthrough"`
	Metrics     Metrics    `yaml:"metrics"`
	Catalog     Catalog    `yaml:"catalog"`
	// DevMode allows the known dev secrets (spec-0003 D23). Only aif.dev.yaml sets it.
	DevMode bool `yaml:"dev_mode"`

	// Path is the file Load read ("" for Parse). File mode watches it (spec-0006 section 5.4).
	Path string `yaml:"-"`
}

// Catalog says where providers and models come from (spec-0006 section 3).
type Catalog struct {
	// Mode: database (default; the file seeds an empty database once) or file (the file
	// is the source of truth and is written to the database at start and on change).
	Mode string `yaml:"mode"`
	// WatchInterval is how often file mode checks the file for changes; default 5s.
	WatchInterval time.Duration `yaml:"watch_interval"`
	// PollInterval is how often a node checks the database revision, in case a
	// catalog_changed event was missed; default 30s (spec-0006 section 5.3).
	PollInterval time.Duration `yaml:"poll_interval"`
	// SecretKeys encrypt provider keys stored in the database (spec-0006 section 4.4):
	// base64 of 32 random bytes each. The first encrypts new values; all of them decrypt,
	// so a new key goes first and old ones stay until every value is written again.
	// Empty: only key references (env:, file:) are accepted.
	SecretKeys []string `yaml:"secret_keys"`
}

// Catalog modes.
const (
	CatalogDatabase = "database"
	CatalogFile     = "file"
)

// Metrics is the Prometheus endpoint on its own listener (spec-0003 D22). Default "off".
type Metrics struct {
	Listen string `yaml:"listen"`
}

func (m Metrics) Enabled() bool { return m.Listen != "" && m.Listen != "off" }

// Known dev secrets from .env.example and scripts/dev.py, refused outside dev_mode.
var devSecrets = map[string]string{
	"admin.master_key":         "dev-master-key-0000",
	"admin.bootstrap.password": "dev-admin-password",
}

type Server struct {
	Listen          string          `yaml:"listen"`
	UpstreamTimeout time.Duration   `yaml:"upstream_timeout"`
	MaxBodyBytes    int64           `yaml:"max_body_bytes"`
	Anthropic       AnthropicServer `yaml:"anthropic"`
	// TrustedProxies are IPs or CIDRs allowed to set ClientIPHeader (spec-0003 D21).
	// Empty: the client IP is always the TCP peer.
	TrustedProxies []string `yaml:"trusted_proxies"`
	ClientIPHeader string   `yaml:"client_ip_header"` // default X-Forwarded-For
}

// AnthropicServer places the Anthropic Messages API (spec-0002 section 3).
type AnthropicServer struct {
	// PathPrefix on the main listener; default "/anthropic", "" turns it off there.
	// A pointer, so an explicit "" differs from "not set".
	PathPrefix *string `yaml:"path_prefix"`
	// Listen is an optional dedicated listener that serves the Anthropic API at its root. Default "off".
	Listen string `yaml:"listen"`
}

// Prefix returns the path prefix on the main listener ("" = off).
func (a AnthropicServer) Prefix() string {
	if a.PathPrefix == nil {
		return DefaultAnthropicPrefix
	}
	return *a.PathPrefix
}

// ListenEnabled reports whether the dedicated Anthropic listener is on.
func (a AnthropicServer) ListenEnabled() bool { return a.Listen != "" && a.Listen != "off" }

const DefaultAnthropicPrefix = "/anthropic"

// API formats a model can serve (spec-0002 section 4).
const (
	FormatOpenAI    = "openai"
	FormatAnthropic = "anthropic"
)

// Admin is the admin API (and optional web UI). It listens on localhost by default;
// any address works (e.g. "0.0.0.0:8081" for public access). "off" disables it.
//
// People sign in through an OIDC identity provider (spec-0001 section 6.3, D41), or with
// the one local account from Bootstrap (D42), the only account with a password.
// MasterKey is optional: a bearer token with the admin role, for scripts and emergency access.
type Admin struct {
	Listen       string         `yaml:"listen"`
	MasterKey    string         `yaml:"master_key"`
	UIDir        string         `yaml:"ui_dir"`
	SessionTTL   time.Duration  `yaml:"session_ttl"`   // browser login lifetime; default 12h
	CookieSecure string         `yaml:"cookie_secure"` // auto (HTTPS or X-Forwarded-Proto: https), always, never
	Bootstrap    AdminBootstrap `yaml:"bootstrap"`
	// PasswordLogin false turns off the local account's password even without OIDC; default
	// true. With any OIDC provider the password is off anyway (D43).
	PasswordLogin *bool `yaml:"password_login"`
	// PublicURL is the admin URL the IdP redirects back to (never taken from the Host header).
	PublicURL string    `yaml:"public_url"`
	OIDC      AdminOIDC `yaml:"oidc"`
}

// AdminBootstrap is the local account (break-glass, D42). It is read on every start: the
// account is created if missing, and its password follows Password or PasswordHash.
type AdminBootstrap struct {
	Email        string `yaml:"email"`
	Password     string `yaml:"password"`      // plain; fine for local development
	PasswordHash string `yaml:"password_hash"` // argon2id PHC string (aif admin hash-password); for production
	Name         string `yaml:"name"`
}

// AdminOIDC is sign-in through outside identity providers (spec-0001 section 6.3).
type AdminOIDC struct {
	SessionTTL time.Duration  `yaml:"session_ttl"` // sessions from an OIDC login; default admin.session_ttl
	Providers  []OIDCProvider `yaml:"providers"`
	// AutoRedirect names a provider the sign-in page goes to without a click (spec-0001 section 6.4).
	AutoRedirect string `yaml:"auto_redirect"`
}

// OIDCProvider is one identity provider. Name is used in URLs, Title on the button.
type OIDCProvider struct {
	Name          string   `yaml:"name"`
	Title         string   `yaml:"title"`
	Issuer        string   `yaml:"issuer"`
	ClientID      string   `yaml:"client_id"`
	ClientSecret  string   `yaml:"client_secret"`
	Scopes        []string `yaml:"scopes"`         // default openid, email, profile
	LinkByEmail   bool     `yaml:"link_by_email"`  // allow the one-time link for every active account
	AllowedGroups []string `yaml:"allowed_groups"` // empty = no group check
	GroupsClaim   string   `yaml:"groups_claim"`   // default groups
}

// PasswordLoginOn reports whether the local account may sign in with its password: only
// while no OIDC provider is set (D43), and admin.password_login is not false.
func (a Admin) PasswordLoginOn() bool {
	return len(a.OIDC.Providers) == 0 && (a.PasswordLogin == nil || *a.PasswordLogin)
}

// MinPasswordLen is the shortest password accepted for admin accounts.
const MinPasswordLen = 12

func (a Admin) Enabled() bool { return a.Listen != "off" }

// Database is PostgreSQL (required). URL is a postgres:// URL or key=value DSN.
type Database struct {
	URL      string `yaml:"url"`
	MaxConns int32  `yaml:"max_conns"` // pool size per node; default 10
	MinConns int32  `yaml:"min_conns"`

	// Removed keys, kept only to give a clear error for old configs.
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

type Metering struct {
	BufferSize    int           `yaml:"buffer_size"`
	BatchSize     int           `yaml:"batch_size"`
	FlushInterval time.Duration `yaml:"flush_interval"`
}

// NATS is the required message bus. Usage events, hooks and control events go through it,
// so extensions in any language only need a NATS client.
type NATS struct {
	URL            string        `yaml:"url"`
	Creds          string        `yaml:"creds"` // optional .creds file (JWT + nkey)
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
	Usage          NATSUsage     `yaml:"usage"`
	Control        NATSControl   `yaml:"control"`
	Audit          NATSAudit     `yaml:"audit"`
	State          NATSState     `yaml:"state"`
	Hooks          Hooks         `yaml:"hooks"`
}

// NATSState is where nodes share provider account state (spec-0004 section 4.4, D40).
type NATSState struct {
	Bucket string `yaml:"bucket"` // JetStream KV bucket; default AIF_ACCOUNT_STATE
}

type NATSUsage struct {
	SubjectPrefix string        `yaml:"subject_prefix"` // records go to <prefix>.<tenant>.<app>
	JetStream     *bool         `yaml:"jetstream"`      // default true: durable stream with dedup
	Stream        string        `yaml:"stream"`
	MaxAge        time.Duration `yaml:"max_age"` // stream retention, used only when the gateway creates it
}

func (u NATSUsage) UseJetStream() bool { return u.JetStream == nil || *u.JetStream }

type NATSControl struct {
	SubjectPrefix string `yaml:"subject_prefix"` // events go to <prefix>.<event>, e.g. key_revoked
}

// NATSAudit is where security events go, e.g. <prefix>.breakglass (spec-0001 section 8.1).
type NATSAudit struct {
	SubjectPrefix string `yaml:"subject_prefix"` // default aif.audit.v1
}

type Hooks struct {
	PreRequest Hook `yaml:"pre_request"`
}

// Hook is one request/reply extension point. An empty Subject turns it off.
type Hook struct {
	Subject  string        `yaml:"subject"`
	Timeout  time.Duration `yaml:"timeout"`
	FailOpen bool          `yaml:"fail_open"` // on timeout / no responder: true = allow, false = 503
	Models   []string      `yaml:"models"`    // same patterns as key allow-lists; empty = all
	Formats  []string      `yaml:"formats"`   // API formats sent to the hook; default [openai]
}

func (h Hook) Enabled() bool { return h.Subject != "" }

type Provider struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"` // openai (default), openrouter, anthropic
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	// KeyRef is where the key comes from, for the database catalog (spec-0006 section 4.4):
	// "env:NAME" when api_key was ${NAME}, or api_key itself when written as env:NAME or
	// file:/path. Parse sets it; "" with a non-empty APIKey means a key value in the file.
	KeyRef  string            `yaml:"-"`
	Headers map[string]string `yaml:"headers"`

	// Anthropic only (spec-0002 section 5).
	WorkspaceID  string `yaml:"workspace_id"`  // sent as anthropic-workspace-id; only for keys not scoped to one workspace
	OpenAIFormat string `yaml:"openai_format"` // required: how OpenAI-format requests reach Anthropic (compat)

	// Several accounts (keys) and how one is picked per request (spec-0004).
	// A provider has api_key or accounts, not both; api_key is one account named "default".
	Accounts    []Account     `yaml:"accounts"`
	Policy      string        `yaml:"policy"`       // priority (default) or round_robin
	Sticky      string        `yaml:"sticky"`       // round_robin only: none (default), key, end_user, session
	MaxAttempts int           `yaml:"max_attempts"` // accounts tried per request; 0 = all
	Cooldown    time.Duration `yaml:"cooldown"`     // rest after a retryable failure; default 30s
	// Credit is the default credit of every account that sets none (task-0010).
	Credit *Credit `yaml:"credit"`
}

// Account is one key of a provider. BaseURL, Headers and WorkspaceID override the provider's.
type Account struct {
	Name        string            `yaml:"name"`
	APIKey      string            `yaml:"api_key"`
	KeyRef      string            `yaml:"-"`      // see Provider.KeyRef
	Weight      *int              `yaml:"weight"` // round_robin only, 0-100, default 1; 0 drains the account
	BaseURL     string            `yaml:"base_url"`
	Headers     map[string]string `yaml:"headers"`
	WorkspaceID string            `yaml:"workspace_id"`
	Credit      *Credit           `yaml:"credit"` // how this account is paid for; nil = not tracked (task-0010)
}

// Credit is how one provider account is paid for (task-0010). An account has one
// credit type; an organization with several kinds of credit is several accounts.
type Credit struct {
	Type string `yaml:"type"` // monthly, prepaid or payg; required
	// USD is the credit of each cycle (monthly) or the budget of each cycle (payg).
	// Prepaid has none: its balance is the top-ups recorded in the database.
	USD float64 `yaml:"usd"`
	// CycleDay is the day of the month a cycle starts (1-31, default 1); in a month
	// without that day the cycle starts on the month's last day. Timezone is an IANA
	// name such as Asia/Bangkok (default UTC). monthly and payg only.
	CycleDay int    `yaml:"cycle_day"`
	Timezone string `yaml:"timezone"`
}

// Credit types.
const (
	CreditMonthly = "monthly" // a plan credit that resets each cycle and expires (for example a Claude plan)
	CreditPrepaid = "prepaid" // a balance that only top-ups raise; no reset
	CreditPayg    = "payg"    // pay-as-you-go: billed later; usd is a budget for the warning only
)

// Periodic reports whether the credit has cycles (monthly and payg).
func (c Credit) Periodic() bool { return c.Type == CreditMonthly || c.Type == CreditPayg }

// Location is the credit's time zone; UTC when unset or unknown.
func (c Credit) Location() *time.Location {
	if c.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// Cycle returns the cycle that holds now: [start, end), in the credit's time zone.
func (c Credit) Cycle(now time.Time) (start, end time.Time) {
	loc := c.Location()
	now = now.In(loc)
	on := func(y int, m time.Month) time.Time {
		d := max(c.CycleDay, 1)
		if last := time.Date(y, m+1, 0, 0, 0, 0, 0, loc).Day(); d > last {
			d = last
		}
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
	start = on(now.Year(), now.Month())
	if now.Before(start) {
		start = on(now.Year(), now.Month()-1)
	}
	return start, on(start.Year(), start.Month()+1)
}

func (c *Credit) validate() error {
	switch c.Type {
	case CreditMonthly, CreditPayg:
		switch {
		case !(c.USD > 0) || math.IsInf(c.USD, 0):
			return fmt.Errorf("credit.usd must be above 0")
		case c.CycleDay < 1 || c.CycleDay > 31:
			return fmt.Errorf("credit.cycle_day must be 1 to 31")
		}
		if _, err := time.LoadLocation(c.Timezone); err != nil || c.Timezone == "Local" {
			return fmt.Errorf("credit.timezone %q: use an IANA name such as Asia/Bangkok", c.Timezone)
		}
	case CreditPrepaid:
		if c.USD != 0 || c.CycleDay != 0 || c.Timezone != "" {
			return fmt.Errorf("credit type prepaid takes no usd, cycle_day or timezone: its balance is the top-ups recorded in the database (aif credit add)")
		}
	case "":
		return fmt.Errorf("credit.type is required: monthly, prepaid or payg")
	default:
		return fmt.Errorf("credit.type %q: use monthly, prepaid or payg", c.Type)
	}
	return nil
}

// WeightOr returns the weight, or 1 when it is not set.
func (a Account) WeightOr() int {
	if a.Weight == nil {
		return 1
	}
	return *a.Weight
}

// Account selection policies and sticky modes (spec-0004 section 4).
const (
	PolicyPriority   = "priority"
	PolicyRoundRobin = "round_robin"
	StickyNone       = "none"
	StickyKey        = "key"
	StickyEndUser    = "end_user"
	StickySession    = "session"
	DefaultAccount   = "default"
)

// Provider types.
const (
	TypeOpenAI     = "openai"
	TypeOpenRouter = "openrouter"
	TypeAnthropic  = "anthropic"
)

// OpenAIFormatCompat sends OpenAI-format requests to Anthropic's OpenAI-compatible endpoint.
// "native" (gateway-side translation) arrives with task-0009.
const OpenAIFormatCompat = "compat"

// Model is a client-facing alias. It has targets (spec-0005), or the short form
// provider + upstream + price, which is one target. NormalizeCatalog turns the short
// form into Targets and clears Provider, Upstream and Price.
type Model struct {
	Name     string `yaml:"name"`
	Provider string `yaml:"provider"`
	Upstream string `yaml:"upstream"`
	Price    *Price `yaml:"price"`
	// Formats lists the API formats that may use this alias. Default: every format at
	// least one target serves (spec-0002 section 4, spec-0005 section 2.3).
	Formats []string `yaml:"formats"`

	Targets     []Target `yaml:"targets"`
	Policy      string   `yaml:"policy"`       // priority (default) or round_robin
	Sticky      string   `yaml:"sticky"`       // round_robin only: none (default), key, end_user, session
	MaxAttempts int      `yaml:"max_attempts"` // upstream calls per request, targets and accounts together; 0 = no cap
}

// Target is one backend of an alias: a provider, the model name and price there.
type Target struct {
	Provider string   `yaml:"provider"`
	Upstream string   `yaml:"upstream"` // default: the alias name
	Price    *Price   `yaml:"price"`
	Weight   *int     `yaml:"weight"`  // round_robin only, 0-100, default 1
	Formats  []string `yaml:"formats"` // default: what the provider type serves
}

// WeightOr returns the weight, or 1 when it is not set.
func (t Target) WeightOr() int {
	if t.Weight == nil {
		return 1
	}
	return *t.Weight
}

// Key names a target inside its alias: provider/upstream.
func (t Target) Key() string { return t.Provider + "/" + t.Upstream }

// Price is in USD per 1M tokens. CachedInput falls back to Input when zero.
// CacheWrite (Anthropic prompt cache writes) falls back to 1.25 x Input, the 5-minute cache price.
type Price struct {
	Input       float64 `yaml:"input" json:"input"`
	Output      float64 `yaml:"output" json:"output"`
	CachedInput float64 `yaml:"cached_input" json:"cached_input"`
	CacheWrite  float64 `yaml:"cache_write" json:"cache_write,omitempty"`
}

// DefaultFormats is the format list for a model or passthrough name on a provider of this type.
func DefaultFormats(providerType string) []string {
	if providerType == TypeAnthropic {
		return []string{FormatOpenAI, FormatAnthropic}
	}
	return []string{FormatOpenAI}
}

const (
	openRouterBaseURL = "https://openrouter.ai/api/v1"
	anthropicBaseURL  = "https://api.anthropic.com/v1"
)

// Load reads a YAML file, expands ${VAR} and ${VAR:-default}, applies defaults and validates.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	cfg.Path = path
	return cfg, nil
}

func Parse(raw []byte) (*Config, error) {
	var cfg Config
	expanded, fromEnv := expandEnv(string(raw))
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	setKeyRefs(cfg.Providers, fromEnv)
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// CatalogOnly is only the catalog part of a config file: what `aif catalog export`
// writes and `aif catalog import` reads.
type CatalogOnly struct {
	Passthrough bool       `yaml:"passthrough"`
	Providers   []Provider `yaml:"providers"`
	Models      []Model    `yaml:"models"`
}

// ParseCatalog reads a catalog file: ${VAR} expansion, key references, defaults and
// the catalog checks. Other sections are ignored.
func ParseCatalog(raw []byte) (*CatalogOnly, error) {
	var cf CatalogOnly
	expanded, fromEnv := expandEnv(string(raw))
	if err := yaml.Unmarshal([]byte(expanded), &cf); err != nil {
		return nil, fmt.Errorf("parse catalog: %w", err)
	}
	setKeyRefs(cf.Providers, fromEnv)
	NormalizeCatalog(cf.Providers, cf.Models)
	if err := ValidateCatalog(cf.Providers, cf.Models); err != nil {
		return nil, err
	}
	return &cf, nil
}

// setKeyRefs fills Provider.KeyRef: api_key written as env:NAME or file:/path is a
// reference; a value that came from ${NAME} becomes env:NAME.
func setKeyRefs(providers []Provider, fromEnv map[string]string) {
	set := func(key, ref *string) {
		switch {
		case strings.HasPrefix(*key, "env:") || strings.HasPrefix(*key, "file:"):
			*ref, *key = *key, ""
		case *key != "":
			if name, ok := fromEnv[*key]; ok {
				*ref = "env:" + name
			}
		}
	}
	for i := range providers {
		p := &providers[i]
		set(&p.APIKey, &p.KeyRef)
		for j := range p.Accounts {
			set(&p.Accounts[j].APIKey, &p.Accounts[j].KeyRef)
		}
	}
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// ExpandEnv replaces ${VAR} and ${VAR:-default}. A bare $VAR is left as is.
func ExpandEnv(s string) string {
	out, _ := expandEnv(s)
	return out
}

// expandEnv also returns, for each value taken from the environment, the variable it
// came from. A default (${VAR:-x}) is not from the environment.
func expandEnv(s string) (string, map[string]string) {
	fromEnv := map[string]string{}
	out := envPattern.ReplaceAllStringFunc(s, func(m string) string {
		parts := envPattern.FindStringSubmatch(m)
		if v, ok := os.LookupEnv(parts[1]); ok && v != "" {
			if _, seen := fromEnv[v]; !seen {
				fromEnv[v] = parts[1]
			}
			return v
		}
		return parts[2]
	})
	return out, fromEnv
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.UpstreamTimeout == 0 {
		c.Server.UpstreamTimeout = 10 * time.Minute
	}
	if c.Server.MaxBodyBytes == 0 {
		c.Server.MaxBodyBytes = 32 << 20
	}
	if c.Admin.Listen == "" {
		c.Admin.Listen = "127.0.0.1:8081"
	}
	if c.Admin.SessionTTL == 0 {
		c.Admin.SessionTTL = 12 * time.Hour
	}
	if c.Admin.CookieSecure == "" {
		c.Admin.CookieSecure = "auto"
	}
	if c.Admin.OIDC.SessionTTL == 0 {
		c.Admin.OIDC.SessionTTL = c.Admin.SessionTTL
	}
	c.Admin.PublicURL = strings.TrimRight(c.Admin.PublicURL, "/")
	for i := range c.Admin.OIDC.Providers {
		p := &c.Admin.OIDC.Providers[i]
		if len(p.Scopes) == 0 {
			p.Scopes = []string{"openid", "email", "profile"}
		}
		if p.GroupsClaim == "" {
			p.GroupsClaim = "groups"
		}
		if p.Title == "" {
			p.Title = p.Name
		}
	}
	if c.Database.MaxConns == 0 {
		c.Database.MaxConns = 10
	}
	if c.Metering.BufferSize == 0 {
		c.Metering.BufferSize = 10000
	}
	if c.Metering.BatchSize == 0 {
		c.Metering.BatchSize = 200
	}
	if c.Metering.FlushInterval == 0 {
		c.Metering.FlushInterval = time.Second
	}
	n := &c.NATS
	if n.URL == "" {
		n.URL = "nats://127.0.0.1:4222"
	}
	if n.ConnectTimeout == 0 {
		n.ConnectTimeout = 5 * time.Second
	}
	if n.Usage.SubjectPrefix == "" {
		n.Usage.SubjectPrefix = "aif.usage.v1"
	}
	if n.Usage.Stream == "" {
		n.Usage.Stream = "AIF_USAGE"
	}
	if n.Usage.MaxAge == 0 {
		n.Usage.MaxAge = 30 * 24 * time.Hour
	}
	if n.Control.SubjectPrefix == "" {
		n.Control.SubjectPrefix = "aif.control.v1"
	}
	if n.Audit.SubjectPrefix == "" {
		n.Audit.SubjectPrefix = "aif.audit.v1"
	}
	if n.State.Bucket == "" {
		n.State.Bucket = "AIF_ACCOUNT_STATE"
	}
	if n.Hooks.PreRequest.Timeout == 0 {
		n.Hooks.PreRequest.Timeout = 200 * time.Millisecond
	}
	if len(n.Hooks.PreRequest.Formats) == 0 {
		n.Hooks.PreRequest.Formats = []string{FormatOpenAI}
	}
	if c.Server.ClientIPHeader == "" {
		c.Server.ClientIPHeader = clientip.DefaultHeader
	}
	if c.Catalog.Mode == "" {
		c.Catalog.Mode = CatalogDatabase
	}
	if c.Catalog.WatchInterval == 0 {
		c.Catalog.WatchInterval = 5 * time.Second
	}
	c.Catalog.SecretKeys = slices.DeleteFunc(c.Catalog.SecretKeys, func(s string) bool { return strings.TrimSpace(s) == "" })
	if c.Catalog.PollInterval == 0 {
		c.Catalog.PollInterval = 30 * time.Second
	}
	if c.Metrics.Listen == "" {
		c.Metrics.Listen = "off"
	}
	if c.Server.Anthropic.Listen == "" {
		c.Server.Anthropic.Listen = "off"
	}
	NormalizeCatalog(c.Providers, c.Models)
}

// NormalizeCatalog applies the defaults of providers and models in place: provider type
// and base URL, model upstream name and formats. The file and the database catalog
// (spec-0006) both use it, so a catalog means the same wherever it comes from.
func NormalizeCatalog(providers []Provider, models []Model) {
	types := map[string]string{}
	for i := range providers {
		p := &providers[i]
		if p.Type == "" {
			p.Type = TypeOpenAI
		}
		if p.BaseURL == "" {
			switch p.Type {
			case TypeOpenRouter:
				p.BaseURL = openRouterBaseURL
			case TypeAnthropic:
				p.BaseURL = anthropicBaseURL
			}
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
		if len(p.Accounts) == 0 { // api_key form: one account named "default"
			p.Accounts = []Account{{Name: DefaultAccount, APIKey: p.APIKey, KeyRef: p.KeyRef}}
			p.APIKey, p.KeyRef = "", ""
		}
		for j := range p.Accounts {
			a := &p.Accounts[j]
			a.BaseURL = strings.TrimRight(a.BaseURL, "/")
			if a.Credit == nil && p.Credit != nil { // the provider's credit is each account's default
				c := *p.Credit
				a.Credit = &c
			}
			if a.Credit != nil && a.Credit.Periodic() && (a.Credit.CycleDay == 0 || a.Credit.Timezone == "") {
				c := *a.Credit // a copy: never change a Credit that another account shares
				if c.CycleDay == 0 {
					c.CycleDay = 1
				}
				if c.Timezone == "" {
					c.Timezone = "UTC"
				}
				a.Credit = &c
			}
		}
		p.Credit = nil
		if p.Policy == "" {
			p.Policy = PolicyPriority
		}
		if p.Sticky == "" {
			p.Sticky = StickyNone
		}
		if p.Cooldown == 0 {
			p.Cooldown = 30 * time.Second
		}
		types[p.Name] = p.Type
	}
	for i := range models {
		m := &models[i]
		if len(m.Targets) == 0 { // short form: one target
			m.Targets = []Target{{Provider: m.Provider, Upstream: m.Upstream, Price: m.Price, Formats: m.Formats}}
			m.Provider, m.Upstream, m.Price = "", "", nil
		}
		var union []string
		for j := range m.Targets {
			t := &m.Targets[j]
			if t.Upstream == "" {
				t.Upstream = m.Name
			}
			if len(t.Formats) == 0 {
				t.Formats = DefaultFormats(types[t.Provider])
			}
			for _, f := range t.Formats {
				if !slices.Contains(union, f) {
					union = append(union, f)
				}
			}
		}
		if len(m.Formats) == 0 {
			slices.SortFunc(union, func(a, b string) int { return formatRank(a) - formatRank(b) })
			m.Formats = union
		}
		if m.Policy == "" {
			m.Policy = PolicyPriority
		}
		if m.Sticky == "" {
			m.Sticky = StickyNone
		}
	}
}

// formatRank keeps format lists in one order: openai, then anthropic.
func formatRank(f string) int {
	if f == FormatOpenAI {
		return 0
	}
	return 1
}

func (c *Config) validate() error {
	if (c.Database.Driver != "" && c.Database.Driver != "postgres") || c.Database.DSN != "" {
		return fmt.Errorf("database.driver and database.dsn were removed: SQLite is no longer supported; set database.url to a PostgreSQL URL (DATABASE_URL)")
	}
	if c.Database.URL == "" {
		return fmt.Errorf("database.url is required (set DATABASE_URL, e.g. postgres://aif:aif-dev@127.0.0.1:5432/aif?sslmode=disable)")
	}
	if c.Database.MaxConns < 1 || c.Database.MinConns < 0 || c.Database.MinConns > c.Database.MaxConns {
		return fmt.Errorf("database: need 1 <= max_conns and 0 <= min_conns <= max_conns")
	}
	if c.Admin.Enabled() {
		if c.Admin.MasterKey != "" && len(c.Admin.MasterKey) < 16 {
			return fmt.Errorf("admin.master_key must be at least 16 characters (or empty to turn it off)")
		}
		if _, _, err := net.SplitHostPort(c.Admin.Listen); err != nil {
			return fmt.Errorf("admin.listen: %w", err)
		}
		if c.Admin.SessionTTL < time.Minute {
			return fmt.Errorf("admin.session_ttl must be at least 1m")
		}
		switch c.Admin.CookieSecure {
		case "auto", "always", "never":
		default:
			return fmt.Errorf("admin.cookie_secure %q: use auto, always or never", c.Admin.CookieSecure)
		}
		if b := c.Admin.Bootstrap; b.Email != "" || b.Password != "" || b.PasswordHash != "" {
			if !strings.Contains(b.Email, "@") {
				return fmt.Errorf("admin.bootstrap.email %q is not an email address", b.Email)
			}
			switch {
			case b.Password != "" && b.PasswordHash != "":
				return fmt.Errorf("admin.bootstrap: set password or password_hash, not both")
			case b.PasswordHash != "":
				if !strings.HasPrefix(b.PasswordHash, "$argon2id$") || strings.Count(b.PasswordHash, "$") != 5 {
					return fmt.Errorf("admin.bootstrap.password_hash is not an argon2id hash (make one with `aif admin hash-password`)")
				}
			case len(b.Password) < MinPasswordLen:
				return fmt.Errorf("admin.bootstrap.password must be at least %d characters (or set password_hash)", MinPasswordLen)
			}
		}
		if err := c.Admin.validateOIDC(); err != nil {
			return err
		}
	}
	if err := c.Server.Anthropic.validate(); err != nil {
		return err
	}
	if err := c.validateProduction(); err != nil {
		return err
	}
	if m := c.Catalog.Mode; m != CatalogDatabase && m != CatalogFile {
		return fmt.Errorf("catalog.mode %q: use database or file", m)
	}
	if c.Catalog.WatchInterval < 100*time.Millisecond || c.Catalog.PollInterval < 100*time.Millisecond {
		return fmt.Errorf("catalog.watch_interval and catalog.poll_interval must be at least 100ms")
	}
	for i, k := range c.Catalog.SecretKeys {
		if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(k)); err != nil || len(b) != 32 {
			return fmt.Errorf("catalog.secret_keys[%d]: must be base64 of 32 random bytes (openssl rand -base64 32)", i)
		}
	}
	if err := validFormats("nats.hooks.pre_request.formats", c.NATS.Hooks.PreRequest.Formats); err != nil {
		return err
	}
	for name, subj := range map[string]string{
		"nats.usage.subject_prefix":      c.NATS.Usage.SubjectPrefix,
		"nats.control.subject_prefix":    c.NATS.Control.SubjectPrefix,
		"nats.audit.subject_prefix":      c.NATS.Audit.SubjectPrefix,
		"nats.hooks.pre_request.subject": c.NATS.Hooks.PreRequest.Subject,
	} {
		if subj != "" && !validSubject(subj) {
			return fmt.Errorf("%s %q is not a valid NATS subject (no spaces, wildcards or empty tokens)", name, subj)
		}
	}
	return ValidateCatalog(c.Providers, c.Models)
}

// ValidateCatalog checks providers and models after NormalizeCatalog: names, types,
// formats and model-to-provider links. It is the whole-catalog check of spec-0006 D39.
func ValidateCatalog(providers []Provider, models []Model) error {
	known := map[string]string{} // provider name -> type
	for _, p := range providers {
		switch {
		case p.Name == "":
			return fmt.Errorf("provider without name")
		case strings.Contains(p.Name, "/"):
			return fmt.Errorf("provider %q: name must not contain '/'", p.Name)
		case known[p.Name] != "":
			return fmt.Errorf("provider %q defined twice", p.Name)
		case p.Type != TypeOpenAI && p.Type != TypeOpenRouter && p.Type != TypeAnthropic:
			return fmt.Errorf("provider %q: type %q is not supported (supported: openai, openrouter, anthropic)", p.Name, p.Type)
		case p.BaseURL == "":
			return fmt.Errorf("provider %q: base_url is required", p.Name)
		}
		if err := p.validateAnthropic(); err != nil {
			return err
		}
		if err := p.validateAccounts(); err != nil {
			return err
		}
		known[p.Name] = p.Type
	}
	seen := map[string]bool{}
	for _, m := range models {
		switch {
		case m.Name == "":
			return fmt.Errorf("model without name")
		case seen[m.Name]:
			return fmt.Errorf("model %q defined twice", m.Name)
		case m.Provider != "" || m.Upstream != "" || m.Price != nil:
			return fmt.Errorf("model %q: use provider/upstream/price or targets, not both", m.Name)
		}
		if err := validateModel(m, known); err != nil {
			return err
		}
		seen[m.Name] = true
	}
	return nil
}

// validateModel checks targets and the selection policy of one alias, after NormalizeCatalog.
func validateModel(m Model, known map[string]string) error {
	if len(m.Targets) == 0 {
		return fmt.Errorf("model %q: needs a provider or targets", m.Name)
	}
	if err := validFormats(fmt.Sprintf("model %q: formats", m.Name), m.Formats); err != nil {
		return err
	}
	switch {
	case m.Policy != PolicyPriority && m.Policy != PolicyRoundRobin:
		return fmt.Errorf("model %q: policy %q: use priority or round_robin", m.Name, m.Policy)
	case m.Sticky != StickyNone && m.Sticky != StickyKey && m.Sticky != StickyEndUser && m.Sticky != StickySession:
		return fmt.Errorf("model %q: sticky %q: use none, key, end_user or session", m.Name, m.Sticky)
	case m.Sticky != StickyNone && m.Policy != PolicyRoundRobin:
		return fmt.Errorf("model %q: sticky works only with policy round_robin", m.Name)
	case m.MaxAttempts < 0:
		return fmt.Errorf("model %q: max_attempts must not be negative", m.Name)
	}
	served, keys, first := map[string]bool{}, map[string]bool{}, 0
	for _, t := range m.Targets {
		switch {
		case known[t.Provider] == "":
			return fmt.Errorf("model %q: unknown provider %q", m.Name, t.Provider)
		case keys[t.Key()]:
			return fmt.Errorf("model %q: target %s defined twice", m.Name, t.Key())
		case t.Weight != nil && m.Policy != PolicyRoundRobin:
			return fmt.Errorf("model %q target %s: weight works only with policy round_robin", m.Name, t.Key())
		case t.WeightOr() < 0 || t.WeightOr() > 100:
			return fmt.Errorf("model %q target %s: weight must be 0 to 100", m.Name, t.Key())
		}
		if err := validFormats(fmt.Sprintf("model %q target %s: formats", m.Name, t.Key()), t.Formats); err != nil {
			return err
		}
		if slices.Contains(t.Formats, FormatAnthropic) && known[t.Provider] != TypeAnthropic {
			return fmt.Errorf("model %q: format anthropic needs a provider of type anthropic (translation is not supported)", m.Name)
		}
		for _, f := range t.Formats {
			served[f] = true
		}
		keys[t.Key()] = true
		if t.WeightOr() > 0 {
			first++
		}
	}
	for _, f := range m.Formats {
		if !served[f] {
			return fmt.Errorf("model %q: format %s is served by none of its targets", m.Name, f)
		}
	}
	if m.Policy == PolicyRoundRobin && first == 0 {
		return fmt.Errorf("model %q: round_robin needs at least one target with weight above 0", m.Name)
	}
	return nil
}

var oidcName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// validateOIDC checks admin.public_url and admin.oidc (spec-0001 section 6.3).
func (a Admin) validateOIDC() error {
	if a.OIDC.SessionTTL < time.Minute {
		return fmt.Errorf("admin.oidc.session_ttl must be at least 1m")
	}
	if len(a.OIDC.Providers) == 0 {
		if a.OIDC.AutoRedirect != "" {
			return fmt.Errorf("admin.oidc.auto_redirect %q: no provider is set in admin.oidc.providers", a.OIDC.AutoRedirect)
		}
		return nil
	}
	if err := checkHTTPSURL("admin.public_url", a.PublicURL); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, p := range a.OIDC.Providers {
		field := "admin.oidc.providers[" + p.Name + "]"
		switch {
		case !oidcName.MatchString(p.Name):
			return fmt.Errorf("admin.oidc.providers: name %q: use a-z, 0-9 and -", p.Name)
		case seen[p.Name]:
			return fmt.Errorf("admin.oidc.providers: name %q is used twice", p.Name)
		case p.ClientID == "":
			return fmt.Errorf("%s.client_id is required", field)
		}
		seen[p.Name] = true
		if err := checkHTTPSURL(field+".issuer", p.Issuer); err != nil {
			return err
		}
	}
	if a.OIDC.AutoRedirect != "" && !seen[a.OIDC.AutoRedirect] {
		return fmt.Errorf("admin.oidc.auto_redirect %q: not a provider in admin.oidc.providers", a.OIDC.AutoRedirect)
	}
	return nil
}

// checkHTTPSURL accepts an absolute https URL, or http for localhost.
func checkHTTPSURL(field, raw string) error {
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s %q: need an absolute URL such as https://host", field, raw)
	}
	if u.Scheme == "https" || (u.Scheme == "http" && IsLoopback(net.JoinHostPort(u.Hostname(), "0"))) {
		return nil
	}
	return fmt.Errorf("%s %q: use https (http only for localhost)", field, raw)
}

// validateProduction checks the settings of spec-0003: trusted proxies, metrics, dev secrets.
func (c *Config) validateProduction() error {
	for _, tp := range c.Server.TrustedProxies {
		if _, err := clientip.ParsePrefix(tp); err != nil {
			return fmt.Errorf("server.trusted_proxies: %w", err)
		}
	}
	if !validHeaderName(c.Server.ClientIPHeader) {
		return fmt.Errorf("server.client_ip_header %q is not a valid header name", c.Server.ClientIPHeader)
	}
	if c.Metrics.Enabled() {
		if _, _, err := net.SplitHostPort(c.Metrics.Listen); err != nil {
			return fmt.Errorf("metrics.listen: %w", err)
		}
	}
	if !c.DevMode {
		values := map[string]string{"admin.master_key": c.Admin.MasterKey, "admin.bootstrap.password": c.Admin.Bootstrap.Password}
		for _, field := range []string{"admin.master_key", "admin.bootstrap.password"} {
			if values[field] == devSecrets[field] {
				return fmt.Errorf("%s is the public dev default; set a secret value (or dev_mode: true, for local development only)", field)
			}
		}
	}
	return nil
}

func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

var accountName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// validateAccounts checks accounts and the selection policy (spec-0004 section 3), after NormalizeCatalog.
func (p Provider) validateAccounts() error {
	if p.APIKey != "" || p.KeyRef != "" {
		return fmt.Errorf("provider %q: use api_key or accounts, not both", p.Name)
	}
	switch p.Policy {
	case PolicyPriority, PolicyRoundRobin:
	default:
		return fmt.Errorf("provider %q: policy %q: use priority or round_robin", p.Name, p.Policy)
	}
	switch p.Sticky {
	case StickyNone:
	case StickyKey, StickyEndUser, StickySession:
		if p.Policy != PolicyRoundRobin {
			return fmt.Errorf("provider %q: sticky works only with policy round_robin", p.Name)
		}
	default:
		return fmt.Errorf("provider %q: sticky %q: use none, key, end_user or session", p.Name, p.Sticky)
	}
	if p.MaxAttempts < 0 || p.Cooldown < 0 {
		return fmt.Errorf("provider %q: max_attempts and cooldown must not be negative", p.Name)
	}
	seen, first := map[string]bool{}, 0
	for _, a := range p.Accounts {
		switch {
		case !accountName.MatchString(a.Name):
			return fmt.Errorf("provider %q: account name %q: use a-z, 0-9 and '-'", p.Name, a.Name)
		case seen[a.Name]:
			return fmt.Errorf("provider %q: account %q defined twice", p.Name, a.Name)
		case a.Weight != nil && p.Policy != PolicyRoundRobin:
			return fmt.Errorf("provider %q account %q: weight works only with policy round_robin", p.Name, a.Name)
		case a.WeightOr() < 0 || a.WeightOr() > 100:
			return fmt.Errorf("provider %q account %q: weight must be 0 to 100", p.Name, a.Name)
		case a.WorkspaceID != "" && p.Type != TypeAnthropic:
			return fmt.Errorf("provider %q account %q: workspace_id is only for type anthropic", p.Name, a.Name)
		}
		if a.Credit != nil {
			if err := a.Credit.validate(); err != nil {
				return fmt.Errorf("provider %q account %q: %v", p.Name, a.Name, err)
			}
		}
		seen[a.Name] = true
		if a.WeightOr() > 0 {
			first++
		}
	}
	if p.Policy == PolicyRoundRobin && first == 0 {
		return fmt.Errorf("provider %q: round_robin needs at least one account with weight above 0", p.Name)
	}
	return nil
}

func (p Provider) validateAnthropic() error {
	if p.Type != TypeAnthropic {
		if p.OpenAIFormat != "" || p.WorkspaceID != "" {
			return fmt.Errorf("provider %q: openai_format and workspace_id are only for type anthropic", p.Name)
		}
		return nil
	}
	switch p.OpenAIFormat {
	case OpenAIFormatCompat:
		return nil
	case "":
		return fmt.Errorf("provider %q: openai_format is required for type anthropic (use %q: Anthropic's OpenAI-compatible endpoint)", p.Name, OpenAIFormatCompat)
	case "native":
		return fmt.Errorf("provider %q: openai_format native is not available yet; use %q", p.Name, OpenAIFormatCompat)
	default:
		return fmt.Errorf("provider %q: openai_format %q is not supported (use %q)", p.Name, p.OpenAIFormat, OpenAIFormatCompat)
	}
}

func (a AnthropicServer) validate() error {
	if p := a.Prefix(); p != "" {
		switch {
		case !strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.ContainsAny(p, " {}"):
			return fmt.Errorf("server.anthropic.path_prefix %q: must start with '/' and not end with '/' (or \"\" to turn it off)", p)
		case p == "/v1" || p == "/openai" || p == "/healthz" || strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/openai/"):
			return fmt.Errorf("server.anthropic.path_prefix %q clashes with the OpenAI paths", p)
		}
	}
	if a.ListenEnabled() {
		if _, _, err := net.SplitHostPort(a.Listen); err != nil {
			return fmt.Errorf("server.anthropic.listen: %w", err)
		}
	}
	return nil
}

func validFormats(field string, fs []string) error {
	for _, f := range fs {
		if f != FormatOpenAI && f != FormatAnthropic {
			return fmt.Errorf("%s: unknown format %q (use openai or anthropic)", field, f)
		}
	}
	return nil
}

// validSubject accepts a literal NATS subject: dot-separated, non-empty tokens, no wildcards or spaces.
func validSubject(s string) bool {
	for _, tok := range strings.Split(s, ".") {
		if tok == "" || strings.ContainsAny(tok, "*> \t\r\n") {
			return false
		}
	}
	return true
}

// IsLoopback reports whether a listen address only accepts local connections.
// An empty host (":8081") listens on all interfaces.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
