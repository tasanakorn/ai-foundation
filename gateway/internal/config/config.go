// Package config loads and validates the gateway configuration file.
//
// Providers and models live in the config file (easy to review and version).
// Keys and usage live in the database (they change at runtime).
package config

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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
}

type Server struct {
	Listen          string          `yaml:"listen"`
	UpstreamTimeout time.Duration   `yaml:"upstream_timeout"`
	MaxBodyBytes    int64           `yaml:"max_body_bytes"`
	Anthropic       AnthropicServer `yaml:"anthropic"`
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
	Name    string            `yaml:"name"`
	Type    string            `yaml:"type"` // openai (default), openrouter, anthropic
	BaseURL string            `yaml:"base_url"`
	APIKey  string            `yaml:"api_key"`
	Headers map[string]string `yaml:"headers"`

	// Anthropic only (spec-0002 section 5).
	WorkspaceID  string `yaml:"workspace_id"`  // sent as anthropic-workspace-id; only for keys not scoped to one workspace
	OpenAIFormat string `yaml:"openai_format"` // required: how OpenAI-format requests reach Anthropic (compat)
}

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
	return Parse(raw)
}

func Parse(raw []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal([]byte(ExpandEnv(string(raw))), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// ExpandEnv replaces ${VAR} and ${VAR:-default}. A bare $VAR is left as is.
func ExpandEnv(s string) string {
	return envPattern.ReplaceAllStringFunc(s, func(m string) string {
		parts := envPattern.FindStringSubmatch(m)
		if v, ok := os.LookupEnv(parts[1]); ok && v != "" {
			return v
		}
		return parts[2]
	})
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
	if c.Server.Anthropic.Listen == "" {
		c.Server.Anthropic.Listen = "off"
	}
	types := map[string]string{}
	for i := range c.Providers {
		p := &c.Providers[i]
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
		types[p.Name] = p.Type
	}
	for i := range c.Models {
		m := &c.Models[i]
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
	providers := map[string]string{} // name -> type
	for _, p := range c.Providers {
		switch {
		case p.Name == "":
			return fmt.Errorf("provider without name")
		case strings.Contains(p.Name, "/"):
			return fmt.Errorf("provider %q: name must not contain '/'", p.Name)
		case providers[p.Name] != "":
			return fmt.Errorf("provider %q defined twice", p.Name)
		case p.Type != TypeOpenAI && p.Type != TypeOpenRouter && p.Type != TypeAnthropic:
			return fmt.Errorf("provider %q: type %q is not supported (supported: openai, openrouter, anthropic)", p.Name, p.Type)
		case p.BaseURL == "":
			return fmt.Errorf("provider %q: base_url is required", p.Name)
		}
		if err := p.validateAnthropic(); err != nil {
			return err
		}
		providers[p.Name] = p.Type
	}
	models := map[string]bool{}
	for _, m := range c.Models {
		switch {
		case m.Name == "":
			return fmt.Errorf("model without name")
		case models[m.Name]:
			return fmt.Errorf("model %q defined twice", m.Name)
		case providers[m.Provider] == "":
			return fmt.Errorf("model %q: unknown provider %q", m.Name, m.Provider)
		}
		if err := validFormats(fmt.Sprintf("model %q: formats", m.Name), m.Formats); err != nil {
			return err
		}
		if slices.Contains(m.Formats, FormatAnthropic) && providers[m.Provider] != TypeAnthropic {
			return fmt.Errorf("model %q: format anthropic needs a provider of type anthropic (translation is not supported)", m.Name)
		}
		models[m.Name] = true
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
