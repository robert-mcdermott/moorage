package reconcile

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemoveOldLogs(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "logs", "apps", "shop", "production")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	write := func(name string, at time.Time) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	gone := []string{write("web.1.ins_old.log", old), write("web.1.ins_old.log.1", old)}
	kept := []string{
		write("web.1.ins_running.log", old),   // running, and quiet
		write("web.1.ins_running.log.1", old), // and what it rotated
		write("web.2.ins_recent.log", time.Now()),
		write("notes.txt", old),
	}
	removeOldLogs(data, map[string]bool{"ins_running": true}, LogRetention, slog.New(slog.DiscardHandler))
	for _, path := range gone {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", path, err)
		}
	}
	for _, path := range kept {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed: %v", path, err)
		}
	}
}
