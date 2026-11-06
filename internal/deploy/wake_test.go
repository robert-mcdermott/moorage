// SPDX-License-Identifier: Apache-2.0

package deploy

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/robert-mcdermott/moorage/internal/store"
)

// A wake that had to wait for longer than whoever asked would, as behind a restore of what is asleep, leaves it asleep, in memory and on record alike.
func TestAWakeThatRanOutOfTimeStaysAsleep(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "moorage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(ctx, filepath.Join(dir, "backups")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.EnsureApp(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAsleep(ctx, "shop", "production", true); err != nil {
		t.Fatal(err)
	}
	d := New(db, Options{DataDir: dir}, slog.New(slog.DiscardHandler))
	if !d.Asleep("shop", "production") {
		t.Fatal("it should know")
	}
	expired, cancel := context.WithCancel(ctx)
	cancel()
	err = d.Wake(expired, "shop", "production", "")
	t.Logf("the wake: %v", err)
	e, err := db.Environment(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("in memory asleep=%v, on record asleep_at=%v", d.Asleep("shop", "production"), e.AsleepAt)
	if !d.Asleep("shop", "production") && !e.AsleepAt.IsZero() {
		t.Errorf("it is said to be awake, so that requests no longer wake it, and what starts instances goes by the record, which says asleep")
	}
}
