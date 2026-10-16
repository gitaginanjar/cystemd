// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package server is the CYSTEMD API server (Argo CD's argocd-server): the
// REST + server-sent-events API the web UI and CLI use, with RBAC on every
// route and an audit line for every mutation. Routes and payloads follow
// Argo CD's API so the same concepts (applications, sync, rollback,
// resource tree, managed resources) carry over.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	consulapi "github.com/hashicorp/consul/api"

	"github.com/gitaginanjar/cystemd/internal/appset"
	"github.com/gitaginanjar/cystemd/internal/auth"
	"github.com/gitaginanjar/cystemd/internal/config"
	"github.com/gitaginanjar/cystemd/internal/controller"
	"github.com/gitaginanjar/cystemd/internal/git"
	"github.com/gitaginanjar/cystemd/internal/metrics"
	"github.com/gitaginanjar/cystemd/internal/nomad"
	"github.com/gitaginanjar/cystemd/internal/rbac"
	"github.com/gitaginanjar/cystemd/internal/store"
	"github.com/gitaginanjar/cystemd/internal/vault"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// BuildInfo identifies the running build.
type BuildInfo struct {
	Version   string `json:"Version"`
	GitCommit string `json:"GitCommit"`
	BuildDate string `json:"BuildDate"`
	GoVersion string `json:"GoVersion"`
	Platform  string `json:"Platform"`
	// Mode is "standalone" (cystemd-server) or "agent" (embedded).
	Mode string `json:"Mode"`
}

// Options wires a Server.
type Options struct {
	Config   *config.Config
	Store    *store.Store
	Auth     *auth.Manager
	RBAC     *rbac.Enforcer
	Live     *controller.Live
	Repos    *controller.RepoService
	Git      *git.Client
	Clusters *nomad.Clusters
	Vault    *vault.Client
	Consul   *consulapi.Client
	Metrics  *metrics.Metrics
	Elector  store.Elector
	Logger   *slog.Logger
	Build    BuildInfo
	// SSO is single sign-on through Nomad (nil: not configured).
	SSO SSO
	// AppSets previews ApplicationSets' Applications (nil: no preview).
	AppSets *appset.Generator
}

// Server serves the API and the web UI.
type Server struct {
	Options
	logger   *slog.Logger
	creds    *Credentials
	commits  commitCache
	ssoCache ssoMethodCache
	roles    projectRoles
}

// New returns a Server.
func New(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Build.Platform == "" {
		o.Build.Platform = runtime.GOOS + "/" + runtime.GOARCH
	}
	if o.Build.GoVersion == "" {
		o.Build.GoVersion = runtime.Version()
	}
	s := &Server{Options: o, logger: o.Logger.With("component", "api")}
	s.creds = &Credentials{Store: o.Store, Vault: o.Vault, Config: o.Config}
	return s
}

// Credentials exposes the repository credential resolver (for the git
// client, which is built before the server).
func (s *Server) Credentials() *Credentials { return s.creds }

// route registers pattern with metrics, recovery and (unless public)
// authentication.
func (s *Server) route(mux *http.ServeMux, pattern string, public bool, h http.HandlerFunc) {
	label := pattern
	if i := strings.Index(label, " "); i >= 0 {
		label = label[i+1:]
	}
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				s.logger.Error("panic in handler", "route", label, "panic", p, "stack", string(debug.Stack()))
				if !rec.wrote {
					writeError(rec, http.StatusInternalServerError, "internal error")
				}
			}
			s.Metrics.HTTPRequests.WithLabelValues(label, r.Method, strconv.Itoa(rec.status)).Inc()
			s.Metrics.HTTPDuration.WithLabelValues(label).Observe(time.Since(start).Seconds())
		}()
		if !public {
			id, err := s.Auth.FromRequest(r)
			if err != nil {
				writeError(rec, http.StatusUnauthorized, "no session information")
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), identityKey{}, id))
		}
		if isMutation(r) && !jsonOrBearer(r) && label != "/api/webhook" {
			// Cookie-authenticated writes must be JSON: a cross-site form
			// cannot send that content type (CSRF defence with SameSite).
			// The webhook acts on no one's behalf: Git servers may post
			// forms.
			writeError(rec, http.StatusUnsupportedMediaType, "requests that change state must send Content-Type: application/json")
			return
		}
		h(rec, r)
	})
}

func isMutation(r *http.Request) bool {
	return r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete || r.Method == http.MethodPatch
}

func jsonOrBearer(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return true
	}
	ct := r.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "application/json") || (r.Method == http.MethodDelete && r.ContentLength <= 0 && r.Header.Get("X-Requested-With") != "")
}

type identityKey struct{}

func identity(r *http.Request) *auth.Identity {
	id, _ := r.Context().Value(identityKey{}).(*auth.Identity)
	if id == nil {
		return &auth.Identity{Username: "anonymous", Provider: auth.ProviderAnonymous}
	}
	return id
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status, r.wrote = code, true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// APIHandler returns the /api routes only (agent mode mounts these next to
// Nomad's own API).
func (s *Server) APIHandler() http.Handler {
	mux := http.NewServeMux()
	s.registerAPI(mux)
	return securityHeaders(mux)
}

// Handler returns the full standalone handler: API, web UI, health and
// metrics.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerAPI(mux)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("GET /metrics", s.Metrics.Handler())
	mux.Handle("/ui/", s.webHandler(true))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusTemporaryRedirect)
	})
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) registerAPI(mux *http.ServeMux) {
	// Public.
	s.route(mux, "GET /api/version", true, s.handleVersion)
	s.route(mux, "GET /api/v1/settings", true, s.handleSettings)
	s.route(mux, "POST /api/v1/session", true, s.handleLogin)
	s.route(mux, "DELETE /api/v1/session", true, s.handleLogout)
	s.route(mux, "GET /api/v1/session/sso", true, s.handleSSOMethods)
	s.route(mux, "POST /api/v1/session/sso/auth-url", true, s.handleSSOAuthURL)
	s.route(mux, "POST /api/v1/session/sso/complete", true, s.handleSSOComplete)
	s.route(mux, "GET /api/badge", true, s.handleBadge)
	s.route(mux, "POST /api/webhook", true, s.handleWebhook)

	s.route(mux, "GET /api/v1/session/userinfo", false, s.handleUserInfo)

	s.route(mux, "GET /api/v1/applications", false, s.handleListApps)
	s.route(mux, "POST /api/v1/applications", false, s.handleCreateApp)
	s.route(mux, "GET /api/v1/applications/{name}", false, s.handleGetApp)
	s.route(mux, "PUT /api/v1/applications/{name}", false, s.handleUpdateApp)
	s.route(mux, "DELETE /api/v1/applications/{name}", false, s.handleDeleteApp)
	s.route(mux, "POST /api/v1/applications/{name}/sync", false, s.handleSync)
	s.route(mux, "DELETE /api/v1/applications/{name}/operation", false, s.handleTerminate)
	s.route(mux, "POST /api/v1/applications/{name}/rollback", false, s.handleRollback)
	s.route(mux, "GET /api/v1/applications/{name}/resource-tree", false, s.handleTree)
	s.route(mux, "GET /api/v1/applications/{name}/managed-resources", false, s.handleManagedResources)
	s.route(mux, "GET /api/v1/applications/{name}/manifests", false, s.handleManifests)
	s.route(mux, "GET /api/v1/applications/{name}/revisions/{revision}/metadata", false, s.handleRevisionMetadata)
	s.route(mux, "GET /api/v1/applications/{name}/syncwindows", false, s.handleSyncWindows)
	s.route(mux, "GET /api/v1/applications/{name}/events", false, s.handleEvents)
	s.route(mux, "GET /api/v1/applications/{name}/resource", false, s.handleResource)
	s.route(mux, "GET /api/v1/applications/{name}/resource/actions", false, s.handleListActions)
	s.route(mux, "POST /api/v1/applications/{name}/resource/actions", false, s.handleRunAction)
	s.route(mux, "GET /api/v1/applications/{name}/allocations/{alloc}/logs", false, s.handleLogs)

	s.route(mux, "GET /api/v1/stream/applications", false, s.handleStreamApps)
	s.route(mux, "GET /api/v1/stream/applications/{name}/resource-tree", false, s.handleStreamTree)

	s.route(mux, "GET /api/v1/projects", false, s.handleListProjects)
	s.route(mux, "POST /api/v1/projects", false, s.handleCreateProject)
	s.route(mux, "GET /api/v1/projects/{name}", false, s.handleGetProject)
	s.route(mux, "PUT /api/v1/projects/{name}", false, s.handleUpdateProject)
	s.route(mux, "DELETE /api/v1/projects/{name}", false, s.handleDeleteProject)
	s.route(mux, "POST /api/v1/projects/{name}/roles/{role}/token", false, s.handleCreateRoleToken)
	s.route(mux, "DELETE /api/v1/projects/{name}/roles/{role}/token/{token}", false, s.handleDeleteRoleToken)

	s.route(mux, "GET /api/v1/repositories", false, s.handleListRepos)
	s.route(mux, "POST /api/v1/repositories", false, s.handleCreateRepo)
	s.route(mux, "DELETE /api/v1/repositories", false, s.handleDeleteRepo)
	s.route(mux, "GET /api/v1/repositories/refs", false, s.handleRepoRefs)

	s.route(mux, "GET /api/v1/clusters", false, s.handleListClusters)
	s.route(mux, "GET /api/v1/clusters/{name}/nodes", false, s.handleClusterNodes)

	s.route(mux, "GET /api/v1/account", false, s.handleAccount)
	s.route(mux, "PUT /api/v1/account/password", false, s.handleUpdatePassword)
	s.route(mux, "GET /api/v1/account/can-i/{resource}/{action}/{object...}", false, s.handleCanI)
	s.route(mux, "GET /api/v1/accounts", false, s.handleListAccounts)
	s.route(mux, "GET /api/v1/accounts/{name}", false, s.handleGetAccount)
	s.route(mux, "POST /api/v1/accounts/{name}/token", false, s.handleGenerateToken)
	s.route(mux, "DELETE /api/v1/accounts/{name}/token/{id}", false, s.handleDeleteToken)

	s.route(mux, "GET /api/v1/health", false, s.handleHealthDetail)
}

// ─── helpers ────────────────────────────────────────────────────────────────

// apiError is the error body (Argo CD's shape).
type apiError struct {
	Error   string `json:"error"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(apiError{Error: msg, Code: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// statusError carries an HTTP status; its message is shown as is.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

func statusErr(code int, format string, args ...any) error {
	return &statusError{code: code, msg: fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) error {
	return statusErr(http.StatusBadRequest, format, args...)
}

func notFound(format string, args ...any) error {
	return statusErr(http.StatusNotFound, format, args...)
}

func forbidden(format string, args ...any) error {
	return statusErr(http.StatusForbidden, format, args...)
}

// errorCode maps an error to an HTTP status.
func errorCode(err error) int {
	var se *statusError
	var re *auth.RequestError
	switch {
	case errors.As(err, &se):
		return se.code
	case errors.As(err, &re) && re.NotFound:
		return http.StatusNotFound
	case errors.As(err, &re):
		return http.StatusBadRequest
	case errors.Is(err, store.ErrNotFound), errors.Is(err, controller.ErrMissing):
		return http.StatusNotFound
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrExists):
		return http.StatusConflict
	case errors.Is(err, controller.ErrNotManaged):
		return http.StatusForbidden
	}
	return http.StatusInternalServerError
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	code := errorCode(err)
	msg := err.Error()
	if code == http.StatusInternalServerError {
		s.logger.Warn("request failed", "error", err)
	}
	writeError(w, code, msg)
}

const maxBody = 4 << 20

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

// authorize enforces RBAC for the caller and audits denials.
func (s *Server) authorize(r *http.Request, resource, action, object string) error {
	if s.enforce(r, resource, action, object) {
		return nil
	}
	s.audit(r, resource+"/"+action, object, "DENIED", "")
	return forbidden("permission denied: %s, %s, %s", resource, action, object)
}

// can is authorize for JSON handlers: it writes the 403 itself.
func (s *Server) can(w http.ResponseWriter, r *http.Request, resource, action, object string) bool {
	if err := s.authorize(r, resource, action, object); err != nil {
		s.fail(w, err)
		return false
	}
	return true
}

func appObject(app *v1.Application) string { return app.Spec.Project + "/" + app.Metadata.Name }

// audit writes one [AUDIT] line per mutation (and denial), the CYSTEMD v4
// audit contract carried over: who, from where, what, on what, result.
func (s *Server) audit(r *http.Request, action, object, result, detail string) {
	id := identity(r)
	attrs := []any{
		"audit", true, "user", id.Username, "provider", id.Provider, "client", clientIP(r),
		"action", action, "object", object, "result", result,
	}
	if detail != "" {
		attrs = append(attrs, "detail", detail)
	}
	s.logger.Info("[AUDIT]", attrs...)
	s.Metrics.Audit.WithLabelValues(strings.ToLower(result)).Inc()
}

// clientIP is the first X-Forwarded-For entry when it is a valid IP (a
// non-IP value could forge log fields), else the peer address.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if c := strings.TrimSpace(strings.Split(xff, ",")[0]); net.ParseIP(c) != nil {
			return c
		}
	}
	return host
}

// ─── version, settings, health ──────────────────────────────────────────────

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Build)
}

// Settings is the public UI configuration.
type Settings struct {
	URL                string `json:"url,omitempty"`
	AppLabelKey        string `json:"appLabelKey"`
	UserLoginsDisabled bool   `json:"userLoginsDisabled"`
	NomadTokenLogin    bool   `json:"nomadTokenLogin"`
	// SSOAuthMethods are the Nomad OIDC auth methods to log in with.
	SSOAuthMethods     []string `json:"ssoAuthMethods,omitempty"`
	AnonymousEnabled   bool     `json:"anonymousEnabled"`
	StatusBadgeEnabled bool     `json:"statusBadgeEnabled"`
	ConsulEnabled      bool     `json:"consulEnabled"`
	VaultEnabled       bool     `json:"vaultEnabled"`
	KVPrefix           string   `json:"kvPrefix,omitempty"`
	Mode               string   `json:"mode"`
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	st := s.settings()
	st.SSOAuthMethods = s.ssoMethods(r.Context())
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) settings() Settings {
	c := s.Config
	return Settings{
		URL:                c.URL,
		AppLabelKey:        v1.AppMetaKey,
		UserLoginsDisabled: !*c.Auth.AdminEnabled,
		NomadTokenLogin:    *c.Auth.NomadTokenLogin,
		AnonymousEnabled:   c.Auth.AnonymousRole != "",
		StatusBadgeEnabled: true,
		ConsulEnabled:      c.ConsulEnabled(),
		VaultEnabled:       c.VaultEnabled(),
		KVPrefix:           c.Consul.KVPrefix,
		Mode:               s.Build.Mode,
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok\n"))
}

// HealthDetail is the control plane's own health (Settings page).
type HealthDetail struct {
	Leader bool               `json:"leader"`
	Consul *ComponentHealth   `json:"consul,omitempty"`
	Vault  *ComponentHealth   `json:"vault,omitempty"`
	Nomad  []*ComponentHealth `json:"nomad"`
}

// ComponentHealth reports one dependency.
type ComponentHealth struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Message string `json:"message"`
}

func (s *Server) handleHealthDetail(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.healthDetail(r.Context()))
}

func (s *Server) healthDetail(ctx context.Context) HealthDetail {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out := HealthDetail{Nomad: []*ComponentHealth{}}
	if s.Elector != nil {
		out.Leader = s.Elector.IsLeader()
	}
	for _, name := range s.Clusters.Names() {
		c, _ := s.Clusters.Get(name)
		h := &ComponentHealth{Name: name, Healthy: true}
		if info, err := c.Info(ctx); err != nil {
			h.Healthy, h.Message = false, err.Error()
		} else {
			h.Message = fmt.Sprintf("Nomad %s, leader %s", info.Version, info.Leader)
		}
		out.Nomad = append(out.Nomad, h)
	}
	if s.Consul != nil {
		h := &ComponentHealth{Name: "consul", Healthy: true}
		if leader, err := s.Consul.Status().Leader(); err != nil {
			h.Healthy, h.Message = false, err.Error()
		} else {
			h.Message = "leader " + leader
		}
		out.Consul = h
	}
	if s.Vault != nil {
		h := &ComponentHealth{Name: "vault", Healthy: true}
		if st, err := s.Vault.Health(ctx); err != nil {
			h.Healthy, h.Message = false, err.Error()
		} else {
			via := "direct"
			if st.ViaAgent {
				via = "via Vault Agent " + st.Address
			}
			h.Healthy = st.Initialized && !st.Sealed
			h.Message = fmt.Sprintf("Vault %s (%s), sealed=%t", st.Version, via, st.Sealed)
		}
		out.Vault = h
	}
	return out
}
