package password

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestValidatePasswordUTF8ByteBounds(t *testing.T) {
	if err := Validate(""); !errors.Is(err, ErrEmpty) {
		t.Fatalf("expected ErrEmpty, got %v", err)
	}
	if err := Validate(string([]byte{0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa, 0xf9, 0xf8})); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("expected ErrInvalidUTF8, got %v", err)
	}
	if err := Validate("1234567"); !errors.Is(err, ErrInvalidLength) {
		t.Fatalf("expected ErrInvalidLength for 7 bytes, got %v", err)
	}
	if err := Validate("12345678"); err != nil {
		t.Fatalf("expected 8 bytes valid, got %v", err)
	}
	if err := Validate(strings.Repeat("a", 72)); err != nil {
		t.Fatalf("expected 72 bytes valid, got %v", err)
	}
	if err := Validate(strings.Repeat("a", 73)); !errors.Is(err, ErrInvalidLength) {
		t.Fatalf("expected ErrInvalidLength for 73 bytes, got %v", err)
	}

	// 3 CJK runes = 9 UTF-8 bytes (valid); 25 CJK runes = 75 UTF-8 bytes (invalid).
	if err := Validate("中文密"); err != nil {
		t.Fatalf("expected 9-byte UTF-8 password valid, got %v", err)
	}
	if err := Validate(strings.Repeat("密", 25)); !errors.Is(err, ErrInvalidLength) {
		t.Fatalf("expected ErrInvalidLength for 75-byte UTF-8 password, got %v", err)
	}
}

func TestHashAndVerify(t *testing.T) {
	// Include leading/trailing spaces to verify passwords are never trimmed.
	raw := "  Pass@1234  "
	hashed, err := Hash(raw, DefaultCost)
	if err != nil {
		t.Fatalf("Hash failed: %v", err)
	}
	if !strings.HasPrefix(hashed, "$2") {
		t.Fatalf("expected bcrypt prefix, got %q", hashed)
	}
	cost, err := bcrypt.Cost([]byte(hashed))
	if err != nil || cost != DefaultCost {
		t.Fatalf("expected cost %d, got %d (err=%v)", DefaultCost, cost, err)
	}

	if err := Verify(raw, hashed); err != nil {
		t.Fatalf("Verify valid password failed: %v", err)
	}
	if err := Verify("Pass@1234", hashed); !errors.Is(err, ErrMismatch) {
		t.Fatalf("expected ErrMismatch when spaces trimmed, got %v", err)
	}
	if err := Verify(strings.Repeat("x", 80), hashed); !errors.Is(err, ErrMismatch) {
		t.Fatalf("expected ErrMismatch for >72 byte input, got %v", err)
	}
	if _, err := Hash(raw, 9); !errors.Is(err, ErrInvalidCost) {
		t.Fatalf("expected ErrInvalidCost for cost < 10, got %v", err)
	}

	// Verify seed admin bcrypt ($2a$10$...) compatibility.
	seedHash := "$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82"
	if err := Verify("WrongSeedPass", seedHash); !errors.Is(err, ErrMismatch) {
		t.Fatalf("expected ErrMismatch for wrong seed pass, got %v", err)
	}
}
