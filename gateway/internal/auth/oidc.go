package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/id"
	"ai-foundation/gateway/internal/store"
)

// OIDC is sign-in through outside identity providers (spec-0001 section 6.3, D41). The
// gateway is only an OIDC client: 2FA, passkeys and "login with Google" are the IdP's.
// A login is accepted only for an active account linked to it (no sign-up).
type OIDC struct {
	svc       *Service
	st        store.Store
	publicURL string
	ttl       time.Duration
	client    *http.Client
	log       *slog.Logger
	now       func() time.Time
	order     []string
	providers map[string]*idp
}

const (
	flowTTL        = 10 * time.Minute
	linkAllowedFor = 24 * time.Hour
	discoveryRetry = 30 * time.Second
)

// ErrUnknownProvider: no IdP with that name in admin.oidc.providers.
var ErrUnknownProvider = errors.New("unknown identity provider")

// UnavailableError: the IdP's discovery document cannot be read now.
type UnavailableError struct{ Provider string }

func (e *UnavailableError) Error() string {
	return "the identity provider " + e.Provider + " cannot be reached; try again in a minute"
}

// RefusedError is a sign-in that is not accepted. Msg is shown to the user; Reason is logged.
type RefusedError struct{ Msg, Reason string }

func (e *RefusedError) Error() string { return e.Msg }

func refused(msg, reason string) error { return &RefusedError{Msg: msg, Reason: reason} }

type idp struct {
	cfg     config.OIDCProvider
	mu      sync.Mutex
	op      *oidc.Provider // nil until discovery works
	lastTry time.Time
}

// NewOIDC reads each provider's discovery document. A provider that cannot be reached
// is logged and tried again on its next sign-in. It returns nil when none is set.
func NewOIDC(ctx context.Context, svc *Service, st store.Store, a config.Admin, client *http.Client, log *slog.Logger) *OIDC {
	if len(a.OIDC.Providers) == 0 {
		return nil
	}
	o := &OIDC{svc: svc, st: st, publicURL: a.PublicURL, ttl: a.OIDC.SessionTTL, client: client, log: log,
		now: time.Now, providers: map[string]*idp{}}
	for _, p := range a.OIDC.Providers {
		o.order = append(o.order, p.Name)
		o.providers[p.Name] = &idp{cfg: p}
	}
	for _, name := range o.order {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _ = o.discover(dctx, o.providers[name])
		cancel()
	}
	return o
}

// ProviderInfo is one sign-in button.
type ProviderInfo struct {
	Name  string `json:"name"`
	Title string `json:"title"`
}

func (o *OIDC) Providers() []ProviderInfo {
	if o == nil {
		return []ProviderInfo{}
	}
	out := make([]ProviderInfo, 0, len(o.order))
	for _, name := range o.order {
		out = append(out, ProviderInfo{Name: name, Title: o.providers[name].cfg.Title})
	}
	return out
}

func (o *OIDC) discover(ctx context.Context, p *idp) (*oidc.Provider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.op != nil {
		return p.op, nil
	}
	if !p.lastTry.IsZero() && o.now().Sub(p.lastTry) < discoveryRetry {
		return nil, &UnavailableError{Provider: p.cfg.Name}
	}
	p.lastTry = o.now()
	op, err := oidc.NewProvider(oidc.ClientContext(ctx, o.client), p.cfg.Issuer)
	if err != nil {
		o.log.Warn("oidc discovery failed; the next sign-in tries again", "provider", p.cfg.Name, "issuer", p.cfg.Issuer, "err", err)
		return nil, &UnavailableError{Provider: p.cfg.Name}
	}
	o.log.Info("oidc provider ready", "provider", p.cfg.Name, "issuer", p.cfg.Issuer)
	p.op = op
	return op, nil
}

func (o *OIDC) lookup(ctx context.Context, name string) (*idp, *oidc.Provider, error) {
	if o == nil {
		return nil, nil, ErrUnknownProvider
	}
	p, ok := o.providers[name]
	if !ok {
		return nil, nil, ErrUnknownProvider
	}
	op, err := o.discover(ctx, p)
	return p, op, err
}

func (o *OIDC) oauth(p *idp, op *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{ClientID: p.cfg.ClientID, ClientSecret: p.cfg.ClientSecret, Endpoint: op.Endpoint(),
		RedirectURL: o.publicURL + "/admin/v1/auth/oidc/" + p.cfg.Name + "/callback", Scopes: p.cfg.Scopes}
}

// Start begins a sign-in, or a link to linkUserID when it is set. It returns the IdP URL
// to send the browser to, and the flow token for the flow cookie.
func (o *OIDC) Start(ctx context.Context, name, linkUserID string) (authURL, flowToken string, err error) {
	p, op, err := o.lookup(ctx, name)
	if err != nil {
		return "", "", err
	}
	flowToken = "aifo_" + randomHex(32)
	state, nonce, verifier := randomHex(16), randomHex(16), oauth2.GenerateVerifier()
	if err := o.st.CreateOIDCFlow(ctx, &store.OIDCFlow{IDHash: hashToken(flowToken), Provider: name, State: state,
		Nonce: nonce, PKCEVerifier: verifier, LinkUserID: linkUserID, ExpiresAt: o.now().UTC().Add(flowTTL)}); err != nil {
		return "", "", err
	}
	return o.oauth(p, op).AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), flowToken, nil
}

// CallbackResult is a finished flow: a new session (Token), or a new link (Linked).
type CallbackResult struct {
	Token  string
	User   *store.AdminUser
	Linked bool
}

// Callback finishes a flow. The flow row is used once, whatever the outcome.
func (o *OIDC) Callback(ctx context.Context, name, flowToken, state, code, userAgent, ip string) (*CallbackResult, error) {
	if flowToken == "" {
		return nil, refused("the sign-in was not started in this browser; try again", "no flow cookie")
	}
	flow, err := o.st.TakeOIDCFlow(ctx, hashToken(flowToken), o.now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		return nil, refused("the sign-in took too long or was already used; try again", "flow missing or expired")
	}
	if err != nil {
		return nil, err
	}
	if flow.Provider != name || subtle.ConstantTimeCompare([]byte(flow.State), []byte(state)) != 1 {
		return nil, refused("the sign-in could not be checked; try again", "state or provider mismatch")
	}
	p, op, err := o.lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	if code == "" {
		return nil, refused("the identity provider sent no code; try again", "no code")
	}
	cctx := oidc.ClientContext(ctx, o.client)
	tok, err := o.oauth(p, op).Exchange(cctx, code, oauth2.VerifierOption(flow.PKCEVerifier))
	if err != nil {
		return nil, refused("the identity provider did not accept the sign-in; try again", "code exchange: "+err.Error())
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, refused("the identity provider sent no ID token", "no id_token")
	}
	idt, err := op.Verifier(&oidc.Config{ClientID: p.cfg.ClientID}).Verify(cctx, raw)
	if err != nil {
		return nil, refused("the identity provider's token is not valid", "id_token: "+err.Error())
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(flow.Nonce)) != 1 {
		return nil, refused("the sign-in could not be checked; try again", "nonce mismatch")
	}
	c, err := o.claims(cctx, p, op, idt, tok)
	if err != nil {
		return nil, err
	}
	if len(p.cfg.AllowedGroups) > 0 && !slices.ContainsFunc(c.groups, func(g string) bool { return slices.Contains(p.cfg.AllowedGroups, g) }) {
		return nil, refused("your "+p.cfg.Title+" account is not in a group that may use the admin site",
			fmt.Sprintf("not in allowed_groups (claim %q has %v)", p.cfg.GroupsClaim, c.groups))
	}
	if flow.LinkUserID != "" {
		u, err := o.link(ctx, p, idt, c, flow.LinkUserID)
		if err != nil {
			return nil, err
		}
		return &CallbackResult{User: u, Linked: true}, nil
	}
	u, err := o.account(ctx, p, idt, c)
	if err != nil {
		return nil, err
	}
	token, err := o.svc.issueSession(ctx, u, userAgent, ip, o.ttl)
	if err != nil {
		return nil, err
	}
	now := o.now().UTC()
	_ = o.st.SetAdminLastLogin(ctx, u.ID, now)
	u.LastLoginAt = &now
	o.svc.Mark(u)
	o.log.Info("admin sign-in through oidc", "provider", name, "user", u.ID, "email", u.Email, "ip", ip)
	return &CallbackResult{Token: token, User: u}, nil
}

type claims struct {
	email    string
	verified bool
	groups   []string
}

// claims reads email, email_verified and the groups claim from the ID token, and from
// the userinfo endpoint for what the token lacks.
func (o *OIDC) claims(ctx context.Context, p *idp, op *oidc.Provider, idt *oidc.IDToken, tok *oauth2.Token) (claims, error) {
	var m map[string]any
	if err := idt.Claims(&m); err != nil {
		return claims{}, refused("the identity provider's token is not valid", "claims: "+err.Error())
	}
	needGroups := len(p.cfg.AllowedGroups) > 0
	_, hasEmail := m["email"]
	_, hasVerified := m["email_verified"]
	_, hasGroups := m[p.cfg.GroupsClaim]
	if (!hasEmail || !hasVerified || (needGroups && !hasGroups)) && op.UserInfoEndpoint() != "" {
		ui, err := op.UserInfo(ctx, oauth2.StaticTokenSource(tok))
		if err != nil {
			o.log.Warn("oidc userinfo failed", "provider", p.cfg.Name, "err", err)
		} else if ui.Subject == idt.Subject {
			var um map[string]any
			if ui.Claims(&um) == nil {
				for k, v := range um {
					if _, ok := m[k]; !ok {
						m[k] = v
					}
				}
			}
		}
	}
	c := claims{verified: truthy(m["email_verified"])}
	c.email, _ = m["email"].(string)
	c.email = normalizeEmail(c.email)
	switch g := m[p.cfg.GroupsClaim].(type) {
	case []any:
		for _, v := range g {
			if s, ok := v.(string); ok {
				c.groups = append(c.groups, s)
			}
		}
	case string:
		c.groups = []string{g}
	}
	return c, nil
}

func truthy(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b == "true"
	}
	return false
}

// account finds the account for a login: by its link, or by a one-time link allowance
// (or link_by_email) and a verified email, which then adds the link.
func (o *OIDC) account(ctx context.Context, p *idp, idt *oidc.IDToken, c claims) (*store.AdminUser, error) {
	notLinked := refused("no admin account is linked to this login; ask an admin to allow a link for your account",
		"no link for "+idt.Issuer+" "+idt.Subject+" ("+c.email+")")
	ident, err := o.st.GetAdminIdentity(ctx, idt.Issuer, idt.Subject)
	switch {
	case err == nil:
		u, err := o.st.GetAdminUser(ctx, ident.UserID)
		if err != nil {
			return nil, err
		}
		if u.DisabledAt != nil || o.svc.IsLocal(u) {
			return nil, refused("this admin account is disabled", "account disabled or local: "+u.ID)
		}
		_ = o.st.SetAdminIdentityLogin(ctx, ident.ID, c.email, o.now().UTC())
		return u, nil
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	if c.email == "" || !c.verified {
		return nil, notLinked
	}
	u, err := o.st.GetAdminUserByEmail(ctx, c.email)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notLinked
	}
	if err != nil {
		return nil, err
	}
	if u.DisabledAt != nil || o.svc.IsLocal(u) {
		return nil, notLinked
	}
	allowed, err := o.st.ClaimAdminLink(ctx, u.ID, o.now().UTC())
	if err != nil {
		return nil, err
	}
	if !allowed && !p.cfg.LinkByEmail {
		return nil, notLinked
	}
	if err := o.addLink(ctx, p, idt, c, u); err != nil {
		return nil, err
	}
	o.log.Info("oidc login linked to an admin account", "provider", p.cfg.Name, "user", u.ID, "email", u.Email,
		"by", map[bool]string{true: "one-time allowance", false: "link_by_email"}[allowed])
	return u, nil
}

// link adds the login to a signed-in user (start?link=1).
func (o *OIDC) link(ctx context.Context, p *idp, idt *oidc.IDToken, c claims, userID string) (*store.AdminUser, error) {
	u, err := o.st.GetAdminUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, refused("your account no longer exists", "link: user gone")
	}
	if err != nil {
		return nil, err
	}
	if u.DisabledAt != nil || o.svc.IsLocal(u) {
		return nil, refused("this account cannot be linked", "link: disabled or local")
	}
	if err := o.addLink(ctx, p, idt, c, u); err != nil {
		return nil, err
	}
	o.log.Info("oidc login linked by its user", "provider", p.cfg.Name, "user", u.ID)
	o.svc.Mark(u)
	return u, nil
}

func (o *OIDC) addLink(ctx context.Context, p *idp, idt *oidc.IDToken, c claims, u *store.AdminUser) error {
	now := o.now().UTC()
	err := o.st.CreateAdminIdentity(ctx, &store.AdminIdentity{ID: id.New("idn_"), UserID: u.ID, Provider: p.cfg.Name,
		Issuer: idt.Issuer, Subject: idt.Subject, Email: c.email, CreatedAt: now, LastLoginAt: &now})
	if errors.Is(err, store.ErrConflict) {
		return refused("this login is already linked to another admin account", "link conflict")
	}
	return err
}

// ---- links, for the admin API ----

// AllowLink opens the one-time link for an account for 24h.
func (s *Service) AllowLink(ctx context.Context, userID string) (*store.AdminUser, error) {
	u, err := s.st.GetAdminUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if s.IsLocal(u) {
		return nil, inputErr("the local account cannot be linked to an identity provider")
	}
	if u.DisabledAt != nil {
		return nil, inputErr("enable the account first")
	}
	until := s.now().UTC().Add(linkAllowedFor)
	if err := s.st.AllowAdminLink(ctx, u.ID, &until); err != nil {
		return nil, err
	}
	u.LinkAllowedUntil = &until
	s.Mark(u)
	return u, nil
}

func (s *Service) ListLinks(ctx context.Context, userID string) ([]store.AdminIdentity, error) {
	if _, err := s.st.GetAdminUser(ctx, userID); err != nil {
		return nil, err
	}
	return s.st.ListAdminIdentities(ctx, userID)
}

func (s *Service) DeleteLink(ctx context.Context, userID, identityID string) error {
	return s.st.DeleteAdminIdentity(ctx, userID, identityID)
}
