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
