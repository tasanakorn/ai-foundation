// Package id makes short, time-sortable, random identifiers.
package id

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"time"
)

// New returns prefix + 32 hex chars: 6 bytes of unix millis, then 10 random bytes.
func New(prefix string) string {
	var b [16]byte
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(time.Now().UnixMilli()))
	copy(b[:6], ts[2:])
	_, _ = rand.Read(b[6:])
	return prefix + hex.EncodeToString(b[:])
}
