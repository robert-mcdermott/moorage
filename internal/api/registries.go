package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// RegistrySealer seals the passwords of container registries.
type RegistrySealer interface {
	SealRegistry(host, password string) []byte
}

// registryHost is what the engines look credentials up by.
var registryHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)

func registryDoc(r store.Registry) apitypes.Registry {
	return apitypes.Registry{Host: r.Host, Username: r.Username, CreatedAt: r.CreatedAt}
}

func (s *Server) registries(w http.ResponseWriter, r *http.Request) {
	all, err := s.db.Registries(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	docs := []apitypes.Registry{}
	for _, reg := range all {
		docs = append(docs, registryDoc(reg))
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) setRegistry(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.PathValue("host"))
	if !registryHost.MatchString(host) {
		problem(w, http.StatusUnprocessableEntity, "invalid_host", "A registry is named by its host, such as ghcr.io or registry.example.com:5000.")
		return
	}
	var req apitypes.RegistryLogin
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "invalid_request", "The request is not a registry's credentials: "+err.Error())
		return
	}
	if req.Username == "" || req.Password == "" {
		problem(w, http.StatusUnprocessableEntity, "invalid_credentials", "A registry's credentials are a username and a password.")
		return
	}
	reg := store.Registry{Host: host, Username: req.Username, Sealed: s.keyring.SealRegistry(host, req.Password), CreatedAt: time.Now().UTC()}
	if err := s.db.SetRegistry(r.Context(), reg); err != nil {
		s.internal(w, err)
		return
	}
	s.log.Info("stored the credentials of a container registry", "host", host, "by", who(r).Name)
	writeJSON(w, http.StatusOK, registryDoc(reg))
}

func (s *Server) deleteRegistry(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.PathValue("host"))
	if err := s.db.DeleteRegistry(r.Context(), host); err != nil {
		s.failed(w, err)
		return
	}
	s.log.Info("removed the credentials of a container registry", "host", host, "by", who(r).Name)
	w.WriteHeader(http.StatusNoContent)
}
