// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"cmp"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/gitaginanjar/cystemd/internal/auth"
	"github.com/gitaginanjar/cystemd/internal/store"
)

type loginRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	NomadToken string `json:"nomadToken"`
}

// login authenticates with a local account or a Nomad ACL token and returns
// the session token; failures are audited.
func (s *Server) login(r *http.Request, req loginRequest) (string, *auth.Identity, error) {
	var token string
	var id *auth.Identity
	var err error
	switch {
	case req.NomadToken != "":
		if !*s.Config.Auth.NomadTokenLogin {
			return "", nil, forbidden("Nomad token login is disabled")
		}
		token, id, err = s.Auth.LoginNomadToken(r.Context(), req.NomadToken)
	default:
		token, id, err = s.Auth.Login(r.Context(), req.Username, req.Password)
	}
	if err != nil {
		code := http.StatusUnauthorized
		if errors.Is(err, auth.ErrTooManyAttempts) {
			code = http.StatusTooManyRequests
		}
		user := req.Username
		if req.NomadToken != "" {
			user = "<nomad token>"
		}
		r = r.WithContext(withIdentity(r, &auth.Identity{Username: user, Provider: "login"}))
		s.audit(r, "session/create", user, "DENIED", err.Error())
		return "", nil, statusErr(code, "%s", err.Error())
	}
	s.audit(r.WithContext(withIdentity(r, id)), "session/create", id.Username, "ALLOWED", "")
	return token, id, nil
}

// setSessionCookie stores the session token in an HTTP-only cookie.
// SameSite=Lax plus the CSRF checks on writes (JSON or htmx headers) keep
// cross-site requests from riding the session.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(s.Auth.TTL()),
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: auth.CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	token, _, err := s.login(r, req)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{})
}

// UserInfo describes the caller (User Info page).
type UserInfo struct {
	LoggedIn bool     `json:"loggedIn"`
	Username string   `json:"username"`
	Provider string   `json:"provider"`
	Groups   []string `json:"groups,omitempty"`
	Roles    []string `json:"roles,omitempty"`
	Iss      string   `json:"iss"`
	// IsAdmin lets the UI show admin-only pages.
	IsAdmin bool `json:"isAdmin"`
}

func (s *Server) userInfo(r *http.Request) UserInfo {
	id := identity(r)
	return UserInfo{
		LoggedIn: id.Provider != auth.ProviderAnonymous,
		Username: id.Username,
		Provider: id.Provider,
		Groups:   id.Groups,
		Roles:    s.RBAC.Roles(id.Username, id.Groups),
		Iss:      "cystemd",
		IsAdmin:  s.RBAC.Enforce(id.Username, id.Groups, "accounts", "update", "*"),
	}
}

func (s *Server) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.userInfo(r))
}

// AccountInfo is a local account without its secrets.
type AccountInfo struct {
	Name          string     `json:"name"`
	Enabled       bool       `json:"enabled"`
	Capabilities  []string   `json:"capabilities,omitempty"`
	Groups        []string   `json:"groups,omitempty"`
	PasswordMtime *time.Time `json:"passwordMtime,omitempty"`
	// Tokens are the account's API tokens (their IDs: a token is shown
	// once, when generated).
	Tokens []store.APIToken `json:"tokens,omitempty"`
}

func accountInfo(a *store.Account) AccountInfo {
	return AccountInfo{Name: a.Name, Enabled: a.Enabled, Capabilities: a.Capabilities, Groups: a.Groups,
		PasswordMtime: a.PasswordMtime, Tokens: a.Tokens}
}

// account describes the caller's own account.
func (s *Server) account(r *http.Request) (*AccountInfo, error) {
	id := identity(r)
	if id.Provider != auth.ProviderLocal {
		return &AccountInfo{Name: id.Username, Enabled: true, Groups: id.Groups}, nil
	}
	acct, err := s.Store.GetAccount(r.Context(), id.Username)
	if err != nil {
		return nil, err
	}
	info := accountInfo(acct)
	return &info, nil
}

// ownAccount reports whether the caller acts on its own local account:
// signed in with its password, or reading it with one of its API tokens.
func ownAccount(r *http.Request, action, name string) bool {
	id := identity(r)
	return id.Provider == auth.ProviderLocal && id.Username == name && (action == "get" || id.TokenID == "")
}

// accountAccess checks that the caller may act on a local account: its own
// (see ownAccount), any with accounts/<action>.
func (s *Server) accountAccess(r *http.Request, action, name string) error {
	if ownAccount(r, action, name) {
		return nil
	}
	return s.authorize(r, "accounts", action, name)
}

// mayUpdateAccount is accountAccess(update) without the audit of a denial,
// for showing controls.
func (s *Server) mayUpdateAccount(r *http.Request, name string) bool {
	id := identity(r)
	return ownAccount(r, "update", name) || s.RBAC.Enforce(id.Username, id.Groups, "accounts", "update", name)
}

// getAccount describes a local account.
func (s *Server) getAccount(r *http.Request, name string) (*AccountInfo, error) {
	if err := s.accountAccess(r, "get", name); err != nil {
		return nil, err
	}
	acct, err := s.Store.GetAccount(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("account %s not found", name)
	}
	if err != nil {
		return nil, err
	}
	info := accountInfo(acct)
	return &info, nil
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	out, err := s.getAccount(r, r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type tokenRequest struct {
	// ID names the token (default: random).
	ID string `json:"id"`
	// ExpiresIn is the token's lifetime in seconds; 0 never expires.
	ExpiresIn int64 `json:"expiresIn"`
}

// TokenResponse is a new API token, shown this once: only its ID is kept.
type TokenResponse struct {
	Token string `json:"token"`
	store.APIToken
}

// maxTokenLifetime bounds expiresIn (in seconds) well below overflowing a
// time.Duration.
const maxTokenLifetime = 100 * 365 * 24 * 3600

// generateToken issues an API token for an account with the apiKey
// capability.
func (s *Server) generateToken(r *http.Request, name string, req tokenRequest) (*TokenResponse, error) {
	if err := s.accountAccess(r, "update", name); err != nil {
		return nil, err
	}
	if req.ExpiresIn < 0 || req.ExpiresIn > maxTokenLifetime {
		return nil, badRequest("expiresIn is a number of seconds (at most a century), 0 for a token that does not expire")
	}
	tok, rec, err := s.Auth.GenerateToken(r.Context(), name, req.ID, time.Duration(req.ExpiresIn)*time.Second)
	if errors.Is(err, store.ErrNotFound) {
		err = notFound("account %s not found", name)
	}
	if err != nil {
		s.audit(r, "accounts/generate-token", name, "FAILED", err.Error())
		return nil, err
	}
	s.audit(r, "accounts/generate-token", name+"/"+rec.ID, "ALLOWED", "")
	return &TokenResponse{Token: tok, APIToken: rec}, nil
}

func (s *Server) handleGenerateToken(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	out, err := s.generateToken(r, r.PathValue("name"), req)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteToken revokes an API token.
func (s *Server) deleteToken(r *http.Request, name, id string) error {
	if err := s.accountAccess(r, "update", name); err != nil {
		return err
	}
	if err := s.Auth.DeleteToken(r.Context(), name, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = notFound("account %s not found", name)
		}
		s.audit(r, "accounts/delete-token", name+"/"+id, "FAILED", err.Error())
		return err
	}
	s.audit(r, "accounts/delete-token", name+"/"+id, "ALLOWED", "")
	return nil
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	if err := s.deleteToken(r, r.PathValue("name"), r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// handleCanI tells the caller whether RBAC lets it perform an action
// (Argo CD's account can-i).
func (s *Server) handleCanI(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	value := "no"
	if s.RBAC.Enforce(id.Username, id.Groups, r.PathValue("resource"), r.PathValue("action"), r.PathValue("object")) {
		value = "yes"
	}
	writeJSON(w, http.StatusOK, map[string]string{"value": value})
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	out, err := s.account(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type passwordRequest struct {
	// Name is the account whose password changes (default: the caller's).
	Name string `json:"name"`
	// CurrentPassword is the caller's.
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// updatePassword changes a local password: the caller's own or, for an
// administrator (accounts/update), another account's. Either way the
// caller confirms with its own password.
func (s *Server) updatePassword(r *http.Request, req passwordRequest) error {
	id := identity(r)
	if id.Provider != auth.ProviderLocal {
		return badRequest("only local accounts have a password")
	}
	if id.TokenID != "" {
		return forbidden("an API token cannot change passwords")
	}
	name := cmp.Or(req.Name, id.Username)
	if name != id.Username {
		if err := s.authorize(r, "accounts", "update", name); err != nil {
			return err
		}
	}
	err := s.Auth.CheckPassword(r.Context(), id.Username, req.CurrentPassword)
	if err == nil {
		err = s.Auth.SetPassword(r.Context(), name, req.NewPassword)
	}
	if err != nil {
		s.audit(r, "accounts/update-password", name, "FAILED", err.Error())
		switch {
		case errors.Is(err, auth.ErrTooManyAttempts):
			return statusErr(http.StatusTooManyRequests, "%s", err.Error())
		case errors.Is(err, store.ErrNotFound):
			return notFound("account %s not found", name)
		}
		return badRequest("%s", err.Error())
	}
	s.audit(r, "accounts/update-password", name, "ALLOWED", "")
	return nil
}

func (s *Server) handleUpdatePassword(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.updatePassword(r, req); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// listAccounts lists the local accounts (needs accounts/get).
func (s *Server) listAccounts(r *http.Request) ([]AccountInfo, error) {
	if err := s.authorize(r, "accounts", "get", "*"); err != nil {
		return nil, err
	}
	accts, err := s.Store.ListAccounts(r.Context())
	if err != nil {
		return nil, err
	}
	out := []AccountInfo{}
	for _, a := range accts {
		out = append(out, accountInfo(a))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	out, err := s.listAccounts(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
