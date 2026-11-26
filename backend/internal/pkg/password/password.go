package password

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const (
	// MinBytes is the minimum UTF-8 byte length required by docs/api.md.
	MinBytes = 8
	// MaxBytes is the maximum UTF-8 byte length allowed by docs/api.md and bcrypt.
	MaxBytes = 72
	// DefaultCost is the minimum recommended bcrypt work factor aligned with init.sql.
	DefaultCost = 10
)

var (
	// ErrEmpty indicates an omitted or empty password value.
	ErrEmpty = errors.New("password must not be empty")
	// ErrInvalidUTF8 indicates a password containing invalid UTF-8 byte sequences.
	ErrInvalidUTF8 = errors.New("password must be valid UTF-8")
	// ErrInvalidLength indicates a password outside [MinBytes, MaxBytes] UTF-8 bytes.
	ErrInvalidLength = errors.New("password must be between 8 and 72 UTF-8 bytes")
	// ErrInvalidCost indicates an unsupported bcrypt cost factor.
	ErrInvalidCost = errors.New("bcrypt cost out of allowed range")
	// ErrMismatch indicates the plaintext password does not match the stored bcrypt hash.
	ErrMismatch = errors.New("password does not match hash")
)

// Validate verifies that raw is valid UTF-8 and within [8, 72] UTF-8 bytes.
func Validate(raw string) error {
	if raw == "" {
		return ErrEmpty
	}
	if !utf8.ValidString(raw) {
		return ErrInvalidUTF8
	}
	byteLen := len(raw)
	if byteLen < MinBytes || byteLen > MaxBytes {
		return ErrInvalidLength
	}
	return nil
}

// Hash validates raw and returns its bcrypt hash with the requested cost (>= 10).
// A cost of 0 defaults to DefaultCost (10).
func Hash(raw string, cost int) (string, error) {
	if err := Validate(raw); err != nil {
		return "", err
	}
	if cost == 0 {
		cost = DefaultCost
	}
	if cost < DefaultCost || cost > bcrypt.MaxCost {
		return "", fmt.Errorf("%w: got %d, want [%d, %d]", ErrInvalidCost, cost, DefaultCost, bcrypt.MaxCost)
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(raw), cost)
	if err != nil {
		return "", fmt.Errorf("bcrypt hash failed: %w", err)
	}
	return string(hashed), nil
}

// Verify checks whether raw matches a stored bcrypt hash.
// Passwords exceeding bcrypt's 72-byte cap or failing UTF-8 checks return ErrMismatch.
func Verify(raw, hashed string) error {
	if raw == "" || hashed == "" || !utf8.ValidString(raw) || len(raw) > MaxBytes {
		return ErrMismatch
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hashed), []byte(raw)); err != nil {
		return ErrMismatch
	}
	return nil
}
