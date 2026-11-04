package api

import (
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/store"
)

var keyName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,63}$`)

// serverInfo describes the server: its version and the apps' git remotes.
func (s *Server) serverInfo(w http.ResponseWriter, r *http.Request) {
	info := apitypes.ServerInfo{
		Version: buildinfo.Version,
		Git:     apitypes.GitInfo{HTTP: s.cfg.PublicURL(s.cfg.ConsoleHost) + "/git", SSH: sshPrefix(s.cfg.Listen.SSH, s.cfg.ConsoleHost)},
	}
	writeJSON(w, http.StatusOK, info)
}

// sshPrefix is the start of the apps' SSH remotes: the console's hostname
// and the SSH port, or localhost for a server that listens on loopback
// only (SSH clients do not all resolve *.localhost).
func sshPrefix(listen, consoleHost string) string {
	if listen == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
		consoleHost = "localhost"
	}
	if port == "22" {
		return "ssh://git@" + consoleHost
	}
	return "ssh://git@" + net.JoinHostPort(consoleHost, port)
}

func sshKeyDoc(k store.SSHKey) apitypes.SSHKey {
	return apitypes.SSHKey{ID: k.ID, Name: k.Name, Fingerprint: k.Fingerprint, PublicKey: k.PublicKey, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt}
}

func (s *Server) sshKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.db.SSHKeys(r.Context(), who(r).UserID)
	if err != nil {
		s.internal(w, err)
		return
	}
	docs := []apitypes.SSHKey{}
	for _, k := range keys {
		docs = append(docs, sshKeyDoc(k))
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) addSSHKey(w http.ResponseWriter, r *http.Request) {
	var req apitypes.NewSSHKey
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "invalid_request", "The request is not a key: "+err.Error())
		return
	}
	key, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(req.PublicKey))
	if err != nil {
		problem(w, http.StatusBadRequest, "invalid_key", "That is not a public key in authorized_keys format (the contents of a .pub file).")
		return
	}
	if msg := weakKey(key); msg != "" {
		problem(w, http.StatusBadRequest, "invalid_key", msg)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.TrimSpace(comment)
	}
	fingerprint := ssh.FingerprintSHA256(key)
	if name == "" || !keyName.MatchString(name) {
		if req.Name != "" {
			problem(w, http.StatusBadRequest, "invalid_name", "A key's name is up to 64 letters, digits and . _ @ -, starting with a letter or digit.")
			return
		}
		name = "key-" + strings.Map(func(r rune) rune {
			if strings.ContainsRune("+/=", r) {
				return -1
			}
			return r
		}, strings.TrimPrefix(fingerprint, "SHA256:"))[:8]
	}
	k := store.SSHKey{
		ID: ids.New(ids.SSHKey), UserID: who(r).UserID, Name: name, Fingerprint: fingerprint,
		PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + " " + name, CreatedAt: time.Now().UTC(),
	}
	if err := s.db.AddSSHKey(r.Context(), k); err != nil {
		var exists *store.KeyExistsError
		if errors.As(err, &exists) {
			problem(w, http.StatusConflict, "key_exists", exists.Error()+".")
			return
		}
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sshKeyDoc(k))
}

func (s *Server) removeSSHKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.db.RemoveSSHKey(r.Context(), who(r).UserID, r.PathValue("key"))
	if errors.Is(err, store.ErrNotFound) {
		problem(w, http.StatusNotFound, "not_found", "You have no key named "+r.PathValue("key")+".")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sshKeyDoc(k))
}

// weakKey says why a key may not be used, or "" (spec/08-security.md:
// Ed25519, ECDSA, or RSA of 3072 bits or more).
func weakKey(key ssh.PublicKey) string {
	switch key.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoSKED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoSKECDSA256:
		return ""
	case ssh.KeyAlgoRSA:
		if c, ok := key.(ssh.CryptoPublicKey); ok {
			if pub, ok := c.CryptoPublicKey().(*rsa.PublicKey); ok && pub.N.BitLen() >= 3072 {
				return ""
			}
		}
		return "RSA keys must have 3072 bits or more: make an Ed25519 key with ssh-keygen -t ed25519."
	}
	return "Moorage accepts Ed25519, ECDSA and RSA (3072 bits or more) keys, not " + key.Type() + "."
}
