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
	Listen          string        `yaml:"listen"`
	UpstreamTimeout time.Duration `yaml:"upstream_timeout"`
	MaxBodyBytes    int64         `yaml:"max_body_bytes"`
}

// Admin is the local admin API (and optional web UI). Set Listen to "off" to disable it.
type Admin struct {
	Listen    string `yaml:"listen"`
	MasterKey string `yaml:"master_key"`
	UIDir     string `yaml:"ui_dir"`
}

func (a Admin) Enabled() bool { return a.Listen != "off" }

type Database struct {
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
}

func (h Hook) Enabled() bool { return h.Subject != "" }

type Provider struct {
	Name    string            `yaml:"name"`
	Type    string            `yaml:"type"`
	BaseURL string            `yaml:"base_url"`
	APIKey  string            `yaml:"api_key"`
	Headers map[string]string `yaml:"headers"`
}

type Model struct {
	Name     string `yaml:"name"`
	Provider string `yaml:"provider"`
	Upstream string `yaml:"upstream"`
	Price    *Price `yaml:"price"`
}

// Price is in USD per 1M tokens. CachedInput falls back to Input when zero.
type Price struct {
	Input       float64 `yaml:"input" json:"input"`
	Output      float64 `yaml:"output" json:"output"`
	CachedInput float64 `yaml:"cached_input" json:"cached_input"`
}

const openRouterBaseURL = "https://openrouter.ai/api/v1"

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
	if c.Database.Driver == "" {
		c.Database.Driver = "sqlite"
	}
	if c.Database.DSN == "" {
		c.Database.DSN = "aif.db"
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
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Type == "" {
			p.Type = "openai"
		}
		if p.Type == "openrouter" && p.BaseURL == "" {
			p.BaseURL = openRouterBaseURL
		}
		p.BaseURL = strings.TrimRight(p.BaseURL, "/")
	}
	for i := range c.Models {
		if c.Models[i].Upstream == "" {
			c.Models[i].Upstream = c.Models[i].Name
		}
	}
}

func (c *Config) validate() error {
	if c.Database.Driver != "sqlite" {
		return fmt.Errorf("database.driver %q is not supported (supported: sqlite)", c.Database.Driver)
	}
	if c.Admin.Enabled() {
		if c.Admin.MasterKey == "" {
			return fmt.Errorf("admin.master_key is required when the admin API is enabled (set AIF_MASTER_KEY, or admin.listen: off)")
		}
		if len(c.Admin.MasterKey) < 16 {
			return fmt.Errorf("admin.master_key must be at least 16 characters")
		}
		if _, _, err := net.SplitHostPort(c.Admin.Listen); err != nil {
			return fmt.Errorf("admin.listen: %w", err)
		}
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
	providers := map[string]bool{}
	for _, p := range c.Providers {
		switch {
		case p.Name == "":
			return fmt.Errorf("provider without name")
		case strings.Contains(p.Name, "/"):
			return fmt.Errorf("provider %q: name must not contain '/'", p.Name)
		case providers[p.Name]:
			return fmt.Errorf("provider %q defined twice", p.Name)
		case p.Type != "openai" && p.Type != "openrouter":
			return fmt.Errorf("provider %q: type %q is not supported (supported: openai, openrouter)", p.Name, p.Type)
		case p.BaseURL == "":
			return fmt.Errorf("provider %q: base_url is required", p.Name)
		}
		providers[p.Name] = true
	}
	models := map[string]bool{}
	for _, m := range c.Models {
		switch {
		case m.Name == "":
			return fmt.Errorf("model without name")
		case models[m.Name]:
			return fmt.Errorf("model %q defined twice", m.Name)
		case !providers[m.Provider]:
			return fmt.Errorf("model %q: unknown provider %q", m.Name, m.Provider)
		}
		models[m.Name] = true
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
