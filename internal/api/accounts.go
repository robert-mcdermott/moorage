package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/auth"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// decode reads a JSON request body, or answers with what is wrong.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		problem(w, http.StatusBadRequest, "bad_request", "The body is not valid JSON: "+err.Error())
		return false
	}
	return true
}

// failed answers with an error of signing in or of an account's details.
func (s *Server) failed(w http.ResponseWriter, err error) {
	if errors.Is(err, auth.ErrSecondFactor) {
		unauthorized(w, "bad_second_factor", sentenceOf(err))
		return
	}
	if errors.Is(err, auth.ErrPasswordsOff) {
		problem(w, http.StatusForbidden, "passwords_off", sentenceOf(err))
		return
	}
	var limited *auth.RateLimitedError
	var field *auth.FieldError
	switch {
	case errors.As(err, &limited):
		w.Header().Set("Retry-After", strconv.Itoa(int(limited.Wait.Seconds())+1))
		problem(w, http.StatusTooManyRequests, "rate_limited", limited.Error())
	case errors.As(err, &field):
		writeProblem(w, apitypes.Problem{
			Title: http.StatusText(http.StatusUnprocessableEntity), Status: http.StatusUnprocessableEntity, Code: "invalid_field", Detail: field.Detail,
			Errors: []apitypes.FieldProblem{{Field: field.Field, Detail: field.Detail}},
		})
	case errors.Is(err, auth.ErrBadCredentials):
		problem(w, http.StatusUnauthorized, "bad_credentials", err.Error())
	case errors.Is(err, store.ErrEmailTaken):
		problem(w, http.StatusConflict, "email_taken", err.Error())
	case errors.Is(err, store.ErrLastAdmin):
		problem(w, http.StatusConflict, "last_admin", err.Error())
	case errors.Is(err, store.ErrTokenNameTaken):
		problem(w, http.StatusConflict, "name_taken", err.Error())
	case errors.Is(err, store.ErrNotFound):
		problem(w, http.StatusNotFound, "not_found", err.Error())
	default:
		s.internal(w, err)
	}
}

func userDoc(u store.User) apitypes.User {
	return apitypes.User{ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role, Disabled: u.Disabled(), CreatedAt: u.CreatedAt}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !auth.SameOrigin(r) {
		problem(w, http.StatusForbidden, "csrf", "Sign in from the console's own pages.")
		return
	}
	var req apitypes.LoginRequest
	if !decode(w, r, &req) {
		return
	}
	ip := s.auth.ClientIP(r)
	u, err := s.auth.SignIn(r.Context(), req.Email, req.Password, ip)
	if err != nil {
		s.failed(w, err)
		return
	}
	needed, err := s.auth.NeedsSecondFactor(r.Context(), u.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if needed {
		if req.Code == "" {
			unauthorized(w, "second_factor_needed", sentenceOf(auth.ErrSecondFactorNeeded))
			return
		}
		if err := s.auth.CheckSecondFactor(r.Context(), u, req.Code, ip); err != nil {
			s.failed(w, err)
			return
		}
	}
	value, err := s.auth.StartSession(r.Context(), u, ip, r.UserAgent())
	if err != nil {
		s.internal(w, err)
		return
	}
	http.SetCookie(w, s.auth.SessionCookie(value))
	writeJSON(w, http.StatusOK, apitypes.LoginResult{User: userDoc(u), CSRFToken: auth.CSRFToken(value)})
}

// logout ends the session in use, or revokes the token in use. The server's
// admin token cannot be revoked this way, and stays as it is.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	switch {
	case p.SessionID != "":
		if err := s.auth.EndSession(r.Context(), p.CookieValue); err != nil {
			s.internal(w, err)
			return
		}
		http.SetCookie(w, s.auth.SessionCookie(""))
	case p.TokenID != "":
		if _, err := s.db.DeleteToken(r.Context(), p.UserID, p.TokenID); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.internal(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deviceStart(w http.ResponseWriter, r *http.Request) {
	var req apitypes.DeviceRequest
	if !decode(w, r, &req) {
		return
	}
	claimed, err := s.auth.Claimed(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	if !claimed {
		problem(w, http.StatusConflict, "not_claimed", "This server has no accounts yet: claim it first, with the setup link that moor server status shows on the server.")
		return
	}
	code, userCode, err := s.auth.StartDevice(r.Context(), strings.TrimSpace(req.Client), s.auth.ClientIP(r))
	if err != nil {
		s.failed(w, err)
		return
	}
	verify := s.cfg.PublicURL(s.cfg.ConsoleHost) + "/device"
	writeJSON(w, http.StatusOK, apitypes.DeviceAuthorization{
		DeviceCode: code, UserCode: userCode, VerificationURL: verify, VerificationURLComplete: verify + "?code=" + url.QueryEscape(userCode),
		ExpiresIn: int(auth.DeviceCodeLife.Seconds()), Interval: int(auth.DevicePollInterval.Seconds()),
	})
}

func (s *Server) deviceToken(w http.ResponseWriter, r *http.Request) {
	var req apitypes.DeviceTokenRequest
	if !decode(w, r, &req) {
		return
	}
	token, u, err := s.auth.PollDevice(r.Context(), req.DeviceCode, "moor login")
	switch {
	case errors.Is(err, auth.ErrDevicePending):
		problem(w, http.StatusBadRequest, "authorization_pending", err.Error())
	case errors.Is(err, auth.ErrDeviceSlowDown):
		problem(w, http.StatusBadRequest, "slow_down", err.Error())
	case errors.Is(err, auth.ErrDeviceDenied):
		problem(w, http.StatusBadRequest, "access_denied", err.Error())
	case errors.Is(err, auth.ErrDeviceExpired):
		problem(w, http.StatusBadRequest, "expired_token", err.Error())
	case err != nil:
		s.internal(w, err)
	default:
		writeJSON(w, http.StatusOK, apitypes.DeviceToken{Token: token, User: userDoc(u)})
	}
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	doc := apitypes.Me{Name: p.Name, Role: store.RoleAdmin, Local: p.Local(), Scopes: p.Scopes, App: p.App}
	if !p.Local() {
		u, err := s.db.UserByID(r.Context(), p.UserID)
		if err != nil {
			s.internal(w, err)
			return
		}
		doc.ID, doc.Email, doc.Name, doc.Role = u.ID, u.Email, u.Name, u.Role
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	docs := []apitypes.Session{}
	if !p.Local() {
		list, err := s.db.UserSessions(r.Context(), p.UserID)
		if err != nil {
			s.internal(w, err)
			return
		}
		for _, x := range list {
			docs = append(docs, apitypes.Session{
				ID: x.ID, Current: x.ID == p.SessionID, IP: x.IP, UserAgent: x.UserAgent,
				CreatedAt: x.CreatedAt, LastSeenAt: x.LastSeenAt, ExpiresAt: x.ExpiresAt,
			})
		}
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	id := r.PathValue("session")
	err := store.ErrNotFound
	if !p.Local() {
		err = s.db.DeleteSession(r.Context(), p.UserID, id)
	}
	if err != nil {
		s.failed(w, err)
		return
	}
	if id == p.SessionID {
		http.SetCookie(w, s.auth.SessionCookie(""))
	}
	w.WriteHeader(http.StatusNoContent)
}

func tokenDoc(t store.APIToken) apitypes.Token {
	return apitypes.Token{ID: t.ID, Name: t.Name, Scopes: t.Scopes, App: t.App, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, CreatedAt: t.CreatedAt}
}

func (s *Server) tokens(w http.ResponseWriter, r *http.Request) {
	list, err := s.db.Tokens(r.Context(), who(r).UserID)
	if err != nil {
		s.internal(w, err)
		return
	}
	docs := []apitypes.Token{}
	for _, t := range list {
		docs = append(docs, tokenDoc(t))
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var req apitypes.NewToken
	if !decode(w, r, &req) {
		return
	}
	t, secret, err := s.auth.CreateToken(r.Context(), who(r).UserID, req.Name, req.Scopes, req.App, req.ExpiresAt)
	if err != nil {
		s.failed(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, apitypes.CreatedToken{Token: tokenDoc(t), Secret: secret})
}

// changePassword changes the caller's password, and ends their other sessions.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	if p.Local() {
		problem(w, http.StatusConflict, "no_account", "The server's admin token belongs to no account.")
		return
	}
	var req apitypes.PasswordChange
	if !decode(w, r, &req) {
		return
	}
	if err := s.auth.ChangePassword(r.Context(), p.UserID, req.Current, req.New, p.SessionID, s.auth.ClientIP(r)); err != nil {
		s.failed(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// endOtherSessions ends the caller's sessions but the one in use.
func (s *Server) endOtherSessions(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	if !p.Local() {
		if err := s.db.DeleteOtherSessions(r.Context(), p.UserID, p.SessionID); err != nil {
			s.internal(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	t, err := s.db.DeleteToken(r.Context(), who(r).UserID, r.PathValue("token"))
	if errors.Is(err, store.ErrNotFound) {
		problem(w, http.StatusNotFound, "not_found", "You have no token named "+r.PathValue("token")+".")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenDoc(t))
}

// lookupUser reads the {user} path value, an ID or an email address.
func (s *Server) lookupUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	which := r.PathValue("user")
	var u store.User
	var err error
	if strings.Contains(which, "@") {
		u, err = s.db.UserByEmail(r.Context(), strings.ToLower(which))
	} else {
		u, err = s.db.UserByID(r.Context(), which)
	}
	if errors.Is(err, store.ErrNotFound) {
		problem(w, http.StatusNotFound, "not_found", "There is no user "+which+".")
		return u, false
	}
	if err != nil {
		s.internal(w, err)
		return u, false
	}
	return u, true
}

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := pageParams(w, r, 50)
	if !ok {
		return
	}
	list, err := s.db.Users(r.Context(), cursor, limit+1)
	if err != nil {
		s.internal(w, err)
		return
	}
	page := apitypes.Page[apitypes.User]{Items: []apitypes.User{}}
	for i, u := range list {
		if i == limit {
			page.NextCursor = encodeCursor(list[i-1].Email)
			break
		}
		page.Items = append(page.Items, userDoc(u))
	}
	writeJSON(w, http.StatusOK, page)
}

func validRole(role string) bool { return role == store.RoleAdmin || role == store.RoleMember }

func (s *Server) invitation(w http.ResponseWriter, r *http.Request, u store.User, status int) {
	code, err := s.auth.Invite(r.Context(), u.ID, who(r).Name)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, status, apitypes.Invitation{
		User: userDoc(u), URL: s.cfg.PublicURL(s.cfg.ConsoleHost) + "/invite?code=" + url.QueryEscape(code),
		ExpiresAt: time.Now().Add(auth.InvitationLife).UTC(),
	})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var req apitypes.NewUser
	if !decode(w, r, &req) {
		return
	}
	role := req.Role
	if role == "" {
		role = store.RoleMember
	}
	if !validRole(role) {
		s.failed(w, &auth.FieldError{Field: "role", Detail: `A role is "admin" or "member".`})
		return
	}
	u, err := s.auth.NewUser(r.Context(), req.Email, req.Name, role, "")
	if err == nil {
		err = s.db.CreateUser(r.Context(), u)
	}
	if err != nil {
		s.failed(w, err)
		return
	}
	s.invitation(w, r, u, http.StatusCreated)
}

func (s *Server) changeUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.lookupUser(w, r)
	if !ok {
		return
	}
	var req apitypes.UserChange
	if !decode(w, r, &req) {
		return
	}
	if req.Role != nil {
		if !validRole(*req.Role) {
			s.failed(w, &auth.FieldError{Field: "role", Detail: `A role is "admin" or "member".`})
			return
		}
		if err := s.db.SetUserRole(r.Context(), u.ID, *req.Role); err != nil {
			s.failed(w, err)
			return
		}
	}
	if req.Disabled != nil {
		if err := s.db.SetUserDisabled(r.Context(), u.ID, *req.Disabled); err != nil {
			s.failed(w, err)
			return
		}
	}
	u, err := s.db.UserByID(r.Context(), u.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, userDoc(u))
}

func (s *Server) inviteUser(w http.ResponseWriter, r *http.Request) {
	if u, ok := s.lookupUser(w, r); ok {
		s.invitation(w, r, u, http.StatusCreated)
	}
}

// pageParams reads ?limit and ?cursor, the cursor decoded.
func pageParams(w http.ResponseWriter, r *http.Request, defaultLimit int) (limit int, cursor string, ok bool) {
	limit = defaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			problem(w, http.StatusBadRequest, "bad_request", "limit must be between 1 and 200.")
			return 0, "", false
		}
		limit = n
	}
	if v := r.URL.Query().Get("cursor"); v != "" {
		data, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil || len(data) == 0 {
			problem(w, http.StatusBadRequest, "invalid_cursor", "That cursor is not one the API gave.")
			return 0, "", false
		}
		cursor = string(data)
	}
	return limit, cursor, true
}

func encodeCursor(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
