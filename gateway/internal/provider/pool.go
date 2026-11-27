package provider

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai-foundation/gateway/internal/config"
)

// Outcomes of one upstream attempt (spec-0004 section 7).
const (
	OutcomeOK          = "ok"
	OutcomeRateLimited = "rate_limited"
	OutcomeError       = "error"
	OutcomeAuth        = "auth"
	OutcomeNoCredits   = "no_credits"
	OutcomeClientError = "client_error"
)

// Rest times from spec-0004 section 5.3. The provider's cooldown covers the rest.
const (
	maxRetryAfter  = 10 * time.Minute
	authRest       = 10 * time.Minute
	noCreditsRest  = time.Hour
	maxPeekedError = 64 << 10
)

// Observer counts upstream attempts (metrics.Gateway).
type Observer interface {
	UpstreamAttempt(provider, account, outcome string)
}

// Trace collects what a pool did for one request: the account that gave the
// answer and how many upstream calls it took.
type Trace struct {
	Account  string
	Attempts int
	// Outcome of the last attempt (one of the Outcome values), for the target loop.
	Outcome string
	// Limit caps Attempts across targets and accounts (spec-0005 section 3.4); 0 = none.
	Limit int
}

// TraceFrom returns the request's Trace, or nil.
func TraceFrom(ctx context.Context) *Trace {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	return t
}

// Identity is who sent the request, for sticky selection (spec-0004 section 4.3).
type Identity struct {
	KeyID, EndUser, Session string
}

type traceKey struct{}
type identityKey struct{}

// WithTrace returns a context whose pool calls fill the returned Trace.
func WithTrace(ctx context.Context) (context.Context, *Trace) {
	t := &Trace{}
	return context.WithValue(ctx, traceKey{}, t), t
}

// WithIdentity sets who sends the request, for sticky selection.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// Pool is one provider with its accounts. It picks an account by the provider's policy
// and fails over to the next one on errors another account can fix, before the first
// byte reaches the client (spec-0004 sections 4 and 5, D27, D28). Cooldowns are shared
// with the other nodes through sync (D40); the round-robin position is per node.
type Pool struct {
	name, typ   string
	policy      string
	sticky      string
	maxAttempts int
	cooldown    time.Duration
	obs         Observer
	sync        StateSync // nil = this node only
	log         *slog.Logger
	now         func() time.Time
	mu          sync.Mutex
	members     []*member
	sel         *Selector
}

type member struct {
	name   string
	p      Provider
	weight int
	credit *config.Credit // nil = not tracked
	// state, under Pool.mu
	until   time.Time
	lastErr string
	lastAt  time.Time
	changed time.Time // when the state last changed, here or on another node
}

// NativePool is a Pool of providers that also serve their own API format (Anthropic).
type NativePool struct{ *Pool }

var (
	_ Provider = (*Pool)(nil)
	_ Native   = (*NativePool)(nil)
)

func newPool(c config.Provider, members []*member, o BuildOptions) *Pool {
	log := o.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.Cooldown == 0 {
		c.Cooldown = 30 * time.Second
	}
	names, weights := make([]string, len(members)), make([]int, len(members))
	for i, m := range members {
		names[i], weights[i] = m.name, m.weight
	}
	return &Pool{name: c.Name, typ: c.Type, policy: c.Policy, sticky: c.Sticky, maxAttempts: c.MaxAttempts,
		cooldown: c.Cooldown, obs: o.Observer, sync: o.Sync, log: log, now: time.Now, members: members,
		sel: NewSelector(c.Policy, c.Sticky, names, weights)}
}

func (p *Pool) Name() string { return p.name }

// Do sends an OpenAI-format request through the policy and failover loop.
func (p *Pool) Do(ctx context.Context, endpoint string, body []byte) (*http.Response, error) {
	return p.run(ctx, func(m Provider) (*http.Response, error) { return m.Do(ctx, endpoint, body) })
}

func (p *NativePool) NativeFormat() string { return config.FormatAnthropic }

// Native sends a native-format request through the policy and failover loop.
func (p *NativePool) Native(ctx context.Context, method, path, query string, header http.Header, body []byte) (*http.Response, error) {
	return p.run(ctx, func(m Provider) (*http.Response, error) {
		return m.(Native).Native(ctx, method, path, query, header, body)
	})
}

// AsPool returns the pool behind a provider from Build.
func AsPool(pr Provider) (*Pool, bool) {
	switch v := pr.(type) {
	case *Pool:
		return v, true
	case *NativePool:
		return v.Pool, true
	}
	return nil, false
}

func (p *Pool) run(ctx context.Context, call func(Provider) (*http.Response, error)) (*http.Response, error) {
	tr, _ := ctx.Value(traceKey{}).(*Trace)
	id, _ := ctx.Value(identityKey{}).(Identity)
	order := p.order(id)
	if p.maxAttempts > 0 && p.maxAttempts < len(order) {
		order = order[:p.maxAttempts]
	}
	var resp *http.Response
	var err error
	for i, m := range order {
		resp, err = call(m.p)
		if tr != nil {
			tr.Account, tr.Attempts, tr.Outcome = m.name, tr.Attempts+1, OutcomeError
		}
		if ctx.Err() != nil { // the client left or the time is up: no outcome to learn from
			return resp, err
		}
		outcome, rest, msg := p.classify(resp, err)
		if tr != nil {
			tr.Outcome = outcome
		}
		if p.obs != nil {
			p.obs.UpstreamAttempt(p.name, m.name, outcome)
		}
		p.record(m, outcome, rest, msg)
		retry := outcome != OutcomeOK && outcome != OutcomeClientError
		if !retry {
			return resp, err
		}
		last := i == len(order)-1 || tr != nil && tr.Limit > 0 && tr.Attempts >= tr.Limit
		level := slog.LevelWarn
		if outcome == OutcomeAuth {
			level = slog.LevelError // a revoked or wrong key needs a person
		}
		p.log.Log(ctx, level, "upstream attempt failed", "provider", p.name, "account", m.name, "outcome", outcome,
			"error", msg, "cooldown", rest.String(), "last_account", last)
		if last {
			return resp, err
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
	return resp, err
}

// classify maps one attempt to an outcome, a rest time and a short error text.
// For an Anthropic 400 it reads the body (up to 64 KiB) to spot "no credits", and
// puts the bytes back so the client still gets them.
func (p *Pool) classify(resp *http.Response, err error) (string, time.Duration, string) {
	if err != nil {
		return OutcomeError, p.cooldown, shorten(err.Error())
	}
	code := resp.StatusCode
	text := strconv.Itoa(code) + " " + http.StatusText(code)
	switch {
	case code >= 200 && code < 300:
		return OutcomeOK, 0, ""
	case code == http.StatusTooManyRequests:
		return OutcomeRateLimited, retryAfter(resp.Header.Get("Retry-After"), p.now(), p.cooldown), text
	case code >= 500:
		return OutcomeError, p.cooldown, text
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return OutcomeAuth, authRest, text
	case code == http.StatusPaymentRequired:
		return OutcomeNoCredits, noCreditsRest, text
	case code == http.StatusBadRequest && p.typ == config.TypeAnthropic:
		head, _ := io.ReadAll(io.LimitReader(resp.Body, maxPeekedError))
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
		if bytes.Contains(bytes.ToLower(head), []byte("credit balance")) {
			return OutcomeNoCredits, noCreditsRest, text + ": credit balance too low"
		}
	}
	return OutcomeClientError, 0, text
}

// retryAfter reads Retry-After (seconds or an HTTP date), capped at 10m.
func retryAfter(h string, now time.Time, fallback time.Duration) time.Duration {
	var d time.Duration
	if s, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && s >= 0 {
		d = time.Duration(s) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = t.Sub(now)
	}
	if d <= 0 {
		return fallback
	}
	return min(d, maxRetryAfter)
}

// record keeps the outcome of one attempt. A rest that starts, or a success that ends a
// rest or clears an error, is shared with the other nodes.
func (p *Pool) record(m *member, outcome string, rest time.Duration, msg string) {
	p.mu.Lock()
	now := p.now()
	changed := false
	switch {
	case outcome == OutcomeOK:
		changed = !m.until.IsZero() || m.lastErr != ""
		m.until, m.lastErr = time.Time{}, ""
	case rest > 0:
		m.until, m.lastErr, m.lastAt = now.Add(rest), msg, now
		changed = true
	}
	var ev AccountEvent
	if changed {
		m.changed = now
		ev = p.event(m)
	}
	p.mu.Unlock()
	if changed && p.sync != nil {
		p.sync.Publish(ev)
	}
}

// order is the list of accounts to try for one request: the policy's first pick, then
// the fallbacks (spec-0004 section 4.1). Accounts in cooldown are skipped; when every
// account rests, the one whose rest ends first is tried anyway.
func (p *Pool) order(id Identity) []*member {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	idx := p.sel.Order(id, func(i int) bool { return !now.Before(p.members[i].until) })
	out := make([]*member, 0, len(idx))
	for _, i := range idx {
		out = append(out, p.members[i])
	}
	if len(out) == 0 && len(p.members) > 0 {
		soonest := p.members[0]
		for _, m := range p.members[1:] {
			if m.until.Before(soonest.until) {
				soonest = m
			}
		}
		out = []*member{soonest}
	}
	return out
}

// Available reports whether at least one account is not resting. A model target whose
// provider has none is skipped (spec-0005 section 3.3).
func (p *Pool) Available() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, m := range p.members {
		if !now.Before(m.until) {
			return true
		}
	}
	return false
}

func shorten(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// AccountState is one account as GET /admin/v1/providers shows it. Never the key.
type AccountState struct {
	Name          string     `json:"name"`
	Weight        int        `json:"weight"`
	State         string     `json:"state"` // ok or cooldown
	CooldownUntil *time.Time `json:"cooldown_until,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	LastErrorAt   *time.Time `json:"last_error_at,omitempty"`
	// Credit is how the account is paid for (task-0010); nil = not tracked. The admin
	// API adds the spend from the usage table and the prepaid ledger.
	Credit *CreditState `json:"credit,omitempty"`
}

// CreditState is how an account is paid for and how much is used (task-0010).
// monthly and payg count the spend of the current cycle against USD; prepaid counts
// the spend since the first ledger entry against the sum of the ledger.
type CreditState struct {
	Type string `json:"type"` // monthly, prepaid or payg
	// monthly and payg
	USD        float64    `json:"usd,omitempty"` // credit (monthly) or budget (payg) of each cycle
	CycleDay   int        `json:"cycle_day,omitempty"`
	Timezone   string     `json:"timezone,omitempty"`
	CycleStart *time.Time `json:"cycle_start,omitempty"`
	CycleEnd   *time.Time `json:"cycle_end,omitempty"`
	// prepaid
	TopupsUSD  *float64   `json:"topups_usd,omitempty"`  // sum of the ledger
	BalanceUSD *float64   `json:"balance_usd,omitempty"` // TopupsUSD - SpentUSD
	Since      *time.Time `json:"since,omitempty"`       // first ledger entry; nil = no entry yet
	// all types
	SpentUSD    float64 `json:"spent_usd"`
	UsedPercent float64 `json:"used_percent"`
	Level       string  `json:"level"` // ok, warning (CreditWarnPercent or more) or exhausted (100 or more)
}

// CreditWarnPercent is where an account's credit shows a warning.
const CreditWarnPercent = 80

// SetSpent fills in the spend of this cycle (monthly, payg).
func (c *CreditState) SetSpent(usd float64) {
	c.SpentUSD = usd
	c.UsedPercent, c.Level = creditLevel(c.USD, usd)
}

// SetPrepaid fills in the ledger sum, the spend since the first entry and the balance.
func (c *CreditState) SetPrepaid(topups, spent float64, since *time.Time) {
	balance := topups - spent
	c.TopupsUSD, c.BalanceUSD, c.Since, c.SpentUSD = &topups, &balance, since, spent
	c.UsedPercent, c.Level = creditLevel(topups, spent)
}

func creditLevel(limit, spent float64) (float64, string) {
	if limit <= 0 {
		return 100, "exhausted"
	}
	pct := math.Round(spent/limit*1000) / 10 // one decimal
	switch {
	case spent >= limit:
		return pct, "exhausted"
	case pct >= CreditWarnPercent:
		return pct, "warning"
	}
	return pct, "ok"
}

// PoolState is a provider with its policy and accounts.
type PoolState struct {
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Policy      string         `json:"policy"`
	Sticky      string         `json:"sticky"`
	MaxAttempts int            `json:"max_attempts"`
	Cooldown    string         `json:"cooldown"`
	Accounts    []AccountState `json:"accounts"`
}

// State reports the policy and every account's state.
func (p *Pool) State() PoolState {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	maxA := p.maxAttempts
	if maxA == 0 {
		maxA = len(p.members)
	}
	s := PoolState{Name: p.name, Type: p.typ, Policy: p.policy, Sticky: p.sticky, MaxAttempts: maxA,
		Cooldown: p.cooldown.String(), Accounts: []AccountState{}}
	for _, m := range p.members {
		a := AccountState{Name: m.name, Weight: m.weight, State: "ok", LastError: m.lastErr}
		if now.Before(m.until) {
			u := m.until.UTC()
			a.State, a.CooldownUntil = "cooldown", &u
		}
		if !m.lastAt.IsZero() {
			at := m.lastAt.UTC()
			a.LastErrorAt = &at
		}
		if c := m.credit; c != nil {
			a.Credit = &CreditState{Type: c.Type, Level: "ok"}
			if c.Periodic() {
				start, end := c.Cycle(now)
				a.Credit.USD, a.Credit.CycleDay, a.Credit.Timezone = c.USD, c.CycleDay, c.Timezone
				a.Credit.CycleStart, a.Credit.CycleEnd = &start, &end
			}
		}
		s.Accounts = append(s.Accounts, a)
	}
	return s
}

// Inherit takes the state of old's accounts with the same names (a catalog reload,
// spec-0006 section 5.6). Renamed or removed accounts start fresh.
func (p *Pool) Inherit(old *Pool) {
	if old == nil || old == p {
		return
	}
	old.mu.Lock()
	prev := make(map[string]member, len(old.members))
	for _, m := range old.members {
		prev[m.name] = *m
	}
	old.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.members {
		if o, ok := prev[m.name]; ok {
			m.until, m.lastErr, m.lastAt, m.changed = o.until, o.lastErr, o.lastAt, o.changed
		}
	}
	p.sel.Inherit(old.sel)
}
