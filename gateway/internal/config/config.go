// Package config loads and validates the gateway configuration file.
//
// Infra settings live in the file. Providers and models (the catalog) are in the file
// today; spec-0006 moves them to the database, where the file seeds them.
// Keys and usage live in the database (they change at runtime).
package config

import (
	"encoding/base64"
	"fmt"
	"net"
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
// People sign in with built-in accounts (email + password, stored in the database).
// MasterKey is optional: a bearer token with the admin role, for scripts and emergency access.
type Admin struct {
	Listen       string         `yaml:"listen"`
	MasterKey    string         `yaml:"master_key"`
	UIDir        string         `yaml:"ui_dir"`
	SessionTTL   time.Duration  `yaml:"session_ttl"`   // browser login lifetime; default 12h
	CookieSecure string         `yaml:"cookie_secure"` // auto (HTTPS or X-Forwarded-Proto: https), always, never
	Bootstrap    AdminBootstrap `yaml:"bootstrap"`
}

// AdminBootstrap creates the first admin account at startup when no account exists yet.
// Later starts ignore it, so changing the password here has no effect after the first start.
type AdminBootstrap struct {
	Email    string `yaml:"email"`
	Password string `yaml:"password"`
	Name     string `yaml:"name"`
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
	Hooks          Hooks         `yaml:"hooks"`
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

type Model struct {
	Name     string `yaml:"name"`
	Provider string `yaml:"provider"`
	Upstream string `yaml:"upstream"`
	Price    *Price `yaml:"price"`
	// Formats lists the API formats that may use this alias. Default: [openai], plus
	// anthropic when the provider is type anthropic (spec-0002 section 4).
	Formats []string `yaml:"formats"`
}

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
			p.Accounts[j].BaseURL = strings.TrimRight(p.Accounts[j].BaseURL, "/")
		}
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
		if m.Upstream == "" {
			m.Upstream = m.Name
		}
		if len(m.Formats) == 0 {
			m.Formats = DefaultFormats(types[m.Provider])
		}
	}
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
		if b := c.Admin.Bootstrap; b.Email != "" || b.Password != "" {
			if !strings.Contains(b.Email, "@") {
				return fmt.Errorf("admin.bootstrap.email %q is not an email address", b.Email)
			}
			if len(b.Password) < MinPasswordLen {
				return fmt.Errorf("admin.bootstrap.password must be at least %d characters", MinPasswordLen)
			}
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
		case known[m.Provider] == "":
			return fmt.Errorf("model %q: unknown provider %q", m.Name, m.Provider)
		}
		if err := validFormats(fmt.Sprintf("model %q: formats", m.Name), m.Formats); err != nil {
			return err
		}
		if slices.Contains(m.Formats, FormatAnthropic) && known[m.Provider] != TypeAnthropic {
			return fmt.Errorf("model %q: format anthropic needs a provider of type anthropic (translation is not supported)", m.Name)
		}
		seen[m.Name] = true
	}
	return nil
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
