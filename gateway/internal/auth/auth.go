// Package auth is admin sign-in: one local account with a password (break-glass,
// spec-0001 D42), outside logins through OIDC (oidc.go, D41), browser sessions, and roles.
// Every way to sign in ends in IssueSession.
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
	ErrPasswordLoginOff   = errors.New("password sign-in is turned off; sign in with your identity provider")
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

// Audit is told about every use of the local account's password or of the master key
// (method "password" or "master_key"). It must not block.
type Audit func(method, userID, email, ip string)

type Options struct {
	SessionTTL    time.Duration
	LocalEmail    string // admin.bootstrap.email; "" = no local account
	PasswordLogin bool   // admin.password_login
	Audit         Audit  // optional
	Log           *slog.Logger
}

type Service struct {
	st            store.Store
	ttl           time.Duration
	localEmail    string
	passwordLogin bool
	audit         Audit
	log           *slog.Logger
	limiter       *limiter
	now           func() time.Time

	mu            sync.Mutex
	lastMasterKey time.Time // last master-key audit event from this node
}

func NewService(st store.Store, o Options) *Service {
	if o.Audit == nil {
		o.Audit = func(string, string, string, string) {}
	}
	return &Service{st: st, ttl: o.SessionTTL, localEmail: normalizeEmail(o.LocalEmail), passwordLogin: o.PasswordLogin,
		audit: o.Audit, log: o.Log, limiter: newLimiter(), now: time.Now}
}

// TTL is how long a new password session lasts.
func (s *Service) TTL() time.Duration { return s.ttl }

// PasswordLogin reports whether the local account may sign in with its password.
func (s *Service) PasswordLogin() bool { return s.passwordLogin && s.localEmail != "" }

// IsLocal reports whether u is the local account (the only one with a password).
func (s *Service) IsLocal(u *store.AdminUser) bool {
	return u != nil && s.localEmail != "" && u.Email == s.localEmail
}

// Mark sets the Local flag on users for the API.
func (s *Service) Mark(users ...*store.AdminUser) {
	for _, u := range users {
		if u != nil {
			u.Local = s.IsLocal(u)
		}
	}
}

// ---- sign in ----

// Login checks the local account's email + password and starts a session. It returns
// the session token (the cookie value; only its hash is stored). Every other account
// is refused here, the same way as a wrong password.
func (s *Service) Login(ctx context.Context, email, password, userAgent, ip string) (string, *store.AdminUser, error) {
	if !s.PasswordLogin() {
		return "", nil, ErrPasswordLoginOff
	}
	email = normalizeEmail(email)
	if !s.limiter.allow("email:"+email) || !s.limiter.allow("ip:"+ip) {
		return "", nil, ErrTooManyAttempts
	}
	var u *store.AdminUser
	var err error
	if email == s.localEmail {
		u, err = s.st.GetAdminUserByEmail(ctx, email)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", nil, err
		}
	}
	if u == nil || u.PasswordHash == "" {
		_, _ = VerifyPassword(dummyHash(), password) // same time as a real check
		s.fail(email, ip)
		return "", nil, ErrInvalidCredentials
	}
	ok, err := VerifyPassword(u.PasswordHash, password)
	if err != nil {
		s.log.Warn("admin password hash unreadable", "user", u.ID, "err", err)
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
	s.log.Warn("break-glass sign-in with the local account", "user", u.ID, "email", u.Email, "ip", ip)
	s.audit("password", u.ID, u.Email, ip)
	s.Mark(u)
	return token, u, nil
}

// MasterKeyUsed records a master-key call: a warning log each time, an audit event at
// most once per minute per node (scripts may call often).
func (s *Service) MasterKeyUsed(ip, method, path string) {
	s.log.Warn("admin API called with the master key", "ip", ip, "method", method, "path", path)
	s.mu.Lock()
	now := s.now()
	send := now.Sub(s.lastMasterKey) >= time.Minute
	if send {
		s.lastMasterKey = now
	}
	s.mu.Unlock()
	if send {
		s.audit("master_key", "", "", ip)
	}
}

func (s *Service) fail(email, ip string) {
	s.limiter.fail("email:" + email)
	s.limiter.fail("ip:" + ip)
}

// IssueSession starts a session (admin.session_ttl) for an already authenticated user.
func (s *Service) IssueSession(ctx context.Context, u *store.AdminUser, userAgent, ip string) (string, error) {
	return s.issueSession(ctx, u, userAgent, ip, s.ttl)
}

func (s *Service) issueSession(ctx context.Context, u *store.AdminUser, userAgent, ip string, ttl time.Duration) (string, error) {
	if u.DisabledAt != nil {
		return "", ErrInvalidCredentials
	}
	token := "aifs_" + randomHex(32)
	now := s.now().UTC()
	if len(userAgent) > 256 {
		userAgent = userAgent[:256]
	}
	if err := s.st.CreateAdminSession(ctx, &store.AdminSession{
		Hash: hashToken(token), UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(ttl), UserAgent: userAgent, IP: ip,
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
	s.Mark(u)
	return u, sess, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.st.DeleteAdminSession(ctx, hashToken(token))
}

// ---- accounts ----

func (s *Service) ListUsers(ctx context.Context) ([]store.AdminUser, error) {
	users, err := s.st.ListAdminUsers(ctx)
	for i := range users {
		s.Mark(&users[i])
	}
	return users, err
}

func (s *Service) GetUser(ctx context.Context, id string) (*store.AdminUser, error) {
	u, err := s.st.GetAdminUser(ctx, id)
	s.Mark(u)
	return u, err
}

const noPasswords = "accounts have no password: they sign in through the identity provider. Only the local account has one, set in config (admin.bootstrap)"

// CreateUserInput is a new OIDC account. Password is only there to give a clear error.
type CreateUserInput struct {
	Email    string  `json:"email"`
	Name     string  `json:"name"`
	Role     string  `json:"role"`
	Password *string `json:"password"`
}

func (s *Service) CreateUser(ctx context.Context, in CreateUserInput) (*store.AdminUser, error) {
	if in.Password != nil {
		return nil, inputErr(noPasswords)
	}
	email := normalizeEmail(in.Email)
	if err := checkEmail(email); err != nil {
		return nil, err
	}
	if email == s.localEmail {
		return nil, &InputError{Msg: email + " is the local account, set in config", Err: store.ErrConflict}
	}
	if in.Role == "" {
		in.Role = store.RoleAdmin
	}
	if !slices.Contains(Roles, in.Role) {
		return nil, inputErr("role must be one of: %s", strings.Join(Roles, ", "))
	}
	now := s.now().UTC()
	u := &store.AdminUser{ID: id.New("usr_"), Email: email, Name: strings.TrimSpace(in.Name), Role: in.Role,
		CreatedAt: now, UpdatedAt: now}
	if err := s.st.CreateAdminUser(ctx, u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, &InputError{Msg: "an account with email " + email + " already exists", Err: store.ErrConflict}
		}
		return nil, err
	}
	return u, nil
}

// UpdateUserInput changes only the fields that are set. Password is only there to give
// a clear error.
type UpdateUserInput struct {
	Name     *string `json:"name"`
	Role     *string `json:"role"`
	Disabled *bool   `json:"disabled"`
	Password *string `json:"password"`
}

// UpdateUser is an admin changing an account. actorID is the signed-in admin ("" for the master key).
func (s *Service) UpdateUser(ctx context.Context, userID, actorID string, in UpdateUserInput) (*store.AdminUser, error) {
	if in.Password != nil {
		return nil, inputErr(noPasswords)
	}
	u, err := s.st.GetAdminUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if s.IsLocal(u) && (in.Role != nil || in.Disabled != nil) {
		return nil, inputErr("the local account is set in config; only its name can change here")
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
	s.Mark(u)
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
	if s.IsLocal(u) {
		return inputErr("the local account is set in config; remove admin.bootstrap.email to stop using it")
	}
	if u.Role == store.RoleAdmin && u.DisabledAt == nil {
		if err := s.keepOneAdmin(ctx); err != nil {
			return err
		}
	}
	return s.st.DeleteAdminUser(ctx, userID) // sessions and links go with it (ON DELETE CASCADE)
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

// SyncLocal makes the local account match config (D42): created if missing, role admin,
// enabled, and the password from b. When the password changes, its sessions end. It is
// safe when several nodes start at once. It also reports old passwords of other accounts,
// which are ignored from now on.
func (s *Service) SyncLocal(ctx context.Context, b config.AdminBootstrap) (created bool, err error) {
	email := normalizeEmail(b.Email)
	if email == "" {
		return false, nil
	}
	if email != s.localEmail {
		return false, fmt.Errorf("local account %s does not match the service (%s)", email, s.localEmail)
	}
	u, err := s.st.GetAdminUserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		hash, err := s.wantHash(b, "")
		if err != nil {
			return false, err
		}
		now := s.now().UTC()
		u = &store.AdminUser{ID: id.New("usr_"), Email: email, Name: strings.TrimSpace(b.Name), Role: store.RoleAdmin,
			PasswordHash: hash, CreatedAt: now, UpdatedAt: now}
		err = s.st.CreateAdminUser(ctx, u)
		if err == nil {
			s.reportOldPasswords(ctx)
			return true, nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return false, err
		}
		if u, err = s.st.GetAdminUserByEmail(ctx, email); err != nil { // another node made it
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	hash, err := s.wantHash(b, u.PasswordHash)
	if err != nil {
		return false, err
	}
	newPassword := hash != u.PasswordHash
	if newPassword || u.Role != store.RoleAdmin || u.DisabledAt != nil {
		u.PasswordHash, u.Role, u.DisabledAt, u.UpdatedAt = hash, store.RoleAdmin, nil, s.now().UTC()
		if err := s.st.UpdateAdminUser(ctx, u); err != nil {
			return false, err
		}
	}
	if newPassword {
		if err := s.st.DeleteAdminSessions(ctx, u.ID, ""); err != nil {
			return false, err
		}
		s.log.Warn("local account password changed from config; its sessions ended", "email", email)
	}
	s.reportOldPasswords(ctx)
	return false, nil
}

// wantHash is the hash the local account should have. With a plain password, a stored
// hash that already matches is kept, so a restart does not end sessions.
func (s *Service) wantHash(b config.AdminBootstrap, stored string) (string, error) {
	if b.PasswordHash != "" {
		if _, err := VerifyPassword(b.PasswordHash, ""); err != nil {
			return "", fmt.Errorf("admin.bootstrap.password_hash: %w", err)
		}
		return b.PasswordHash, nil
	}
	if stored != "" {
		if ok, _ := VerifyPassword(stored, b.Password); ok {
			return stored, nil
		}
	}
	return HashPassword(b.Password)
}

func (s *Service) reportOldPasswords(ctx context.Context) {
	users, err := s.st.ListAdminUsers(ctx)
	if err != nil {
		return
	}
	n := 0
	for _, u := range users {
		if u.PasswordHash != "" && !s.IsLocal(&u) {
			n++
		}
	}
	if n > 0 {
		s.log.Warn("other admin accounts still have an old password; it is ignored, they must sign in through OIDC", "accounts", n)
	}
}

func (s *Service) HasUsers(ctx context.Context) (bool, error) {
	users, err := s.st.ListAdminUsers(ctx)
	return len(users) > 0, err
}

// ---- helpers ----

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b)
}

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

// CheckPassword checks a new local password (aif admin hash-password).
func CheckPassword(p string) error {
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
