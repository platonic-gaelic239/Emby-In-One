package backend

import "testing"

func TestHashAndVerifyPassword(t *testing.T) {
	hashed, err := HashPassword("secret")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if ok := VerifyPassword("secret", hashed); !ok {
		t.Fatalf("VerifyPassword returned false for matching password")
	}
	if ok := VerifyPassword("wrong", hashed); ok {
		t.Fatalf("VerifyPassword returned true for wrong password")
	}
}

func TestVerifyPasswordRejectsPlaintext(t *testing.T) {
	// Plaintext stored password must always be rejected, even if it matches input
	if VerifyPassword("secret", "secret") {
		t.Fatal("VerifyPassword should reject plaintext stored password")
	}
	if VerifyPassword("", "") {
		t.Fatal("VerifyPassword should reject empty inputs")
	}
	if VerifyPassword("x", "") {
		t.Fatal("VerifyPassword should reject empty stored")
	}
	if VerifyPassword("", "x") {
		t.Fatal("VerifyPassword should reject empty plain")
	}
}
