// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// OIDCProvider is a minimal OpenID Connect provider (authorization code
// flow) for tests: whoever is sent to it is signed in at once as the user
// its claims describe.
type OIDCProvider struct {
	URL          string
	ClientID     string
	ClientSecret string

	key    *rsa.PrivateKey
	mu     sync.Mutex
	claims map[string]any
	codes  map[string]oidcGrant
	tokens map[string]map[string]any // access token → user info
}

type oidcGrant struct {
	nonce, redirectURI string
	claims             map[string]any
}

const oidcKeyID = "test"

// StartOIDCProvider starts a provider for a client; claims must have sub.
func StartOIDCProvider(t testing.TB, clientID, clientSecret string, claims map[string]any) *OIDCProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &OIDCProvider{ClientID: clientID, ClientSecret: clientSecret, key: key, claims: claims,
		codes: map[string]oidcGrant{}, tokens: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	mux.HandleFunc("GET /userinfo", p.userinfo)
	mux.HandleFunc("GET /keys", p.keys)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p.URL = srv.URL
	return p
}

// SetClaims changes who signs in next.
func (p *OIDCProvider) SetClaims(claims map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.claims = claims
}

func (p *OIDCProvider) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"issuer":                                p.URL,
		"authorization_endpoint":                p.URL + "/authorize",
		"token_endpoint":                        p.URL + "/token",
		"userinfo_endpoint":                     p.URL + "/userinfo",
		"jwks_uri":                              p.URL + "/keys",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "email", "profile", "groups"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
	})
}

func (p *OIDCProvider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if q.Get("client_id") != p.ClientID || q.Get("response_type") != "code" || err != nil || redirect.Scheme == "" {
		http.Error(w, "invalid authorization request", http.StatusBadRequest)
		return
	}
	code := randomHex(16)
	p.mu.Lock()
	p.codes[code] = oidcGrant{nonce: q.Get("nonce"), redirectURI: q.Get("redirect_uri"), claims: maps.Clone(p.claims)}
	p.mu.Unlock()
	back := redirect.Query()
	back.Set("code", code)
	back.Set("state", q.Get("state"))
	redirect.RawQuery = back.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (p *OIDCProvider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, "invalid_request")
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != p.ClientID || secret != p.ClientSecret {
		oauthError(w, "invalid_client")
		return
	}
	code := r.PostForm.Get("code")
	p.mu.Lock()
	g, found := p.codes[code]
	delete(p.codes, code)
	p.mu.Unlock()
	if r.PostForm.Get("grant_type") != "authorization_code" || !found || g.redirectURI != r.PostForm.Get("redirect_uri") {
		oauthError(w, "invalid_grant")
		return
	}
	now := time.Now()
	claims := jwt.MapClaims{"iss": p.URL, "aud": p.ClientID, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()}
	if g.nonce != "" {
		claims["nonce"] = g.nonce
	}
	maps.Copy(claims, g.claims)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = oidcKeyID
	idToken, err := tok.SignedString(p.key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	access := randomHex(16)
	p.mu.Lock()
	p.tokens[access] = g.claims
	p.mu.Unlock()
	writeJSONResponse(w, http.StatusOK, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 300, "id_token": idToken})
}

func (p *OIDCProvider) userinfo(w http.ResponseWriter, r *http.Request) {
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	p.mu.Lock()
	info, found := p.tokens[token]
	p.mu.Unlock()
	if !bearer || !found {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	writeJSONResponse(w, http.StatusOK, info)
}

func (p *OIDCProvider) keys(w http.ResponseWriter, _ *http.Request) {
	pub := p.key.PublicKey
	writeJSONResponse(w, http.StatusOK, map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": oidcKeyID,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func oauthError(w http.ResponseWriter, code string) {
	writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": code})
}

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
