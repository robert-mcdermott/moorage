package api

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/doctor"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// serverInfo describes the server: its version and the apps' git remotes.
func (s *Server) serverInfo(w http.ResponseWriter, r *http.Request) {
	info := apitypes.ServerInfo{
		Version: buildinfo.Version,
		Git:     apitypes.GitInfo{HTTP: s.cfg.PublicURL(s.cfg.ConsoleHost) + "/git", SSH: sshPrefix(s.cfg.Listen.SSH, s.cfg.ConsoleHost)},
	}
	st := s.deployer.EngineKnown()
	info.Containers = apitypes.ContainerEngine{State: string(st.State), Kind: st.Kind, Version: st.Version, Problem: st.Problem()}
	if who(r).Admin && who(r).Envs == "" {
		info.Containers.Reason = st.Reason
	}
	writeJSON(w, http.StatusOK, info)
}

// serverDoctor makes the checks of moor server doctor, from the server itself.
func (s *Server) serverDoctor(w http.ResponseWriter, r *http.Request) {
	docs := []apitypes.Check{}
	for _, c := range doctor.Run(r.Context(), doctor.Options{Config: s.cfg, WantsEngine: s.deployer.WantsEngine, Engine: s.deployer.EngineStatus}) {
		docs = append(docs, apitypes.Check{Name: c.Name, Status: c.Status, Detail: c.Detail, Fix: c.Fix})
	}
	writeJSON(w, http.StatusOK, docs)
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
	k, err := s.auth.AddSSHKey(r.Context(), who(r).UserID, req.Name, req.PublicKey)
	var invalid *auth.InvalidKeyError
	var exists *store.KeyExistsError
	switch {
	case errors.As(err, &invalid):
		problem(w, http.StatusBadRequest, invalid.Code, invalid.Error())
	case errors.As(err, &exists):
		problem(w, http.StatusConflict, "key_exists", exists.Error()+".")
	case err != nil:
		s.internal(w, err)
	default:
		writeJSON(w, http.StatusCreated, sshKeyDoc(k))
	}
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
