// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package client is a Go client for the CYSTEMD API (the API the web UI
// and the cystemd CLI use). Requests carry a session token (from Login) or
// any bearer token the server accepts.
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Client talks to one CYSTEMD server.
type Client struct {
	// BaseURL is the server, e.g. https://cystemd.example.com or a
	// cystemd agent's address (it serves the API under /api/).
	BaseURL string
	// Token is sent as a bearer token.
	Token string
	HTTP  *http.Client
	// UserAgent identifies the caller in the server's logs.
	UserAgent string
}

// Options configure a Client.
type Options struct {
	Token     string
	CACert    string // PEM file to trust
	Insecure  bool   // skip TLS verification
	UserAgent string
	Timeout   time.Duration
}

// New returns a client for baseURL.
func New(baseURL string, o Options) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid server address %q: expected http(s)://host[:port]", baseURL)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if o.CACert != "" || o.Insecure {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: o.Insecure} //nolint:gosec // opt-in
		if o.CACert != "" {
			pem, err := os.ReadFile(o.CACert)
			if err != nil {
				return nil, fmt.Errorf("CA certificate: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("CA certificate %s: no PEM certificates", o.CACert)
			}
			cfg.RootCAs = pool
		}
		tr.TLSClientConfig = cfg
	}
	ua := o.UserAgent
	if ua == "" {
		ua = "cystemd-client"
	}
	return &Client{BaseURL: u.String(), Token: o.Token, HTTP: &http.Client{Transport: tr, Timeout: o.Timeout}, UserAgent: ua}, nil
}

// Error is an API error response.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("server returned %d %s", e.Code, http.StatusText(e.Code))
	}
	return e.Message
}

// IsNotFound reports whether err is a 404.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == http.StatusNotFound
}

// IsUnauthorized reports whether err is a 401 (no or expired session).
func IsUnauthorized(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == http.StatusUnauthorized
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values, body any) (*http.Request, error) {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return req, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	req, err := c.request(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return decodeError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func decodeError(resp *http.Response) error {
	e := &Error{Code: resp.StatusCode}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, e) != nil || e.Message == "" {
		e.Message = strings.TrimSpace(string(b))
		if e.Message == "" || strings.HasPrefix(e.Message, "<") {
			e.Message = ""
		}
	}
	e.Code = resp.StatusCode
	return e
}

func esc(s string) string { return url.PathEscape(s) }

// ─── session ────────────────────────────────────────────────────────────

// Login authenticates with a local account and returns a session token.
func (c *Client) Login(ctx context.Context, username, password string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/session", nil, map[string]string{"username": username, "password": password}, &out)
	return out.Token, err
}

// LoginNomadToken exchanges a Nomad ACL token for a session token.
func (c *Client) LoginNomadToken(ctx context.Context, nomadToken string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/session", nil, map[string]string{"nomadToken": nomadToken}, &out)
	return out.Token, err
}

// SSOAuthMethods lists the Nomad OIDC auth methods the server offers for
// single sign-on (none when it is not configured).
func (c *Client) SSOAuthMethods(ctx context.Context) ([]string, error) {
	var out struct {
		AuthMethods []string `json:"authMethods"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/session/sso", nil, nil, &out)
	return out.AuthMethods, err
}

// SSOAuthURL starts a single sign-on with an auth method (empty: the
// default one) whose callback the caller receives at redirectURI. It
// returns the auth method and the URL to send the user to.
func (c *Client) SSOAuthURL(ctx context.Context, method, redirectURI, clientNonce string) (string, string, error) {
	var out struct {
		AuthMethod string `json:"authMethod"`
		AuthURL    string `json:"authURL"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/session/sso/auth-url", nil,
		map[string]string{"authMethod": method, "redirectURI": redirectURI, "clientNonce": clientNonce}, &out)
	return out.AuthMethod, out.AuthURL, err
}

// SSOCallback holds the parameters of the identity provider's callback.
type SSOCallback struct {
	AuthMethod  string `json:"authMethod"`
	ClientNonce string `json:"clientNonce"`
	State       string `json:"state"`
	Code        string `json:"code"`
	Iss         string `json:"iss,omitempty"`
	RedirectURI string `json:"redirectURI"`
}

// LoginSSO completes a single sign-on and returns a session token.
func (c *Client) LoginSSO(ctx context.Context, cb SSOCallback) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/session/sso/complete", nil, cb, &out)
	return out.Token, err
}

// UserInfo describes the caller.
type UserInfo struct {
	LoggedIn bool     `json:"loggedIn"`
	Username string   `json:"username"`
	Provider string   `json:"provider"`
	Groups   []string `json:"groups,omitempty"`
	Roles    []string `json:"roles,omitempty"`
	IsAdmin  bool     `json:"isAdmin"`
}

// UserInfo returns the caller's identity.
func (c *Client) UserInfo(ctx context.Context) (*UserInfo, error) {
	var out UserInfo
	return &out, c.do(ctx, http.MethodGet, "/api/v1/session/userinfo", nil, nil, &out)
}

// UpdatePassword changes the caller's local password.
func (c *Client) UpdatePassword(ctx context.Context, current, next string) error {
	return c.do(ctx, http.MethodPut, "/api/v1/account/password", nil, map[string]string{"currentPassword": current, "newPassword": next}, nil)
}

// BuildInfo is the server version.
type BuildInfo struct {
	Version   string `json:"Version"`
	GitCommit string `json:"GitCommit"`
	BuildDate string `json:"BuildDate"`
	GoVersion string `json:"GoVersion"`
	Platform  string `json:"Platform"`
	Mode      string `json:"Mode"`
}

// Version returns the server's build.
func (c *Client) Version(ctx context.Context) (*BuildInfo, error) {
	var out BuildInfo
	return &out, c.do(ctx, http.MethodGet, "/api/version", nil, nil, &out)
}

// ─── applications ───────────────────────────────────────────────────────

// ListOptions filter Applications.
type ListOptions struct {
	Projects []string
	Selector string // label selector: k=v[,k=v]
	Search   string
}

// ListApplications lists the Applications the caller may see.
func (c *Client) ListApplications(ctx context.Context, o ListOptions) ([]*v1.Application, error) {
	q := url.Values{}
	if len(o.Projects) > 0 {
		q.Set("projects", strings.Join(o.Projects, ","))
	}
	if o.Selector != "" {
		q.Set("selector", o.Selector)
	}
	if o.Search != "" {
		q.Set("search", o.Search)
	}
	var out struct {
		Items []*v1.Application `json:"items"`
	}
	return out.Items, c.do(ctx, http.MethodGet, "/api/v1/applications", q, nil, &out)
}

// GetApplication returns an Application; refresh "normal" or "hard"
// requests a comparison first.
func (c *Client) GetApplication(ctx context.Context, name, refresh string) (*v1.Application, error) {
	q := url.Values{}
	if refresh != "" {
		q.Set("refresh", refresh)
	}
	var out v1.Application
	return &out, c.do(ctx, http.MethodGet, "/api/v1/applications/"+esc(name), q, nil, &out)
}

// CreateApplication creates an Application (upsert replaces an existing
// one's spec).
func (c *Client) CreateApplication(ctx context.Context, app *v1.Application, upsert bool) (*v1.Application, error) {
	q := url.Values{}
	if upsert {
		q.Set("upsert", "true")
	}
	var out v1.Application
	return &out, c.do(ctx, http.MethodPost, "/api/v1/applications", q, app, &out)
}

// UpdateApplication replaces an Application's spec, labels and
// annotations (conditional on its resourceVersion when set).
func (c *Client) UpdateApplication(ctx context.Context, app *v1.Application) (*v1.Application, error) {
	var out v1.Application
	return &out, c.do(ctx, http.MethodPut, "/api/v1/applications/"+esc(app.Metadata.Name), nil, app, &out)
}

// DeleteApplication deletes an Application; cascade also deletes its jobs
// and Consul keys.
func (c *Client) DeleteApplication(ctx context.Context, name string, cascade bool) error {
	q := url.Values{"cascade": {strconv.FormatBool(cascade)}}
	return c.do(ctx, http.MethodDelete, "/api/v1/applications/"+esc(name), q, nil, nil)
}

// SyncRequest is a sync (Argo CD's ApplicationSyncRequest).
type SyncRequest struct {
	Revision    string                     `json:"revision,omitempty"`
	Prune       bool                       `json:"prune,omitempty"`
	DryRun      bool                       `json:"dryRun,omitempty"`
	Resources   []v1.SyncOperationResource `json:"resources,omitempty"`
	SyncOptions []string                   `json:"syncOptions,omitempty"`
	Retry       *v1.RetryStrategy          `json:"retryStrategy,omitempty"`
	Infos       []v1.Info                  `json:"infos,omitempty"`
}

// Sync requests a sync.
func (c *Client) Sync(ctx context.Context, name string, req SyncRequest) (*v1.Application, error) {
	var out v1.Application
	return &out, c.do(ctx, http.MethodPost, "/api/v1/applications/"+esc(name)+"/sync", nil, req, &out)
}

// Rollback requests a rollback to a history entry.
func (c *Client) Rollback(ctx context.Context, name string, id int64, prune, dryRun bool) (*v1.Application, error) {
	var out v1.Application
	body := map[string]any{"id": id, "prune": prune, "dryRun": dryRun}
	return &out, c.do(ctx, http.MethodPost, "/api/v1/applications/"+esc(name)+"/rollback", nil, body, &out)
}

// TerminateOperation stops the running operation.
func (c *Client) TerminateOperation(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/applications/"+esc(name)+"/operation", nil, nil, nil)
}

// ResourceRef identifies a resource tree node.
type ResourceRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// ResourceNode is a resource tree node.
type ResourceNode struct {
	ResourceRef
	ParentRefs []ResourceRef    `json:"parentRefs,omitempty"`
	Info       []InfoItem       `json:"info,omitempty"`
	Health     *v1.HealthStatus `json:"health,omitempty"`
	CreatedAt  *time.Time       `json:"createdAt,omitempty"`
	Images     []string         `json:"images,omitempty"`
}

// InfoItem is a name/value of a node.
type InfoItem struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// InfoValue returns the value of the named info item.
func (n ResourceNode) InfoValue(name string) string {
	for _, i := range n.Info {
		if i.Name == name {
			return i.Value
		}
	}
	return ""
}

// ResourceTree is an Application's live resource tree.
type ResourceTree struct {
	Nodes []ResourceNode `json:"nodes"`
	// OrphanedNodes are the orphaned jobs of the destination namespace,
	// when the project watches for them.
	OrphanedNodes []ResourceNode `json:"orphanedNodes,omitempty"`
}

// ResourceTree returns the live resource tree.
func (c *Client) ResourceTree(ctx context.Context, name string) (*ResourceTree, error) {
	var out ResourceTree
	return &out, c.do(ctx, http.MethodGet, "/api/v1/applications/"+esc(name)+"/resource-tree", nil, nil, &out)
}

// ResourceDiff is a managed resource's desired and live state.
type ResourceDiff struct {
	Kind                string `json:"kind"`
	Namespace           string `json:"namespace,omitempty"`
	Name                string `json:"name"`
	TargetState         string `json:"targetState"`
	LiveState           string `json:"liveState"`
	NormalizedLiveState string `json:"normalizedLiveState"`
	PlanDiff            string `json:"planDiff,omitempty"`
	Modified            bool   `json:"modified"`
	RequiresPruning     bool   `json:"requiresPruning,omitempty"`
}

// ManagedResources pairs every managed resource's desired and live state.
func (c *Client) ManagedResources(ctx context.Context, name string) ([]ResourceDiff, error) {
	var out struct {
		Items []ResourceDiff `json:"items"`
	}
	return out.Items, c.do(ctx, http.MethodGet, "/api/v1/applications/"+esc(name)+"/managed-resources", nil, nil, &out)
}

// Manifests are rendered manifests at a revision.
type Manifests struct {
	Manifests  []string      `json:"manifests"`
	Revision   string        `json:"revision"`
	SourceType v1.SourceType `json:"sourceType"`
}

// Manifests renders the Application's source at revision (default: its
// target revision).
func (c *Client) Manifests(ctx context.Context, name, revision string) (*Manifests, error) {
	q := url.Values{}
	if revision != "" {
		q.Set("revision", revision)
	}
	var out Manifests
	return &out, c.do(ctx, http.MethodGet, "/api/v1/applications/"+esc(name)+"/manifests", q, nil, &out)
}

// Events lists an Application's events.
func (c *Client) Events(ctx context.Context, name string) ([]v1.Event, error) {
	var out struct {
		Items []v1.Event `json:"items"`
	}
	return out.Items, c.do(ctx, http.MethodGet, "/api/v1/applications/"+esc(name)+"/events", nil, nil, &out)
}

// RunAction runs a resource action (restart, stop, promote...).
func (c *Client) RunAction(ctx context.Context, name string, node ResourceRef, action string) error {
	q := url.Values{"kind": {node.Kind}, "namespace": {node.Namespace}, "resourceName": {node.Name}, "uid": {node.UID}}
	return c.do(ctx, http.MethodPost, "/api/v1/applications/"+esc(name)+"/resource/actions", q, map[string]string{"action": action}, nil)
}

// LogOptions select a log stream.
type LogOptions struct {
	Task      string
	Stderr    bool
	Follow    bool
	TailLines int
}

// LogLine is one log line.
type LogLine struct {
	Content   string    `json:"content"`
	TimeStamp time.Time `json:"timeStamp"`
	AllocID   string    `json:"allocId"`
	Task      string    `json:"task"`
	Last      bool      `json:"last,omitempty"`
}

// Logs streams an allocation task's log lines to fn until the stream ends
// (not following), ctx is done or fn returns an error.
func (c *Client) Logs(ctx context.Context, name, allocID string, o LogOptions, fn func(LogLine) error) error {
	q := url.Values{"follow": {strconv.FormatBool(o.Follow)}}
	if o.Task != "" {
		q.Set("task", o.Task)
	}
	if o.Stderr {
		q.Set("type", "stderr")
	}
	if o.TailLines > 0 {
		q.Set("tailLines", strconv.Itoa(o.TailLines))
	}
	req, err := c.request(ctx, http.MethodGet, "/api/v1/applications/"+esc(name)+"/allocations/"+esc(allocID)+"/logs", q, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	hc := *c.HTTP
	hc.Timeout = 0 // a stream lasts as long as it lasts
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return decodeError(resp)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Result *LogLine `json:"result"`
			Error  string   `json:"error"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			return fmt.Errorf("log stream: %w", err)
		}
		if ev.Error != "" {
			return errors.New(ev.Error)
		}
		if ev.Result == nil {
			continue
		}
		if ev.Result.Last {
			return nil
		}
		if err := fn(*ev.Result); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// WatchApplications streams Application changes (ADDED, MODIFIED,
// DELETED) to fn; the first events are the current Applications.
func (c *Client) WatchApplications(ctx context.Context, name string, fn func(typ string, app *v1.Application) error) error {
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	req, err := c.request(ctx, http.MethodGet, "/api/v1/stream/applications", q, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	hc := *c.HTTP
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return decodeError(resp)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Result struct {
				Type        string          `json:"type"`
				Application *v1.Application `json:"application"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			return err
		}
		if err := fn(ev.Result.Type, ev.Result.Application); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return sc.Err()
}

// ─── projects, repositories, clusters, accounts ─────────────────────────

// ListProjects lists the projects the caller may see.
func (c *Client) ListProjects(ctx context.Context) ([]*v1.AppProject, error) {
	var out struct {
		Items []*v1.AppProject `json:"items"`
	}
	return out.Items, c.do(ctx, http.MethodGet, "/api/v1/projects", nil, nil, &out)
}

// GetProject returns a project.
func (c *Client) GetProject(ctx context.Context, name string) (*v1.AppProject, error) {
	var out v1.AppProject
	return &out, c.do(ctx, http.MethodGet, "/api/v1/projects/"+esc(name), nil, nil, &out)
}

// SyncWindows is how the sync windows of its project stand for an
// Application.
func (c *Client) SyncWindows(ctx context.Context, app string) (*v1.SyncWindowState, error) {
	var out v1.SyncWindowState
	return &out, c.do(ctx, http.MethodGet, "/api/v1/applications/"+esc(app)+"/syncwindows", nil, nil, &out)
}

// CreateProject creates a project.
func (c *Client) CreateProject(ctx context.Context, p *v1.AppProject) error {
	return c.do(ctx, http.MethodPost, "/api/v1/projects", nil, p, nil)
}

// UpdateProject replaces a project's spec.
func (c *Client) UpdateProject(ctx context.Context, p *v1.AppProject) error {
	return c.do(ctx, http.MethodPut, "/api/v1/projects/"+esc(p.Metadata.Name), nil, p, nil)
}

// DeleteProject deletes an unused project.
func (c *Client) DeleteProject(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/projects/"+esc(name), nil, nil, nil)
}

// ListRepositories lists the connected repositories.
func (c *Client) ListRepositories(ctx context.Context) ([]*v1.Repository, error) {
	var out struct {
		Items []*v1.Repository `json:"items"`
	}
	return out.Items, c.do(ctx, http.MethodGet, "/api/v1/repositories", nil, nil, &out)
}

// AddRepository connects a repository (force saves it even when the
// connection test fails).
func (c *Client) AddRepository(ctx context.Context, r *v1.Repository, force bool) (*v1.Repository, error) {
	q := url.Values{}
	if force {
		q.Set("force", "true")
	}
	var out v1.Repository
	return &out, c.do(ctx, http.MethodPost, "/api/v1/repositories", q, r, &out)
}

// RemoveRepository disconnects a repository.
func (c *Client) RemoveRepository(ctx context.Context, repo string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/repositories", url.Values{"repo": {repo}}, nil, nil)
}

// Cluster is a destination Nomad cluster.
type Cluster struct {
	Name            string             `json:"name"`
	Server          string             `json:"server"`
	Version         string             `json:"version,omitempty"`
	Region          string             `json:"region,omitempty"`
	Nodes           int                `json:"nodes"`
	ReadyNodes      int                `json:"readyNodes"`
	Applications    int                `json:"applications"`
	ConnectionState v1.ConnectionState `json:"connectionState"`
}

// ListClusters describes the destination clusters.
func (c *Client) ListClusters(ctx context.Context) ([]Cluster, error) {
	var out struct {
		Items []Cluster `json:"items"`
	}
	return out.Items, c.do(ctx, http.MethodGet, "/api/v1/clusters", nil, nil, &out)
}

// Account is a local account.
type Account struct {
	Name          string     `json:"name"`
	Enabled       bool       `json:"enabled"`
	Capabilities  []string   `json:"capabilities,omitempty"`
	Groups        []string   `json:"groups,omitempty"`
	PasswordMtime *time.Time `json:"passwordMtime,omitempty"`
	Tokens        []Token    `json:"tokens,omitempty"`
}

// Token describes an API token; the token itself is shown once, when
// generated.
type Token struct {
	ID        string     `json:"id"`
	IssuedAt  time.Time  `json:"issuedAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// ListAccounts lists the local accounts.
func (c *Client) ListAccounts(ctx context.Context) ([]Account, error) {
	var out struct {
		Items []Account `json:"items"`
	}
	return out.Items, c.do(ctx, http.MethodGet, "/api/v1/accounts", nil, nil, &out)
}

// GetAccount describes a local account.
func (c *Client) GetAccount(ctx context.Context, name string) (*Account, error) {
	var out Account
	return &out, c.do(ctx, http.MethodGet, "/api/v1/accounts/"+esc(name), nil, nil, &out)
}

// GenerateToken issues an API token for an account with the apiKey
// capability: an empty id is chosen by the server, expiresIn 0 never
// expires.
func (c *Client) GenerateToken(ctx context.Context, account, id string, expiresIn time.Duration) (string, *Token, error) {
	var out struct {
		Secret string `json:"token"`
		Token
	}
	body := map[string]any{"id": id, "expiresIn": int64(expiresIn / time.Second)}
	if err := c.do(ctx, http.MethodPost, "/api/v1/accounts/"+esc(account)+"/token", nil, body, &out); err != nil {
		return "", nil, err
	}
	return out.Secret, &out.Token, nil
}

// ParseLifetime parses a token lifetime: a duration ("12h") or a number of
// days ("90d"); "" and 0 never expire.
func ParseLifetime(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	bad := fmt.Errorf("%q is not a lifetime such as 12h or 90d (0: never expires)", s)
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 || n > 36500 {
			return 0, bad
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil || d < 0 {
			return 0, bad
		}
	}
	if d > 0 && d < time.Second {
		return 0, bad
	}
	return d, nil
}

// DeleteToken revokes an account's API token.
func (c *Client) DeleteToken(ctx context.Context, account, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/accounts/"+esc(account)+"/token/"+esc(id), nil, nil, nil)
}

// SetAccountPassword sets another local account's password, which needs
// accounts/update; current is the caller's own password.
func (c *Client) SetAccountPassword(ctx context.Context, account, current, next string) error {
	return c.do(ctx, http.MethodPut, "/api/v1/account/password", nil,
		map[string]string{"name": account, "currentPassword": current, "newPassword": next}, nil)
}

// CanI reports whether the caller may perform action on object of
// resource (an application object is <project>/<application>).
func (c *Client) CanI(ctx context.Context, action, resource, object string) (bool, error) {
	var out struct {
		Value string `json:"value"`
	}
	segs := []string{esc(resource), esc(action)}
	for _, p := range strings.Split(object, "/") {
		segs = append(segs, esc(p))
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/account/can-i/"+strings.Join(segs, "/"), nil, nil, &out)
	return out.Value == "yes", err
}
