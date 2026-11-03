// Package auth holds Moorage's credentials. Until accounts arrive (wave 2),
// the API is guarded by one bootstrap admin token.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/store"
)

// TokenPrefix starts every Moorage token, so that secret scanners can
// recognise a leaked one.
const TokenPrefix = "moor_"

const bootstrapSetting = "auth.bootstrap_token"

// NewToken returns a new random token: the prefix and 32 random bytes in
// base 62.
func NewToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return TokenPrefix + new(big.Int).SetBytes(b[:]).Text(62)
}

// Hash returns the SHA-256 of a token; only hashes are stored.
func Hash(token string) [32]byte {
	return sha256.Sum256([]byte(token))
}

// Matches reports, in constant time, whether token has the given hash.
func Matches(token string, hash [32]byte) bool {
	h := Hash(token)
	return subtle.ConstantTimeCompare(h[:], hash[:]) == 1
}

type bootstrapRecord struct {
	SHA256    string `json:"sha256"`
	CreatedAt int64  `json:"created_at"`
}

// BootstrapToken makes sure the server has an admin token. The token's hash
// is stored as a setting and the token itself in file (mode 0600), where an
// operator on the server can read it. When the file is missing a new token
// replaces the old one, since the file is the only place it can be read.
func BootstrapToken(ctx context.Context, db *store.DB, file string) (hash [32]byte, created bool, err error) {
	var rec bootstrapRecord
	found, err := db.Setting(ctx, bootstrapSetting, &rec)
	if err != nil {
		return hash, false, err
	}
	if found {
		data, err := os.ReadFile(file)
		switch {
		case err == nil:
			token := strings.TrimSpace(string(data))
			stored, decodeErr := hex.DecodeString(rec.SHA256)
			if decodeErr == nil && len(stored) == len(hash) {
				copy(hash[:], stored)
				if Matches(token, hash) {
					return hash, false, nil
				}
			}
		case !errors.Is(err, fs.ErrNotExist):
			return hash, false, err
		}
	}
	token := NewToken()
	if err := writeSecret(file, token+"\n"); err != nil {
		return hash, false, fmt.Errorf("write the admin token: %w", err)
	}
	hash = Hash(token)
	rec = bootstrapRecord{SHA256: hex.EncodeToString(hash[:]), CreatedAt: time.Now().UnixMilli()}
	if err := db.PutSetting(ctx, bootstrapSetting, rec, "moorage"); err != nil {
		return hash, false, err
	}
	return hash, true, nil
}

// writeSecret replaces file with content, readable only by its owner.
func writeSecret(file, content string) error {
	tmp := file + ".new"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}
