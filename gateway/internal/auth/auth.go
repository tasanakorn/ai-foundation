// Package auth is the built-in admin login: accounts with email + password, browser
// sessions, and roles. It is the first way to sign in. Outside logins ("login with ...")
// can be added later; they only need to map an identity to an AdminUser and call
// IssueSession.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"slices"
	"strings"
	"sync"
	"time"

	"ai-foundation/gateway/internal/config"
	"ai-foundation/gateway/internal/id"
	"ai-foundation/gateway/internal/store"
)

var (
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrTooManyAttempts    = errors.New("too many failed sign-in attempts; try again later")
	ErrNoSession          = errors.New("not signed in")
)

// InputError is a bad request from the caller (shown to the user as is).
type InputError struct {
	Msg string
	Err error // optional cause, e.g. store.ErrConflict
}

func (e *InputError) Error() string { return e.Msg }
func (e *InputError) Unwrap() error { return e.Err }

func inputErr(format string, a ...any) error { return &InputError{Msg: fmt.Sprintf(format, a...)} }

var Roles = []string{store.RoleAdmin, store.RoleViewer}

type Service struct {
	st      store.Store
	ttl     time.Duration
	log     *slog.Logger
	limiter *limiter
	now     func() time.Time
}

func NewService(st store.Store, sessionTTL time.Duration, log *slog.Logger) *Service {
	return &Service{st: st, ttl: sessionTTL, log: log, limiter: newLimiter(), now: time.Now}
}

// TTL is how long a new session lasts.
func (s *Service) TTL() time.Duration { return s.ttl }

// ---- sign in ----

// Login checks email + password and starts a session. It returns the session token
// (the cookie value; only its hash is stored).
func (s *Service) Login(ctx context.Context, email, password, userAgent, ip string) (string, *store.AdminUser, error) {
	email = normalizeEmail(email)
	if !s.limiter.allow("email:"+email) || !s.limiter.allow("ip:"+ip) {
		return "", nil, ErrTooManyAttempts
	}
	u, err := s.st.GetAdminUserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		_, _ = VerifyPassword(dummyHash(), password) // same time as a real check
		s.fail(email, ip)
		return "", nil, ErrInvalidCredentials
	}
	if err != nil {
		return "", nil, err
	}
	ok := false
	if u.PasswordHash != "" {
		if ok, err = VerifyPassword(u.PasswordHash, password); err != nil {
			s.log.Warn("admin password hash unreadable", "user", u.ID, "err", err)
		}
	}
	if !ok || u.DisabledAt != nil {
		s.fail(email, ip)
		return "", nil, ErrInvalidCredentials
	}
	s.limiter.reset("email:" + email)
	token, err := s.IssueSession(ctx, u, userAgent, ip)
	if err != nil {
		return "", nil, err
	}
	now := s.now().UTC()
	_ = s.st.SetAdminLastLogin(ctx, u.ID, now)
	u.LastLoginAt = &now
	return token, u, nil
}

func (s *Service) fail(email, ip string) {
	s.limiter.fail("email:" + email)
	s.limiter.fail("ip:" + ip)
}

// IssueSession starts a session for an already authenticated user.
// Outside login methods (OIDC, SSO) call this after they verify the identity.
func (s *Service) IssueSession(ctx context.Context, u *store.AdminUser, userAgent, ip string) (string, error) {
	if u.DisabledAt != nil {
		return "", ErrInvalidCredentials
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := "aifs_" + hex.EncodeToString(b[:])
	now := s.now().UTC()
	if len(userAgent) > 256 {
		userAgent = userAgent[:256]
	}
	if err := s.st.CreateAdminSession(ctx, &store.AdminSession{
		Hash: hashToken(token), UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(s.ttl), UserAgent: userAgent, IP: ip,
	}); err != nil {
		return "", err
	}
	_ = s.st.DeleteExpiredAdminSessions(ctx, now) // housekeeping; cheap with the index
	return token, nil
}

// Session returns the signed-in user for a session token.
func (s *Service) Session(ctx context.Context, token string) (*store.AdminUser, *store.AdminSession, error) {
	if token == "" {
		return nil, nil, ErrNoSession
	}
	sess, err := s.st.GetAdminSession(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, ErrNoSession
	}
	if err != nil {
		return nil, nil, err
	}
	u, err := s.st.GetAdminUser(ctx, sess.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, ErrNoSession
	}
	if err != nil {
		return nil, nil, err
	}
	if u.DisabledAt != nil {
		return nil, nil, ErrNoSession
	}
	return u, sess, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.st.DeleteAdminSession(ctx, hashToken(token))
}

// ChangePassword is the signed-in user's own change. Other sessions of the user end;
// keepSession (the current one) stays.
func (s *Service) ChangePassword(ctx context.Context, u *store.AdminUser, current, next, keepSession string) error {
	ok, _ := VerifyPassword(u.PasswordHash, current)
	if !ok {
		return inputErr("current password is wrong")
	}
	if err := checkPassword(next); err != nil {
		return err
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	u.PasswordHash, u.UpdatedAt = hash, s.now().UTC()
	if err := s.st.UpdateAdminUser(ctx, u); err != nil {
		return err
	}
	return s.st.DeleteAdminSessions(ctx, u.ID, keepSession)
}

// ---- accounts ----

type CreateUserInput struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Password string `json:"password"`
}

func (s *Service) CreateUser(ctx context.Context, in CreateUserInput) (*store.AdminUser, error) {
	email := normalizeEmail(in.Email)
	if err := checkEmail(email); err != nil {
		return nil, err
	}
	if in.Role == "" {
		in.Role = store.RoleAdmin
	}
	if !slices.Contains(Roles, in.Role) {
		return nil, inputErr("role must be one of: %s", strings.Join(Roles, ", "))
	}
	if err := checkPassword(in.Password); err != nil {
		return nil, err
	}
	hash, err := HashPassword(in.Password)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	u := &store.AdminUser{ID: id.New("usr_"), Email: email, Name: strings.TrimSpace(in.Name), Role: in.Role,
		PasswordHash: hash, CreatedAt: now, UpdatedAt: now}
	if err := s.st.CreateAdminUser(ctx, u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, &InputError{Msg: "an account with email " + email + " already exists", Err: store.ErrConflict}
		}
		return nil, err
	}
	return u, nil
}

// UpdateUserInput changes only the fields that are set.
type UpdateUserInput struct {
	Name     *string `json:"name"`
	Role     *string `json:"role"`
	Disabled *bool   `json:"disabled"`
	Password *string `json:"password"`
}

// UpdateUser is an admin changing an account. actorID is the signed-in admin ("" for the master key).
func (s *Service) UpdateUser(ctx context.Context, userID, actorID string, in UpdateUserInput) (*store.AdminUser, error) {
	u, err := s.st.GetAdminUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	wasActiveAdmin := u.Role == store.RoleAdmin && u.DisabledAt == nil
	endSessions := false
	if in.Name != nil {
		u.Name = strings.TrimSpace(*in.Name)
	}
	if in.Role != nil {
		if !slices.Contains(Roles, *in.Role) {
			return nil, inputErr("role must be one of: %s", strings.Join(Roles, ", "))
		}
		u.Role = *in.Role
	}
	if in.Disabled != nil {
		if *in.Disabled && userID == actorID {
			return nil, inputErr("you cannot disable your own account")
		}
		switch {
		case *in.Disabled && u.DisabledAt == nil:
			now := s.now().UTC()
			u.DisabledAt, endSessions = &now, true
		case !*in.Disabled:
			u.DisabledAt = nil
		}
	}
	if in.Password != nil {
		if err := checkPassword(*in.Password); err != nil {
			return nil, err
		}
		if u.PasswordHash, err = HashPassword(*in.Password); err != nil {
			return nil, err
		}
		endSessions = true
	}
	if wasActiveAdmin && (u.Role != store.RoleAdmin || u.DisabledAt != nil) {
		if err := s.keepOneAdmin(ctx); err != nil {
			return nil, err
		}
	}
	u.UpdatedAt = s.now().UTC()
	if err := s.st.UpdateAdminUser(ctx, u); err != nil {
		return nil, err
	}
	if endSessions {
		if err := s.st.DeleteAdminSessions(ctx, u.ID, ""); err != nil {
			return nil, err
		}
	}
	return u, nil
}

func (s *Service) DeleteUser(ctx context.Context, userID, actorID string) error {
	if userID == actorID {
		return inputErr("you cannot delete your own account")
	}
	u, err := s.st.GetAdminUser(ctx, userID)
	if err != nil {
		return err
	}
	if u.Role == store.RoleAdmin && u.DisabledAt == nil {
		if err := s.keepOneAdmin(ctx); err != nil {
			return err
		}
	}
	return s.st.DeleteAdminUser(ctx, userID) // sessions go with it (ON DELETE CASCADE)
}

// keepOneAdmin fails when the change would leave no active admin account.
func (s *Service) keepOneAdmin(ctx context.Context) error {
	n, err := s.st.CountActiveAdmins(ctx)
	if err != nil {
		return err
	}
	if n <= 1 {
		return inputErr("this is the last active admin account; add another admin first")
	}
	return nil
}

// ResetPassword sets a new password by email and ends all sessions (CLI recovery).
func (s *Service) ResetPassword(ctx context.Context, email, password string) (*store.AdminUser, error) {
	u, err := s.st.GetAdminUserByEmail(ctx, normalizeEmail(email))
	if err != nil {
		return nil, err
	}
	return s.UpdateUser(ctx, u.ID, "", UpdateUserInput{Password: &password})
}

// Bootstrap creates the first admin account when there is none yet. It is safe when
// several nodes start at once: the unique email lets only one create it.
func (s *Service) Bootstrap(ctx context.Context, b config.AdminBootstrap) (bool, error) {
	if b.Email == "" {
		return false, nil
	}
	users, err := s.st.ListAdminUsers(ctx)
	if err != nil || len(users) > 0 {
		return false, err
	}
	_, err = s.CreateUser(ctx, CreateUserInput{Email: b.Email, Name: b.Name, Role: store.RoleAdmin, Password: b.Password})
	if errors.Is(err, store.ErrConflict) {
		return false, nil // another node won
	}
	return err == nil, err
}

func (s *Service) HasUsers(ctx context.Context) (bool, error) {
	users, err := s.st.ListAdminUsers(ctx)
	return len(users) > 0, err
}

// ---- helpers ----

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func checkEmail(email string) error {
	a, err := mail.ParseAddress(email)
	if err != nil || a.Address != email || a.Name != "" {
		return inputErr("%q is not a valid email address", email)
	}
	return nil
}

func checkPassword(p string) error {
	switch {
	case len(p) < config.MinPasswordLen:
		return inputErr("password must be at least %d characters", config.MinPasswordLen)
	case len(p) > 256:
		return inputErr("password must be at most 256 characters")
	}
	return nil
}

// limiter counts failed sign-ins per key (email, IP) in a fixed window. Per node only:
// with N nodes an attacker gets N times the budget, which is still slow enough.
type limiter struct {
	mu     sync.Mutex
	window time.Duration
	max    map[string]int // by key prefix
	hits   map[string]*window
	now    func() time.Time
}

type window struct {
	start time.Time
	fails int
}

func newLimiter() *limiter {
	return &limiter{window: 15 * time.Minute, max: map[string]int{"email": 10, "ip": 100},
		hits: map[string]*window{}, now: time.Now}
}

func (l *limiter) limit(key string) int {
	prefix, _, _ := strings.Cut(key, ":")
	return l.max[prefix]
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.hits[key]
	if w == nil || l.now().Sub(w.start) > l.window {
		return true
	}
	return w.fails < l.limit(key)
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.hits) > 10000 { // drop old windows so memory stays small
		for k, w := range l.hits {
			if now.Sub(w.start) > l.window {
				delete(l.hits, k)
			}
		}
	}
	w := l.hits[key]
	if w == nil || now.Sub(w.start) > l.window {
		w = &window{start: now}
		l.hits[key] = w
	}
	w.fails++
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}
