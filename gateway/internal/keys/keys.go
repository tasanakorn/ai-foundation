// Package keys creates, authenticates and revokes gateway API keys.
package keys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"ai-foundation/gateway/internal/id"
	"ai-foundation/gateway/internal/store"
)

const (
	SecretPrefix = "sk-aif-"
	displayLen   = 12
	cacheTTL     = 30 * time.Second
	negativeTTL  = 5 * time.Second
	maxCache     = 10000
)

var ErrInvalid = errors.New("invalid api key")

// Generate returns a new random secret: "sk-aif-" + 32 URL-safe chars (192 bits).
func Generate() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return SecretPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// Hash is the stored form of a secret.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

type CreateInput struct {
	Name          string            `json:"name"`
	TenantID      string            `json:"tenant_id"`
	AppID         string            `json:"app_id"`
	AllowedModels []string          `json:"allowed_models"`
	Metadata      map[string]string `json:"metadata"`
	ExpiresAt     *time.Time        `json:"expires_at"`
}

type entry struct {
	key *store.Key // nil = known invalid
	exp time.Time
}

// Service authenticates keys with a short in-memory cache in front of the store.
// Revocation through this Service is instant on this node; other nodes see it within cacheTTL.
type Service struct {
	st    store.Store
	now   func() time.Time
	mu    sync.RWMutex
	cache map[string]entry
}

func NewService(st store.Store) *Service {
	return &Service{st: st, now: time.Now, cache: map[string]entry{}}
}

func (s *Service) Create(ctx context.Context, in CreateInput) (string, *store.Key, error) {
	secret, err := Generate()
	if err != nil {
		return "", nil, err
	}
	k := &store.Key{
		ID:            id.New("key_"),
		Name:          in.Name,
		Prefix:        secret[:displayLen],
		Hash:          Hash(secret),
		TenantID:      in.TenantID,
		AppID:         in.AppID,
		AllowedModels: in.AllowedModels,
		Metadata:      in.Metadata,
		CreatedAt:     s.now().UTC(),
		ExpiresAt:     in.ExpiresAt,
	}
	if k.AllowedModels == nil {
		k.AllowedModels = []string{}
	}
	if k.Metadata == nil {
		k.Metadata = map[string]string{}
	}
	if err := s.st.CreateKey(ctx, k); err != nil {
		return "", nil, err
	}
	return secret, k, nil
}

// Authenticate returns the active key for secret, or ErrInvalid.
// Other errors mean the store failed and the caller should answer 5xx.
func (s *Service) Authenticate(ctx context.Context, secret string) (*store.Key, error) {
	if !strings.HasPrefix(secret, SecretPrefix) {
		return nil, ErrInvalid
	}
	h := Hash(secret)
	now := s.now()

	s.mu.RLock()
	e, ok := s.cache[h]
	s.mu.RUnlock()
	if !ok || now.After(e.exp) {
		k, err := s.st.GetKeyByHash(ctx, h)
		switch {
		case errors.Is(err, store.ErrNotFound):
			e = entry{nil, now.Add(negativeTTL)}
		case err != nil:
			return nil, err
		default:
			e = entry{k, now.Add(cacheTTL)}
		}
		s.mu.Lock()
		if len(s.cache) >= maxCache {
			s.cache = map[string]entry{} // crude bound; keeps memory flat under key-guessing traffic
		}
		s.cache[h] = e
		s.mu.Unlock()
	}
	if e.key == nil || !e.key.Active(now) {
		return nil, ErrInvalid
	}
	return e.key, nil
}

func (s *Service) Revoke(ctx context.Context, id string) error {
	if err := s.st.RevokeKey(ctx, id, s.now().UTC()); err != nil {
		return err
	}
	s.mu.Lock()
	for h, e := range s.cache {
		if e.key != nil && e.key.ID == id {
			delete(s.cache, h)
		}
	}
	s.mu.Unlock()
	return nil
}

// Allows reports whether key may call model. Empty list = all models.
// A trailing "*" matches by prefix, e.g. "openrouter/anthropic/*".
func Allows(k *store.Key, model string) bool {
	if len(k.AllowedModels) == 0 {
		return true
	}
	for _, p := range k.AllowedModels {
		if p == model || p == "*" {
			return true
		}
		if strings.HasSuffix(p, "*") && strings.HasPrefix(model, p[:len(p)-1]) {
			return true
		}
	}
	return false
}
