package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2id's parameters: RFC 9106's second recommended option, for servers
// without much memory to spare.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
	saltLen      = 16

	// MinPasswordLength is the shortest password accepted; MaxPasswordLength
	// bounds the work one request can ask for.
	MinPasswordLength = 12
	MaxPasswordLength = 1024
)

// A hash takes 64 MiB for a moment: only a few at a time, so that a burst of
// sign-ins cannot exhaust the server's memory.
var hashing = make(chan struct{}, 4)

//go:embed common-passwords.txt
var commonPasswordList string

var commonPasswords = sync.OnceValue(func() map[string]bool {
	set := map[string]bool{}
	for _, line := range strings.Split(commonPasswordList, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			set[strings.ToLower(line)] = true
		}
	}
	return set
})

// CheckPassword says what is wrong with a new password, or returns nil.
func CheckPassword(password string) error {
	switch {
	case len(password) < MinPasswordLength:
		return fmt.Errorf("a password must have at least %d characters", MinPasswordLength)
	case len(password) > MaxPasswordLength:
		return fmt.Errorf("a password may have at most %d characters", MaxPasswordLength)
	case commonPasswords()[strings.ToLower(password)]:
		return errors.New("that password is one of the most common: choose another")
	case repetitive(password):
		return errors.New("that password only repeats a few characters: choose another")
	}
	return nil
}

// repetitive reports whether s is a short pattern repeated.
func repetitive(s string) bool {
	for period := 1; period <= 4 && period < len(s); period++ {
		same := true
		for i := period; i < len(s); i++ {
			if s[i] != s[i-period] {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

// HashPassword returns a password's Argon2id hash, in the PHC string format.
func HashPassword(ctx context.Context, password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := argonKey(ctx, password, salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches a hash from HashPassword.
func VerifyPassword(ctx context.Context, hash, password string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("not an Argon2id hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, fmt.Errorf("unsupported Argon2 version %q", parts[2])
	}
	var memory, iterations uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &threads); err != nil {
		return false, fmt.Errorf("bad Argon2 parameters %q", parts[3])
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	got, err := argonKey(ctx, password, salt, iterations, memory, threads, uint32(len(want)))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func argonKey(ctx context.Context, password string, salt []byte, t, m uint32, p uint8, n uint32) ([]byte, error) {
	select {
	case hashing <- struct{}{}:
		defer func() { <-hashing }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return argon2.IDKey([]byte(password), salt, t, m, p, n), nil
}

// A hash to verify against when an account does not exist, so that signing in
// takes as long for an unknown address as for a known one.
var decoyHash = sync.OnceValue(func() string {
	h, _ := HashPassword(context.Background(), NewToken())
	return h
})
