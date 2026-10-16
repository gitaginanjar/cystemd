// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gitaginanjar/cystemd/internal/auth"
)

// SSO is single sign-on through Nomad's OIDC auth methods: Nomad talks to
// the identity provider and issues an ACL token, which becomes a session.
type SSO interface {
	// Methods lists the OIDC auth methods to offer, the default first.
	Methods(ctx context.Context) ([]string, error)
	// AuthURL starts a login: where to send the user.
	AuthURL(ctx context.Context, method, redirectURI, nonce string) (string, error)
	// Complete finishes it with the parameters of the provider's callback.
	Complete(ctx context.Context, cb SSOCallback) (*auth.NomadToken, error)
}

// SSOCallback completes a login.
type SSOCallback struct {
	AuthMethod  string `json:"authMethod"`
	ClientNonce string `json:"clientNonce"`
	State       string `json:"state"`
	Code        string `json:"code"`
	Iss         string `json:"iss,omitempty"`
	RedirectURI string `json:"redirectURI"`
}

// ssoCookie holds a browser login in progress (ssoFlow).
const ssoCookie = "cystemd.sso"

// ssoCallbackPath is where the identity provider sends the browser back;
// the auth methods must allow <url>/ui/auth/callback as a redirect URI.
const ssoCallbackPath = "/ui/auth/callback"

// ssoFlow is the browser's half of a login: the nonce Nomad will check,
// the auth method and where to go afterwards.
type ssoFlow struct {
	Method string `json:"m"`
	Nonce  string `json:"n"`
	Return string `json:"r"`
}

// ssoMethodCache keeps the auth methods for the login page.
type ssoMethodCache struct {
	mu      sync.Mutex
	methods []string
	at      time.Time
}

const ssoMethodsTTL = 30 * time.Second

// ssoMethods are the auth methods offered (none when SSO is off or Nomad
// cannot be reached).
func (s *Server) ssoMethods(ctx context.Context) []string {
	if s.SSO == nil {
		return nil
	}
	c := &s.ssoCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < ssoMethodsTTL {
		return c.methods
	}
	methods, err := s.SSO.Methods(ctx)
	if err != nil {
		s.logger.Warn("list Nomad OIDC auth methods", "error", err)
	}
	c.methods, c.at = methods, time.Now()
	return methods
}

// ssoMethod picks the requested auth method, or the default one.
func (s *Server) ssoMethod(ctx context.Context, name string) (string, error) {
	methods := s.ssoMethods(ctx)
	switch {
	case len(methods) == 0:
		return "", statusErr(http.StatusServiceUnavailable, "no OIDC auth method is available in Nomad")
	case name == "":
		return methods[0], nil
	case !slices.Contains(methods, name):
		return "", statusErr(http.StatusBadRequest, "unknown SSO auth method %q", name)
	}
	return name, nil
}

// externalURL is the control plane's URL as browsers reach it.
func (s *Server) externalURL(r *http.Request) string {
	if u := strings.TrimRight(s.Config.URL, "/"); u != "" {
		return u
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func randomNonce() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ssoLogin turns the token Nomad issued into a session, and audits.
func (s *Server) ssoLogin(r *http.Request, method string, tok *auth.NomadToken, err error) (string, *auth.Identity, error) {
	if err != nil {
		r = r.WithContext(withIdentity(r, &auth.Identity{Username: "<sso>", Provider: auth.ProviderSSO}))
		s.audit(r, "session/create", method, "DENIED", err.Error())
		return "", nil, statusErr(http.StatusUnauthorized, "SSO login failed: %v", err)
	}
	token, id, err := s.Auth.LoginSSO(tok)
	if err != nil {
		return "", nil, err
	}
	s.audit(r.WithContext(withIdentity(r, id)), "session/create", id.Username, "ALLOWED", "sso:"+method)
	return token, id, nil
}

// ─── browser ────────────────────────────────────────────────────────────

// webSSOLogin sends the browser to the identity provider, through the
// auth URL Nomad builds.
func (s *Server) webSSOLogin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if s.SSO == nil {
		s.webLoginFailed(w, r, http.StatusNotFound, "Single sign-on is not configured.")
		return
	}
	method, err := s.ssoMethod(r.Context(), q.Get("method"))
	if err != nil {
		s.webLoginFailed(w, r, errorCode(err), err.Error())
		return
	}
	flow := ssoFlow{Method: method, Nonce: randomNonce(), Return: safeReturn(q.Get("return_url"))}
	authURL, err := s.SSO.AuthURL(r.Context(), method, s.externalURL(r)+ssoCallbackPath, flow.Nonce)
	if err != nil {
		s.logger.Warn("start SSO login", "method", method, "error", err)
		s.webLoginFailed(w, r, http.StatusBadGateway, "Single sign-on is unavailable: "+err.Error())
		return
	}
	b, _ := json.Marshal(flow)
	http.SetCookie(w, &http.Cookie{
		Name: ssoCookie, Value: base64.RawURLEncoding.EncodeToString(b), Path: "/ui/auth/",
		MaxAge: 600, HttpOnly: true, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		// Lax: the provider's redirect back is a top-level navigation.
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

// webSSOCallback completes the login when the provider sends the browser
// back.
func (s *Server) webSSOCallback(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: ssoCookie, Value: "", Path: "/ui/auth/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	var flow ssoFlow
	c, err := r.Cookie(ssoCookie)
	if err == nil {
		var b []byte
		if b, err = base64.RawURLEncoding.DecodeString(c.Value); err == nil {
			err = json.Unmarshal(b, &flow)
		}
	}
	if err != nil || s.SSO == nil || flow.Nonce == "" {
		s.webLoginFailed(w, r, http.StatusBadRequest, "The sign-in expired, or was started in another browser. Try again.")
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		desc := q.Get("error_description")
		if desc == "" {
			desc = e
		}
		s.webLoginFailed(w, r, http.StatusUnauthorized, "The identity provider refused the sign-in: "+desc)
		return
	}
	tok, err := s.SSO.Complete(r.Context(), SSOCallback{AuthMethod: flow.Method, ClientNonce: flow.Nonce,
		State: q.Get("state"), Code: q.Get("code"), Iss: q.Get("iss"), RedirectURI: s.externalURL(r) + ssoCallbackPath})
	token, _, err := s.ssoLogin(r, flow.Method, tok, err)
	if err != nil {
		s.webLoginFailed(w, r, errorCode(err), err.Error())
		return
	}
	s.setSessionCookie(w, r, token)
	http.Redirect(w, r, safeReturn(flow.Return), http.StatusSeeOther)
}

// ─── API (the CLI) ──────────────────────────────────────────────────────

// SSOMethods lists the auth methods offered.
type SSOMethods struct {
	AuthMethods []string `json:"authMethods"`
}

// SSOAuthURLRequest starts a login whose callback the client receives.
type SSOAuthURLRequest struct {
	AuthMethod  string `json:"authMethod,omitempty"`
	RedirectURI string `json:"redirectURI"`
	ClientNonce string `json:"clientNonce"`
}

// SSOAuthURL is where to send the user.
type SSOAuthURL struct {
	AuthMethod string `json:"authMethod"`
	AuthURL    string `json:"authURL"`
}

func (s *Server) handleSSOMethods(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, SSOMethods{AuthMethods: append([]string{}, s.ssoMethods(r.Context())...)})
}

func (s *Server) handleSSOAuthURL(w http.ResponseWriter, r *http.Request) {
	var req SSOAuthURLRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if s.SSO == nil {
		s.fail(w, statusErr(http.StatusNotFound, "single sign-on is not configured"))
		return
	}
	if req.RedirectURI == "" || len(req.ClientNonce) < 16 {
		s.fail(w, badRequest("redirectURI and a clientNonce of at least 16 characters are required"))
		return
	}
	method, err := s.ssoMethod(r.Context(), req.AuthMethod)
	if err != nil {
		s.fail(w, err)
		return
	}
	u, err := s.SSO.AuthURL(r.Context(), method, req.RedirectURI, req.ClientNonce)
	if err != nil {
		s.fail(w, statusErr(http.StatusBadGateway, "%v", err))
		return
	}
	writeJSON(w, http.StatusOK, SSOAuthURL{AuthMethod: method, AuthURL: u})
}

func (s *Server) handleSSOComplete(w http.ResponseWriter, r *http.Request) {
	var cb SSOCallback
	if err := decode(r, &cb); err != nil {
		s.fail(w, err)
		return
	}
	if s.SSO == nil {
		s.fail(w, statusErr(http.StatusNotFound, "single sign-on is not configured"))
		return
	}
	method, err := s.ssoMethod(r.Context(), cb.AuthMethod)
	if err != nil {
		s.fail(w, err)
		return
	}
	cb.AuthMethod = method
	if cb.Code == "" || cb.State == "" || cb.ClientNonce == "" || cb.RedirectURI == "" {
		s.fail(w, badRequest("code, state, clientNonce and redirectURI are required"))
		return
	}
	tok, err := s.SSO.Complete(r.Context(), cb)
	token, _, err := s.ssoLogin(r, method, tok, err)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}
