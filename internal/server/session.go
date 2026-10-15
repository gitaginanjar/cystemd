// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/gitaginanjar/cystemd/internal/auth"
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

// AccountInfo is a local account without its secret.
type AccountInfo struct {
	Name          string     `json:"name"`
	Enabled       bool       `json:"enabled"`
	Capabilities  []string   `json:"capabilities,omitempty"`
	Groups        []string   `json:"groups,omitempty"`
	PasswordMtime *time.Time `json:"passwordMtime,omitempty"`
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
	return &AccountInfo{Name: acct.Name, Enabled: acct.Enabled, Capabilities: acct.Capabilities, Groups: acct.Groups, PasswordMtime: acct.PasswordMtime}, nil
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
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// updatePassword changes the caller's own local password.
func (s *Server) updatePassword(r *http.Request, current, next string) error {
	id := identity(r)
	if id.Provider != auth.ProviderLocal {
		return badRequest("only local accounts have a password")
	}
	if err := s.Auth.UpdatePassword(r.Context(), id.Username, current, next); err != nil {
		s.audit(r, "accounts/update-password", id.Username, "FAILED", err.Error())
		return badRequest("%s", err.Error())
	}
	s.audit(r, "accounts/update-password", id.Username, "ALLOWED", "")
	return nil
}

func (s *Server) handleUpdatePassword(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.updatePassword(r, req.CurrentPassword, req.NewPassword); err != nil {
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
		out = append(out, AccountInfo{Name: a.Name, Enabled: a.Enabled, Capabilities: a.Capabilities, Groups: a.Groups, PasswordMtime: a.PasswordMtime})
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
