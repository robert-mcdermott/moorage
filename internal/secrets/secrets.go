// Package secrets encrypts the values of secret variables
// (spec/08-security.md, "Secrets"; decision D16). Each value is sealed with
// AES-256-GCM under its app's data key, with the app, the scope and the
// variable's name as associated data, so a ciphertext cannot be moved to
// another variable. Data keys are themselves sealed by the master key, which
// lives outside the database: a backup of the database alone reveals no
// secret.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// MasterKeyEnv names a file holding the master key, in place of the one in
// the data directory. Under systemd, a credential named master.key
// (LoadCredential=) is used when this is not set.
const MasterKeyEnv = "MOORAGE_MASTER_KEY_FILE"

// keySize is the size of master and data keys: AES-256.
const keySize = 32

// MasterKey is where the master key came from and the key itself.
type MasterKey struct {
	Path    string
	Created bool
	key     []byte
}

// Derive returns a key for one purpose, made from the master key. It changes with the master key.
func (m MasterKey) Derive(purpose string) []byte {
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte("moorage " + purpose))
	return mac.Sum(nil)
}

// LoadMasterKey reads the master key: from the file named by
// $MOORAGE_MASTER_KEY_FILE, from systemd's credentials directory, or from
// defaultPath. The default file is created (readable by its owner only)
// when it does not exist and create is true — that is, when no secret has
// been sealed yet; otherwise a missing key is an error, since the secrets
// it sealed could not be read.
func LoadMasterKey(defaultPath string, getenv func(string) string, create bool) (MasterKey, error) {
	path := defaultPath
	switch {
	case getenv(MasterKeyEnv) != "":
		path = getenv(MasterKeyEnv)
	case getenv("CREDENTIALS_DIRECTORY") != "":
		if p := filepath.Join(getenv("CREDENTIALS_DIRECTORY"), "master.key"); exists(p) {
			path = p
		}
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && path == defaultPath {
		if !create {
			return MasterKey{}, fmt.Errorf("the master key %s is missing, and the database holds secrets sealed with it: restore the key from your backup (secrets cannot be read without it)", path)
		}
		key := make([]byte, keySize)
		rand.Read(key)
		encoded := base64.StdEncoding.EncodeToString(key) + "\n"
		if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
			return MasterKey{}, fmt.Errorf("write the master key: %w", err)
		}
		return MasterKey{Path: path, Created: true, key: key}, nil
	}
	if err != nil {
		return MasterKey{}, fmt.Errorf("read the master key: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != keySize {
		return MasterKey{}, fmt.Errorf("%s is not a master key (32 bytes, base64-encoded)", path)
	}
	return MasterKey{Path: path, key: key}, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// KeyStore keeps the sealed data keys of apps.
type KeyStore interface {
	// DataKeys returns every app's sealed data key, by app ID.
	DataKeys(ctx context.Context) (map[string][]byte, error)
	// AddDataKey stores an app's sealed data key unless it has one, and
	// returns the one stored.
	AddDataKey(ctx context.Context, appID string, sealed []byte) ([]byte, error)
}

// Keyring seals and opens the values of secret variables.
type Keyring struct {
	master cipher.AEAD
	store  KeyStore

	mu   sync.Mutex
	keys map[string]cipher.AEAD // by app ID
}

// Open returns a keyring, checking that the master key opens every data
// key already stored.
func Open(ctx context.Context, master MasterKey, store KeyStore) (*Keyring, error) {
	aead, err := newAEAD(master.key)
	if err != nil {
		return nil, err
	}
	k := &Keyring{master: aead, store: store, keys: map[string]cipher.AEAD{}}
	sealed, err := store.DataKeys(ctx)
	if err != nil {
		return nil, err
	}
	for appID, s := range sealed {
		if _, err := k.unwrap(appID, s); err != nil {
			return nil, fmt.Errorf("the master key %s does not open the secrets in the database (it is not the key they were sealed with)", master.Path)
		}
	}
	return k, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// version is the first byte of everything this package seals.
const version = 1

func seal(aead cipher.AEAD, plaintext, ad []byte) []byte {
	out := make([]byte, 1+aead.NonceSize(), 1+aead.NonceSize()+len(plaintext)+aead.Overhead())
	out[0] = version
	rand.Read(out[1:])
	return aead.Seal(out, out[1:], plaintext, ad)
}

func open(aead cipher.AEAD, sealed, ad []byte) ([]byte, error) {
	if len(sealed) < 1+aead.NonceSize() || sealed[0] != version {
		return nil, errors.New("not a sealed value")
	}
	nonce := sealed[1 : 1+aead.NonceSize()]
	return aead.Open(nil, nonce, sealed[1+aead.NonceSize():], ad)
}

func dataKeyAD(appID string) []byte { return []byte("moorage data key\x00" + appID) }

func (k *Keyring) unwrap(appID string, sealed []byte) (cipher.AEAD, error) {
	key, err := open(k.master, sealed, dataKeyAD(appID))
	if err != nil {
		return nil, err
	}
	return newAEAD(key)
}

// dataKey returns an app's data key, creating it on first use.
func (k *Keyring) dataKey(ctx context.Context, appID string) (cipher.AEAD, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if aead, ok := k.keys[appID]; ok {
		return aead, nil
	}
	key := make([]byte, keySize)
	rand.Read(key)
	sealed, err := k.store.AddDataKey(ctx, appID, seal(k.master, key, dataKeyAD(appID)))
	if err != nil {
		return nil, err
	}
	aead, err := k.unwrap(appID, sealed)
	if err != nil {
		return nil, fmt.Errorf("the data key of %s cannot be opened: %w", appID, err)
	}
	k.keys[appID] = aead
	return aead, nil
}

// channelAD binds what a notification channel needs to that channel.
func channelAD(id string) []byte { return []byte("moorage channel\x00" + id) }

// SealChannel encrypts what a notification channel needs, with the master key: it belongs to
// the server, not to an app.
func (k *Keyring) SealChannel(id string, value []byte) []byte {
	return seal(k.master, value, channelAD(id))
}

// OpenChannel decrypts what a notification channel needs.
func (k *Keyring) OpenChannel(id string, sealed []byte) ([]byte, error) {
	plain, err := open(k.master, sealed, channelAD(id))
	if err != nil {
		return nil, fmt.Errorf("the channel %s cannot be decrypted", id)
	}
	return plain, nil
}

// registryAD binds a password to its registry.
func registryAD(host string) []byte { return []byte("moorage registry\x00" + host) }

// SealRegistry encrypts a container registry's password, with the master key:
// it belongs to the server, not to an app.
func (k *Keyring) SealRegistry(host, password string) []byte {
	return seal(k.master, []byte(password), registryAD(host))
}

// OpenRegistry decrypts a container registry's password.
func (k *Keyring) OpenRegistry(host string, sealed []byte) (string, error) {
	plain, err := open(k.master, sealed, registryAD(host))
	if err != nil {
		return "", fmt.Errorf("the password for %s cannot be decrypted", host)
	}
	return string(plain), nil
}

// valueAD binds a value to its variable: the app, the scope (an
// environment ID, or "" for all environments) and the name.
func valueAD(appID, scope, name string) []byte {
	return []byte("moorage variable\x00" + appID + "\x00" + scope + "\x00" + name)
}

// Seal encrypts a variable's value.
func (k *Keyring) Seal(ctx context.Context, appID, scope, name, value string) ([]byte, error) {
	aead, err := k.dataKey(ctx, appID)
	if err != nil {
		return nil, err
	}
	return seal(aead, []byte(value), valueAD(appID, scope, name)), nil
}

// Open decrypts a variable's value.
func (k *Keyring) Open(ctx context.Context, appID, scope, name string, sealed []byte) (string, error) {
	aead, err := k.dataKey(ctx, appID)
	if err != nil {
		return "", err
	}
	plain, err := open(aead, sealed, valueAD(appID, scope, name))
	if err != nil {
		return "", fmt.Errorf("the secret %s cannot be decrypted", name)
	}
	return string(plain), nil
}
