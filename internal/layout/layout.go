// Package layout names the files and directories inside a server's data directory
// (spec/02-architecture.md, "On-disk layout").
package layout

import (
	"os"
	"path/filepath"
)

// Layout names the files and directories inside a data directory.
type Layout struct {
	Root       string
	Database   string
	Keys       string
	Repos      string
	Builds     string
	AppData    string
	Logs       string
	Toolchains string
	Cache      string
	Certs      string
	Run        string
	Tmp        string
	Backups    string
	LockFile   string
	// AdminToken holds the bootstrap admin token, readable only by the
	// server's user.
	AdminToken string
	// MasterKey seals the data keys of secrets (unless the key is given
	// elsewhere: see secrets.LoadMasterKey).
	MasterKey string
	// SSHHostKey is the git server's SSH host key.
	SSHHostKey string
	// ClaimCode holds the first-run claim code until the server is claimed.
	ClaimCode string
}

// NewLayout returns the layout of the data directory root.
func New(root string) Layout {
	at := func(elem ...string) string { return filepath.Join(append([]string{root}, elem...)...) }
	return Layout{
		Root:       root,
		Database:   at("moorage.db"),
		Keys:       at("keys"),
		Repos:      at("repos"),
		Builds:     at("builds"),
		AppData:    at("data"),
		Logs:       at("logs"),
		Toolchains: at("toolchains"),
		Cache:      at("cache"),
		Certs:      at("certs"),
		Run:        at("run"),
		Tmp:        at("tmp"),
		Backups:    at("backups"),
		LockFile:   at("run", "moor.lock"),
		AdminToken: at("keys", "admin-token"),
		MasterKey:  at("keys", "master.key"),
		SSHHostKey: at("keys", "ssh_host_ed25519_key"),
		ClaimCode:  at("keys", "claim-code"),
	}
}

// Prepare creates the data directory and its subdirectories. Those holding
// keys, certificates, runtime state, temporary files and backups are private
// to the server's user.
func (l Layout) Prepare() error {
	dirs := []struct {
		path string
		mode os.FileMode
	}{
		{l.Root, 0o755},
		{l.Repos, 0o755},
		{l.Builds, 0o755},
		{l.AppData, 0o755},
		{l.Logs, 0o755},
		{l.Toolchains, 0o755},
		{l.Cache, 0o755},
		{l.Keys, 0o700},
		{l.Certs, 0o700},
		{l.Run, 0o700},
		{l.Tmp, 0o700},
		{l.Backups, 0o700},
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return err
		}
		if d.mode == 0o700 {
			// A temporary directory that isolation made passable stays so, for the apps running
			// from it through a restart: isolation looks to what is in it.
			if fi, err := os.Stat(d.path); err == nil && d.path == l.Tmp && fi.Mode().Perm() == 0o711 {
				continue
			}
			// MkdirAll leaves an existing directory's mode alone.
			if err := os.Chmod(d.path, d.mode); err != nil {
				return err
			}
		}
	}
	return nil
}
