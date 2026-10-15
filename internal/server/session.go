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

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	var token string
	var id *auth.Identity
	var err error
	switch {
	case req.NomadToken != "":
		if !*s.Config.Auth.NomadTokenLogin {
			writeError(w, http.StatusForbidden, "Nomad token login is disabled")
			return
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
		writeError(w, code, err.Error())
		return
	}
	r = r.WithContext(withIdentity(r, id))
	s.audit(r, "session/create", id.Username, "ALLOWED", "")
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(s.Auth.TTL()),
	})
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: auth.CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
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

func (s *Server) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	writeJSON(w, http.StatusOK, UserInfo{
		LoggedIn: id.Provider != auth.ProviderAnonymous,
		Username: id.Username,
		Provider: id.Provider,
		Groups:   id.Groups,
		Roles:    s.RBAC.Roles(id.Username, id.Groups),
		Iss:      "cystemd",
		IsAdmin:  s.RBAC.Enforce(id.Username, id.Groups, "accounts", "update", "*"),
	})
}

// AccountInfo is a local account without its secret.
type AccountInfo struct {
	Name          string     `json:"name"`
	Enabled       bool       `json:"enabled"`
	Capabilities  []string   `json:"capabilities,omitempty"`
	Groups        []string   `json:"groups,omitempty"`
	PasswordMtime *time.Time `json:"passwordMtime,omitempty"`
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	if id.Provider != auth.ProviderLocal {
		writeJSON(w, http.StatusOK, AccountInfo{Name: id.Username, Enabled: true, Groups: id.Groups})
		return
	}
	acct, err := s.Store.GetAccount(r.Context(), id.Username)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, AccountInfo{Name: acct.Name, Enabled: acct.Enabled, Capabilities: acct.Capabilities, Groups: acct.Groups, PasswordMtime: acct.PasswordMtime})
}

type passwordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

func (s *Server) handleUpdatePassword(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	if id.Provider != auth.ProviderLocal {
		writeError(w, http.StatusBadRequest, "only local accounts have a password")
		return
	}
	var req passwordRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.Auth.UpdatePassword(r.Context(), id.Username, req.CurrentPassword, req.NewPassword); err != nil {
		s.audit(r, "accounts/update-password", id.Username, "FAILED", err.Error())
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "accounts/update-password", id.Username, "ALLOWED", "")
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.can(w, r, "accounts", "get", "*") {
		return
	}
	accts, err := s.Store.ListAccounts(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []AccountInfo{}
	for _, a := range accts {
		out = append(out, AccountInfo{Name: a.Name, Enabled: a.Enabled, Capabilities: a.Capabilities, Groups: a.Groups, PasswordMtime: a.PasswordMtime})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
