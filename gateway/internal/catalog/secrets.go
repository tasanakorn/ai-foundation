package catalog

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"ai-foundation/gateway/internal/store"
)

// aad binds ciphertexts to this use, so they cannot be passed off as another kind of value.
var aad = []byte("aif catalog provider key v1")

// Keyring encrypts provider keys stored in the database (spec-0006 section 4.4).
// The first key seals new values; any key opens values sealed with it (rotation).
type Keyring struct {
	keys []secretKey
}

type secretKey struct {
	id   string
	aead cipher.AEAD
}

// ParseKeyring reads base64 keys of 32 bytes. No keys gives an empty keyring that
// refuses to seal or open. A key's ID comes from the key itself, so it needs no name.
func ParseKeyring(encoded []string) (*Keyring, error) {
	kr := &Keyring{}
	for i, e := range encoded {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(e))
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("secret key %d: must be base64 of 32 bytes", i)
		}
		block, _ := aes.NewCipher(raw)
		aead, _ := cipher.NewGCM(block)
		sum := sha256.Sum256(raw)
		kr.keys = append(kr.keys, secretKey{id: "k" + hex.EncodeToString(sum[:4]), aead: aead})
	}
	return kr, nil
}

// ErrNoSecretKey means an encrypted key was used without catalog.secret_keys.
var ErrNoSecretKey = errors.New("encrypted provider keys need catalog.secret_keys (AIF_CATALOG_SECRET_KEY)")

// Enabled reports whether the keyring can seal values.
func (k *Keyring) Enabled() bool { return k != nil && len(k.keys) > 0 }

// SetKey stores value in a as an encrypted key with the first secret key, and clears any reference.
func (k *Keyring) SetKey(a *store.CatalogAccount, value string) error {
	if !k.Enabled() {
		return ErrNoSecretKey
	}
	if value == "" {
		return errors.New("empty key")
	}
	sk := k.keys[0]
	nonce := make([]byte, sk.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	a.KeyRef, a.KeyID, a.KeyHint = "", sk.id, hint(value)
	a.KeyCipher = sk.aead.Seal(nonce, nonce, []byte(value), aad)
	return nil
}

// open decrypts an account's key. Errors name the key ID, never the value.
func (k *Keyring) open(a store.CatalogAccount) (string, error) {
	if !k.Enabled() {
		return "", ErrNoSecretKey
	}
	for _, sk := range k.keys {
		if sk.id != a.KeyID {
			continue
		}
		n := sk.aead.NonceSize()
		if len(a.KeyCipher) < n {
			return "", fmt.Errorf("encrypted key (secret key %s) is damaged", a.KeyID)
		}
		v, err := sk.aead.Open(nil, a.KeyCipher[:n], a.KeyCipher[n:], aad)
		if err != nil {
			return "", fmt.Errorf("encrypted key (secret key %s) does not decrypt", a.KeyID)
		}
		return string(v), nil
	}
	return "", fmt.Errorf("encrypted key uses secret key %s, which is not in catalog.secret_keys", a.KeyID)
}

// hint is the last 4 characters of a key, for display.
func hint(v string) string {
	if len(v) <= 8 {
		return "" // too short to show any of it
	}
	return v[len(v)-4:]
}
