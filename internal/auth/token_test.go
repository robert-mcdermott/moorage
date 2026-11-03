package auth

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/robert-mcdermott/moorage/internal/store"
)

func TestNewToken(t *testing.T) {
	a, b := NewToken(), NewToken()
	if !strings.HasPrefix(a, TokenPrefix) || len(a) < len(TokenPrefix)+40 || a == b {
		t.Errorf("tokens %q, %q", a, b)
	}
	if !Matches(a, Hash(a)) || Matches(b, Hash(a)) {
		t.Error("Matches is wrong")
	}
}

func TestBootstrapToken(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "moorage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Migrate(ctx, filepath.Join(dir, "backups")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "admin-token")

	hash, created, err := BootstrapToken(ctx, db, file)
	if err != nil || !created {
		t.Fatalf("first call: created %v, %v", created, err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(data))
	if !Matches(token, hash) {
		t.Error("the file's token does not match the stored hash")
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
			t.Errorf("token file mode %v", fi.Mode().Perm())
		}
	}

	again, created, err := BootstrapToken(ctx, db, file)
	if err != nil || created || again != hash {
		t.Errorf("second call: created %v, same %v, %v", created, again == hash, err)
	}

	os.Remove(file)
	rotated, created, err := BootstrapToken(ctx, db, file)
	if err != nil || !created || rotated == hash {
		t.Errorf("after the file was removed: created %v, rotated %v, %v", created, rotated != hash, err)
	}
	if Matches(token, rotated) {
		t.Error("the old token still matches after rotation")
	}
}
