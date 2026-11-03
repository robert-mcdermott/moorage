package secrets

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type memStore struct {
	mu   sync.Mutex
	keys map[string][]byte
}

func (s *memStore) DataKeys(context.Context) (map[string][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]byte{}
	for k, v := range s.keys {
		out[k] = v
	}
	return out, nil
}

func (s *memStore) AddDataKey(_ context.Context, appID string, sealed []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.keys[appID]; ok {
		return existing, nil
	}
	s.keys[appID] = sealed
	return sealed, nil
}

func noEnv(string) string { return "" }

func TestSealAndOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	mk, err := LoadMasterKey(path, noEnv, true)
	if err != nil || !mk.Created {
		t.Fatalf("LoadMasterKey: %+v %v", mk, err)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("master key mode %v", fi.Mode())
	}
	store := &memStore{keys: map[string][]byte{}}
	ctx := context.Background()
	k, err := Open(ctx, mk, store)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := k.Seal(ctx, "app_1", "env_1", "API_KEY", "s3cr3t-value")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("s3cr3t")) {
		t.Fatal("the sealed value contains the plaintext")
	}
	again, _ := k.Seal(ctx, "app_1", "env_1", "API_KEY", "s3cr3t-value")
	if bytes.Equal(sealed, again) {
		t.Error("sealing twice gave the same ciphertext")
	}
	if v, err := k.Open(ctx, "app_1", "env_1", "API_KEY", sealed); err != nil || v != "s3cr3t-value" {
		t.Errorf("Open = %q, %v", v, err)
	}
	// A ciphertext moved to another variable, scope or app does not open.
	for _, other := range [][3]string{{"app_1", "env_1", "OTHER"}, {"app_1", "", "API_KEY"}, {"app_2", "env_1", "API_KEY"}} {
		if _, err := k.Open(ctx, other[0], other[1], other[2], sealed); err == nil {
			t.Errorf("opened as %v", other)
		}
	}

	// A new keyring over the same store, with the same key, opens it.
	mk2, err := LoadMasterKey(path, noEnv, false)
	if err != nil || mk2.Created {
		t.Fatalf("reload: %+v %v", mk2, err)
	}
	k2, err := Open(ctx, mk2, store)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := k2.Open(ctx, "app_1", "env_1", "API_KEY", sealed); err != nil || v != "s3cr3t-value" {
		t.Errorf("reopened: %q %v", v, err)
	}

	// Another master key is refused at once.
	other := filepath.Join(dir, "other.key")
	mk3, _ := LoadMasterKey(other, noEnv, true)
	if _, err := Open(ctx, mk3, store); err == nil || !strings.Contains(err.Error(), "does not open") {
		t.Errorf("wrong master key: %v", err)
	}
}

func TestMasterKeyLocations(t *testing.T) {
	dir := t.TempDir()
	def := filepath.Join(dir, "keys", "master.key")
	os.MkdirAll(filepath.Dir(def), 0o700)
	if _, err := LoadMasterKey(def, noEnv, false); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("missing key with secrets: %v", err)
	}

	elsewhere := filepath.Join(dir, "elsewhere.key")
	created, _ := LoadMasterKey(elsewhere, noEnv, true)
	env := func(k string) string {
		if k == MasterKeyEnv {
			return elsewhere
		}
		return ""
	}
	mk, err := LoadMasterKey(def, env, false)
	if err != nil || mk.Path != elsewhere || !bytes.Equal(mk.key, created.key) {
		t.Errorf("from %s: %+v %v", MasterKeyEnv, mk, err)
	}

	creds := filepath.Join(dir, "creds")
	os.MkdirAll(creds, 0o700)
	data, _ := os.ReadFile(elsewhere)
	os.WriteFile(filepath.Join(creds, "master.key"), data, 0o600)
	systemd := func(k string) string {
		if k == "CREDENTIALS_DIRECTORY" {
			return creds
		}
		return ""
	}
	if mk, err := LoadMasterKey(def, systemd, false); err != nil || !bytes.Equal(mk.key, created.key) {
		t.Errorf("from systemd credentials: %+v %v", mk, err)
	}

	os.WriteFile(elsewhere, []byte("not a key\n"), 0o600)
	if _, err := LoadMasterKey(def, env, false); err == nil {
		t.Error("accepted a malformed key")
	}
}
