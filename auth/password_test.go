package auth

import "testing"

func TestHashVerify_Roundtrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ok, err := VerifyPassword(hash, "correct horse battery staple")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Error("verify returned false for the correct password")
	}
}

func TestVerify_WrongPassword(t *testing.T) {
	hash, _ := HashPassword("secret-value")
	ok, err := VerifyPassword(hash, "not-it")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ok {
		t.Error("verify returned true for a wrong password")
	}
}

func TestHash_DistinctSalts(t *testing.T) {
	a, _ := HashPassword("same")
	b, _ := HashPassword("same")
	if a == b {
		t.Error("two hashes of the same password are identical — salt not random")
	}
}

func TestVerify_InvalidHashFormat(t *testing.T) {
	for _, bad := range []string{"", "plain", "$argon2id$nope", "$bcrypt$v=1$x$y$z"} {
		if _, err := VerifyPassword(bad, "x"); err == nil {
			t.Errorf("VerifyPassword(%q) returned nil error, want format error", bad)
		}
	}
}

func TestSecret_HashStable(t *testing.T) {
	raw, err := GenerateSecret()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if raw == "" {
		t.Fatal("empty secret")
	}
	if HashSecret(raw) != HashSecret(raw) {
		t.Error("HashSecret not deterministic")
	}
	if HashSecret(raw) == raw {
		t.Error("HashSecret returned the raw value")
	}
}
