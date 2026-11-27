package provider

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-foundation/gateway/internal/config"
)

// fakeAccounts starts one upstream per account; status(name) picks each answer.
type fakeAccounts struct {
	mu     sync.Mutex
	status map[string]func() (int, http.Header, string)
	calls  []string
}

func (f *fakeAccounts) server(t *testing.T, name string) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, name)
		fn := f.status[name]
		f.mu.Unlock()
		code, hdr, body := 200, http.Header{}, `{"ok":"`+name+`"}`
		if fn != nil {
			code, hdr, body = fn()
		}
		for k, v := range hdr {
			w.Header()[k] = v
		}
		w.WriteHeader(code)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *fakeAccounts) set(name string, code int, hdr http.Header, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if code == 0 {
		delete(f.status, name)
		return
	}
	f.status[name] = func() (int, http.Header, string) { return code, hdr, body }
}

type countObs struct {
	mu  sync.Mutex
	got map[string]int
}

func (o *countObs) UpstreamAttempt(p, a, outcome string) {
	o.mu.Lock()
	o.got[a+":"+outcome]++
	o.mu.Unlock()
}

func w(n int) *int { return &n }

// newTestPool builds a pool of accounts (name, weight; weight -1 = not set).
func newTestPool(t *testing.T, typ, policy, sticky string, accounts ...any) (*Pool, *fakeAccounts, *countObs) {
	t.Helper()
	f := &fakeAccounts{status: map[string]func() (int, http.Header, string){}}
	c := config.Provider{Name: "p", Type: typ, Policy: policy, Sticky: sticky, Cooldown: 30 * time.Second, OpenAIFormat: "compat"}
	for i := 0; i < len(accounts); i += 2 {
		name := accounts[i].(string)
		a := config.Account{Name: name, APIKey: "key-" + name, BaseURL: f.server(t, name)}
		if wt := accounts[i+1].(int); wt >= 0 {
			a.Weight = w(wt)
		}
		c.Accounts = append(c.Accounts, a)
	}
	obs := &countObs{got: map[string]int{}}
	ps, err := Build([]config.Provider{c}, BuildOptions{Client: http.DefaultClient, Observer: obs})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := AsPool(ps["p"])
	return pool, f, obs
}

func call(t *testing.T, p *Pool, id Identity) (int, string, *Trace) {
	t.Helper()
	ctx, tr := WithTrace(WithIdentity(context.Background(), id))
	resp, err := p.Do(ctx, "/chat/completions", []byte(`{}`))
	if err != nil {
		return 0, err.Error(), tr
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), tr
}

func firstPicks(p *Pool, n int, id Identity) map[string]int {
	got := map[string]int{}
	for range n {
		got[p.order(id)[0].name]++
	}
	return got
}

func TestRoundRobinWeights(t *testing.T) {
	p, _, _ := newTestPool(t, "openai", config.PolicyRoundRobin, config.StickyNone, "a", 3, "b", 1)
	var seq []string
	for range 4 {
		seq = append(seq, p.order(Identity{})[0].name)
	}
	if strings.Join(seq, "") != "aaba" {
		t.Fatalf("one cycle of 3:1 is %v, want a a b a (smooth)", seq)
	}
	if got := firstPicks(p, 1000, Identity{}); got["a"] != 750 || got["b"] != 250 {
		t.Fatalf("1000 picks of 3:1: %v", got)
	}

	// A resting account leaves the cycle; its share goes to the others by weight.
	p3, _, _ := newTestPool(t, "openai", config.PolicyRoundRobin, config.StickyNone, "a", 2, "b", 1, "c", 1)
	p3.members[0].until = time.Now().Add(time.Hour)
	if got := firstPicks(p3, 1000, Identity{}); got["a"] != 0 || got["b"] != 500 || got["c"] != 500 {
		t.Fatalf("share with a in cooldown: %v", got)
	}

	// weight 0 drains: never first, still the last fallback.
	pd, _, _ := newTestPool(t, "openai", config.PolicyRoundRobin, config.StickyNone, "a", 1, "old", 0)
	if got := firstPicks(pd, 100, Identity{}); got["old"] != 0 {
		t.Fatalf("drained account picked first: %v", got)
	}
	if o := pd.order(Identity{}); len(o) != 2 || o[1].name != "old" {
		t.Fatalf("drained account is not the last fallback: %v", names(o))
	}
}

func names(ms []*member) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.name)
	}
	return out
}

func TestStickySpread(t *testing.T) {
	p, _, _ := newTestPool(t, "openai", config.PolicyRoundRobin, config.StickyKey, "a", 3, "b", 1)
	owner := map[string]string{}
	count := map[string]int{}
	for i := range 2000 {
		key := fmt.Sprintf("key_%d", i)
		first := p.order(Identity{KeyID: key})[0].name
		if again := p.order(Identity{KeyID: key})[0].name; again != first {
			t.Fatalf("%s moved from %s to %s", key, first, again)
		}
		owner[key] = first
		count[first]++
	}
	if share := float64(count["a"]) / 2000; math.Abs(share-0.75) > 0.04 {
		t.Fatalf("share of a with 3:1 is %.3f", share)
	}
	// Adding an account moves only identities to the new account, never between the old ones.
	p2, _, _ := newTestPool(t, "openai", config.PolicyRoundRobin, config.StickyKey, "a", 3, "b", 1, "c", 1)
	for key, was := range owner {
		if now := p2.order(Identity{KeyID: key})[0].name; now != was && now != "c" {
			t.Fatalf("%s moved from %s to %s", key, was, now)
		}
	}
	// session falls back to the key ID; a resting account sends its identities elsewhere.
	ps, _, _ := newTestPool(t, "openai", config.PolicyRoundRobin, config.StickySession, "a", 1, "b", 1)
	if ps.sel.stickyKey(Identity{KeyID: "k"}) != "k" || ps.sel.stickyKey(Identity{KeyID: "k", Session: "s1"}) != "s:s1" {
		t.Fatal("session sticky key")
	}
}

func TestPriorityFailover(t *testing.T) {
	p, f, obs := newTestPool(t, "openai", config.PolicyPriority, config.StickyNone, "a", -1, "b", -1)
	f.set("a", 429, http.Header{"Retry-After": {"5"}}, `{"error":"slow down"}`)
	code, body, tr := call(t, p, Identity{})
	if code != 200 || !strings.Contains(body, `"b"`) || tr.Account != "b" || tr.Attempts != 2 {
		t.Fatalf("failover: %d %s %+v", code, body, tr)
	}
	st := p.State()
	if a := st.Accounts[0]; a.State != "cooldown" || a.CooldownUntil == nil || time.Until(*a.CooldownUntil) > 6*time.Second || !strings.HasPrefix(a.LastError, "429") {
		t.Fatalf("a after 429: %+v", a)
	}
	// While a rests, b goes first: one call only.
	if _, _, tr := call(t, p, Identity{}); tr.Account != "b" || tr.Attempts != 1 {
		t.Fatalf("while a rests: %+v", tr)
	}
	if obs.got["a:rate_limited"] != 1 || obs.got["b:ok"] != 2 {
		t.Fatalf("observer: %v", obs.got)
	}
	// Every account resting: the one whose rest ends first is still tried.
	f.set("a", 0, nil, "")
	p.members[1].until = time.Now().Add(time.Hour)
	if code, _, tr := call(t, p, Identity{}); code != 200 || tr.Account != "a" || tr.Attempts != 1 {
		t.Fatalf("all resting: %d %+v", code, tr)
	}
}

func TestFailoverRules(t *testing.T) {
	cases := []struct {
		name     string
		typ      string
		code     int
		body     string
		attempts int
		outcome  string
		rest     time.Duration
	}{
		{"server error", "openai", 503, `{}`, 2, OutcomeError, 30 * time.Second},
		{"overloaded", "anthropic", 529, `{}`, 2, OutcomeError, 30 * time.Second},
		{"bad key", "openai", 401, `{}`, 2, OutcomeAuth, authRest},
		{"payment", "openai", 402, `{}`, 2, OutcomeNoCredits, noCreditsRest},
		{"no credits", "anthropic", 400, `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`, 2, OutcomeNoCredits, noCreditsRest},
		{"bad request", "anthropic", 400, `{"error":{"message":"max_tokens: field required"}}`, 1, OutcomeClientError, 0},
		{"not found", "openai", 404, `{}`, 1, OutcomeClientError, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, f, obs := newTestPool(t, tc.typ, config.PolicyPriority, config.StickyNone, "a", -1, "b", -1)
			f.set("a", tc.code, nil, tc.body)
			code, body, tr := call(t, p, Identity{})
			if tr.Attempts != tc.attempts || obs.got["a:"+tc.outcome] != 1 {
				t.Fatalf("attempts %d, observer %v", tr.Attempts, obs.got)
			}
			if tc.attempts == 1 && (code != tc.code || body != tc.body) {
				t.Fatalf("client gets the upstream answer unchanged: %d %s", code, body)
			}
			until := p.members[0].until
			if tc.rest == 0 && !until.IsZero() || tc.rest > 0 && math.Abs(time.Until(until).Seconds()-tc.rest.Seconds()) > 2 {
				t.Fatalf("rest until %v, want %v", until, tc.rest)
			}
		})
	}
}

// When every attempt fails, the client gets the last failure unchanged (spec-0004 section 5.4).
func TestLastFailureUnchanged(t *testing.T) {
	p, f, _ := newTestPool(t, "anthropic", config.PolicyPriority, config.StickyNone, "a", -1, "b", -1)
	credit := `{"error":{"message":"Your credit balance is too low"}}`
	f.set("a", 503, nil, `{"from":"a"}`)
	f.set("b", 400, nil, credit)
	code, body, tr := call(t, p, Identity{})
	if code != 400 || body != credit || tr.Account != "b" || tr.Attempts != 2 {
		t.Fatalf("last failure: %d %s %+v", code, body, tr)
	}
}

func TestMaxAttemptsAndCancel(t *testing.T) {
	p, f, _ := newTestPool(t, "openai", config.PolicyPriority, config.StickyNone, "a", -1, "b", -1, "c", -1)
	p.maxAttempts = 2
	for _, n := range []string{"a", "b", "c"} {
		f.set(n, 500, nil, `{}`)
	}
	if _, _, tr := call(t, p, Identity{}); tr.Attempts != 2 {
		t.Fatalf("max_attempts 2: %d attempts", tr.Attempts)
	}

	p2, f2, _ := newTestPool(t, "openai", config.PolicyPriority, config.StickyNone, "a", -1, "b", -1)
	f2.set("a", 500, nil, `{}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, tr := WithTrace(ctx)
	if _, err := p2.Do(ctx, "/chat/completions", nil); err == nil || tr.Attempts != 1 || !p2.members[0].until.IsZero() {
		t.Fatalf("canceled request: err %v, attempts %d, a rests %v", err, tr.Attempts, !p2.members[0].until.IsZero())
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for h, want := range map[string]time.Duration{
		"":      30 * time.Second,
		"7":     7 * time.Second,
		"99999": maxRetryAfter,
		now.Add(time.Minute).Format(http.TimeFormat): time.Minute,
		"soon": 30 * time.Second,
	} {
		if got := retryAfter(h, now, 30*time.Second); got != want {
			t.Errorf("Retry-After %q: %v, want %v", h, got, want)
		}
	}
}

func TestInherit(t *testing.T) {
	old, _, _ := newTestPool(t, "openai", config.PolicyRoundRobin, config.StickyNone, "a", 3, "b", 1)
	old.members[0].until, old.members[0].lastErr = time.Now().Add(time.Minute), "429 Too Many Requests"
	old.order(Identity{})
	cur, _, _ := newTestPool(t, "openai", config.PolicyRoundRobin, config.StickyNone, "a", 3, "new", 1)
	cur.Inherit(old)
	if s := cur.State(); s.Accounts[0].State != "cooldown" || s.Accounts[0].LastError == "" || s.Accounts[1].State != "ok" {
		t.Fatalf("after inherit: %+v", s.Accounts)
	}
}

func TestCreditLevels(t *testing.T) {
	for _, c := range []struct {
		spent   float64
		percent float64
		level   string
	}{{0, 0, "ok"}, {79.94, 79.9, "ok"}, {79.96, 80, "warning"}, {99.99, 100, "warning"}, {100, 100, "exhausted"}, {130, 130, "exhausted"}} {
		s := CreditState{Type: "monthly", USD: 100}
		s.SetSpent(c.spent)
		if s.UsedPercent != c.percent || s.Level != c.level {
			t.Errorf("spent %v: %v percent, %q; want %v, %q", c.spent, s.UsedPercent, s.Level, c.percent, c.level)
		}
	}
}

func TestPrepaidLevels(t *testing.T) {
	var s CreditState
	s.SetPrepaid(0, 0, nil) // no top-up yet
	if s.Level != "exhausted" || *s.BalanceUSD != 0 {
		t.Fatalf("empty ledger: %+v", s)
	}
	since := time.Now()
	s.SetPrepaid(100, 85, &since)
	if s.Level != "warning" || *s.BalanceUSD != 15 || *s.TopupsUSD != 100 || s.UsedPercent != 85 {
		t.Fatalf("85 of 100: %+v", s)
	}
	s.SetPrepaid(100, 120, &since)
	if s.Level != "exhausted" || *s.BalanceUSD != -20 {
		t.Fatalf("over: %+v", s)
	}
}
