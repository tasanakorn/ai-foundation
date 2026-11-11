package auth

import (
	"strings"
	"testing"
)

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash format: %s", h)
	}
	if h2, _ := HashPassword("correct horse battery"); h2 == h {
		t.Fatal("same password must get a new salt")
	}
	if ok, err := VerifyPassword(h, "correct horse battery"); !ok || err != nil {
		t.Fatalf("right password: %v %v", ok, err)
	}
	if ok, _ := VerifyPassword(h, "wrong horse battery"); ok {
		t.Fatal("wrong password accepted")
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$AA$AA", "$argon2id$v=19$m=99999999,t=1,p=1$AA$AA", "$argon2id$v=19$m=8,t=1,p=1$!!$AA"} {
		if ok, err := VerifyPassword(bad, "x"); ok || err == nil {
			t.Errorf("bad hash %q: %v %v", bad, ok, err)
		}
	}
}
