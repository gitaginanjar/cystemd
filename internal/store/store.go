// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Key layout under the prefix (default "cystemd"):
//
//	applications/<name>     Application (metadata, spec, status, operation)
//	projects/<name>         AppProject
//	repositories/<id>       Repository (no secret material)
//	repo-creds/<id>         repository credentials when Vault is not used
//	accounts/<name>         local account
//	events/<application>    Application event ring buffer
//	owned-kv/<application>  Consul keys an Application wrote (for pruning)
//	settings/<name>         server settings (signing key, …)
const (
	ownedKVDir      = "owned-kv/"
	applicationsDir = "applications/"
	projectsDir     = "projects/"
	repositoriesDir = "repositories/"
	repoCredsDir    = "repo-creds/"
	accountsDir     = "accounts/"
	eventsDir       = "events/"
	settingsDir     = "settings/"
)

// maxEvents is the event ring-buffer size per Application.
const maxEvents = 100

// Store is the typed object store.
type Store struct {
	kv     KV
	prefix string
	now    func() time.Time
}

// New returns a Store over kv with keys under prefix.
func New(kv KV, prefix string) *Store {
	prefix = strings.Trim(prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &Store{kv: kv, prefix: prefix, now: time.Now}
}

// KV exposes the backend (for leader election and the controller's own
// Consul keys).
func (s *Store) KV() KV { return s.kv }

func (s *Store) key(dir, name string) string { return s.prefix + dir + name }

// ─── Applications ───────────────────────────────────────────────────────────

func decodeApp(e *Entry) (*v1alpha1.Application, error) {
	var app v1alpha1.Application
	if err := json.Unmarshal(e.Value, &app); err != nil {
		return nil, fmt.Errorf("decode %s: %w", e.Key, err)
	}
	app.Metadata.ResourceVersion = strconv.FormatUint(e.ModifyIndex, 10)
	return &app, nil
}

func encodeApp(app *v1alpha1.Application) ([]byte, error) {
	c := app.DeepCopy()
	c.Metadata.ResourceVersion = ""
	return json.Marshal(c)
}

// ListApplications returns every Application, sorted by name.
func (s *Store) ListApplications(ctx context.Context) ([]*v1alpha1.Application, error) {
	entries, _, err := s.kv.List(ctx, s.key(applicationsDir, ""))
	if err != nil {
		return nil, err
	}
	return s.decodeApps(entries), nil
}

func (s *Store) decodeApps(entries []*Entry) []*v1alpha1.Application {
	out := make([]*v1alpha1.Application, 0, len(entries))
	for _, e := range entries {
		if app, err := decodeApp(e); err == nil {
			out = append(out, app)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

// GetApplication returns one Application or ErrNotFound.
func (s *Store) GetApplication(ctx context.Context, name string) (*v1alpha1.Application, error) {
	e, err := s.kv.Get(ctx, s.key(applicationsDir, name))
	if err != nil {
		return nil, err
	}
	return decodeApp(e)
}

// CreateApplication stores a new Application (ErrExists when taken).
func (s *Store) CreateApplication(ctx context.Context, app *v1alpha1.Application) (*v1alpha1.Application, error) {
	app = app.DeepCopy()
	now := s.now().UTC().Truncate(time.Second)
	app.Metadata.CreationTimestamp = &now
	app.Metadata.Generation = 1
	b, err := encodeApp(app)
	if err != nil {
		return nil, err
	}
	if err := s.kv.CAS(ctx, s.key(applicationsDir, app.Metadata.Name), b, 0); err != nil {
		if errors.Is(err, ErrConflict) {
			return nil, ErrExists
		}
		return nil, err
	}
	return s.GetApplication(ctx, app.Metadata.Name)
}

// UpdateApplication writes app if its ResourceVersion is still current
// (ErrConflict otherwise). Generation increments when the spec changed.
func (s *Store) UpdateApplication(ctx context.Context, app *v1alpha1.Application) (*v1alpha1.Application, error) {
	idx, err := strconv.ParseUint(app.Metadata.ResourceVersion, 10, 64)
	if err != nil || idx == 0 {
		return nil, fmt.Errorf("update %s: missing resourceVersion", app.Metadata.Name)
	}
	key := s.key(applicationsDir, app.Metadata.Name)
	cur, err := s.kv.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if cur.ModifyIndex != idx {
		return nil, ErrConflict
	}
	prev, err := decodeApp(cur)
	if err != nil {
		return nil, err
	}
	next := app.DeepCopy()
	next.Metadata.CreationTimestamp = prev.Metadata.CreationTimestamp
	next.Metadata.Generation = prev.Metadata.Generation
	if !specEqual(prev.Spec, next.Spec) {
		next.Metadata.Generation++
	}
	b, err := encodeApp(next)
	if err != nil {
		return nil, err
	}
	if err := s.kv.CAS(ctx, key, b, idx); err != nil {
		return nil, err
	}
	return s.GetApplication(ctx, app.Metadata.Name)
}

func specEqual(a, b v1alpha1.ApplicationSpec) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// MutateApplication applies fn to the latest version of name and writes the
// result, retrying when another writer won the race. fn may be called more
// than once and must not have side effects outside the object.
func (s *Store) MutateApplication(ctx context.Context, name string, fn func(*v1alpha1.Application) error) (*v1alpha1.Application, error) {
	for attempt := 0; ; attempt++ {
		app, err := s.GetApplication(ctx, name)
		if err != nil {
			return nil, err
		}
		if err := fn(app); err != nil {
			return nil, err
		}
		out, err := s.UpdateApplication(ctx, app)
		if errors.Is(err, ErrConflict) && attempt < 20 {
			continue
		}
		return out, err
	}
}

// DeleteApplication removes an Application, its events and its key
// ownership record.
func (s *Store) DeleteApplication(ctx context.Context, name string) error {
	if err := s.kv.Delete(ctx, s.key(applicationsDir, name)); err != nil {
		return err
	}
	if err := s.kv.Delete(ctx, s.key(ownedKVDir, name)); err != nil {
		return err
	}
	return s.kv.Delete(ctx, s.key(eventsDir, name))
}

// OwnedKeys returns the Consul keys an Application has written.
func (s *Store) OwnedKeys(ctx context.Context, app string) ([]string, error) {
	e, err := s.kv.Get(ctx, s.key(ownedKVDir, app))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []string
	if err := json.Unmarshal(e.Value, &keys); err != nil {
		return nil, err
	}
	return keys, nil
}

// SetOwnedKeys records the Consul keys an Application owns.
func (s *Store) SetOwnedKeys(ctx context.Context, app string, keys []string) error {
	sort.Strings(keys)
	b, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	return s.kv.Put(ctx, s.key(ownedKVDir, app), b)
}

// AppEventType is the kind of change a watch reports.
type AppEventType string

const (
	EventAdded    AppEventType = "ADDED"
	EventModified AppEventType = "MODIFIED"
	EventDeleted  AppEventType = "DELETED"
)

// AppEvent is one Application change.
type AppEvent struct {
	Type        AppEventType
	Application *v1alpha1.Application
}

// WatchApplications streams Application changes until ctx ends. The first
// batch reports every existing Application as ADDED.
func (s *Store) WatchApplications(ctx context.Context) <-chan AppEvent {
	ch := make(chan AppEvent, 64)
	go func() {
		defer close(ch)
		// known: the modify index and labels of every Application seen, so
		// that a deletion still says who declared it.
		type seenApp struct {
			index  uint64
			labels map[string]string
		}
		known := map[string]seenApp{}
		var index uint64
		backoff := time.Second
		for ctx.Err() == nil {
			entries, next, err := s.kv.Watch(ctx, s.key(applicationsDir, ""), index)
			if err != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, 30*time.Second)
				continue
			}
			backoff = time.Second
			index = next
			seen := map[string]bool{}
			for _, e := range entries {
				name := strings.TrimPrefix(e.Key, s.key(applicationsDir, ""))
				seen[name] = true
				prev, ok := known[name]
				if ok && prev.index == e.ModifyIndex {
					continue
				}
				app, err := decodeApp(e)
				if err != nil {
					continue
				}
				typ := EventModified
				if !ok {
					typ = EventAdded
				}
				known[name] = seenApp{index: e.ModifyIndex, labels: app.Metadata.Labels}
				select {
				case ch <- AppEvent{Type: typ, Application: app}:
				case <-ctx.Done():
					return
				}
			}
			for name, last := range known {
				if seen[name] {
					continue
				}
				delete(known, name)
				gone := &v1alpha1.Application{Metadata: v1alpha1.ObjectMeta{Name: name, Labels: last.labels}}
				select {
				case ch <- AppEvent{Type: EventDeleted, Application: gone}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch
}

// ─── Projects ───────────────────────────────────────────────────────────────

// ListProjects returns every project, including the implicit default.
func (s *Store) ListProjects(ctx context.Context) ([]*v1alpha1.AppProject, error) {
	entries, _, err := s.kv.List(ctx, s.key(projectsDir, ""))
	if err != nil {
		return nil, err
	}
	out := []*v1alpha1.AppProject{}
	hasDefault := false
	for _, e := range entries {
		var p v1alpha1.AppProject
		if json.Unmarshal(e.Value, &p) != nil {
			continue
		}
		p.Metadata.ResourceVersion = strconv.FormatUint(e.ModifyIndex, 10)
		hasDefault = hasDefault || p.Metadata.Name == v1alpha1.DefaultProject
		out = append(out, &p)
	}
	if !hasDefault {
		out = append(out, v1alpha1.DefaultAppProject())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

// GetProject returns a project; "default" exists even when never stored.
func (s *Store) GetProject(ctx context.Context, name string) (*v1alpha1.AppProject, error) {
	e, err := s.kv.Get(ctx, s.key(projectsDir, name))
	if errors.Is(err, ErrNotFound) && name == v1alpha1.DefaultProject {
		return v1alpha1.DefaultAppProject(), nil
	}
	if err != nil {
		return nil, err
	}
	var p v1alpha1.AppProject
	if err := json.Unmarshal(e.Value, &p); err != nil {
		return nil, err
	}
	p.Metadata.ResourceVersion = strconv.FormatUint(e.ModifyIndex, 10)
	return &p, nil
}

// PutProject creates or replaces a project, keeping its creation time and
// its status (the tokens of the roles it still has): the status is the
// control plane's, changed by MutateProject only. With a resourceVersion,
// it fails with ErrConflict unless the project is still at that version.
func (s *Store) PutProject(ctx context.Context, p *v1alpha1.AppProject) error {
	return s.putProject(ctx, p, false)
}

// MutateProject applies fn to a project, its status included, and stores
// it, retrying when the project changed in between. The default project
// is stored on its first change.
func (s *Store) MutateProject(ctx context.Context, name string, fn func(*v1alpha1.AppProject) error) (*v1alpha1.AppProject, error) {
	for attempt := 0; ; attempt++ {
		p, err := s.GetProject(ctx, name)
		if err != nil {
			return nil, err
		}
		if p.Metadata.ResourceVersion == "" {
			p.Metadata.ResourceVersion = "0" // the default project, never stored
		}
		if err := fn(p); err != nil {
			return nil, err
		}
		err = s.putProject(ctx, p, true)
		if errors.Is(err, ErrConflict) && attempt < 20 {
			continue
		}
		if err != nil {
			return nil, err
		}
		return s.GetProject(ctx, name)
	}
}

func (s *Store) putProject(ctx context.Context, p *v1alpha1.AppProject, withStatus bool) error {
	key := s.key(projectsDir, p.Metadata.Name)
	c := *p
	if !withStatus {
		c.Status = v1alpha1.AppProjectStatus{}
	}
	var index uint64
	cur, err := s.kv.Get(ctx, key)
	switch {
	case err == nil:
		index = cur.ModifyIndex
		var prev v1alpha1.AppProject
		if json.Unmarshal(cur.Value, &prev) == nil {
			if prev.Metadata.CreationTimestamp != nil {
				c.Metadata.CreationTimestamp = prev.Metadata.CreationTimestamp
			}
			if !withStatus {
				c.Status = prev.Status
			}
		}
	case !errors.Is(err, ErrNotFound):
		return err
	}
	// Tokens go with their role.
	var kept map[string]v1alpha1.JWTTokens
	for role, toks := range c.Status.JWTTokensByRole {
		if c.Role(role) != nil && len(toks.Items) > 0 {
			if kept == nil {
				kept = map[string]v1alpha1.JWTTokens{}
			}
			kept[role] = toks
		}
	}
	c.Status.JWTTokensByRole = kept
	// A resourceVersion makes it a conditional update: the project must not
	// have changed since it was read.
	if rv := p.Metadata.ResourceVersion; rv != "" {
		if want, err := strconv.ParseUint(rv, 10, 64); err != nil || want != index {
			return ErrConflict
		}
	}
	c.Metadata.ResourceVersion = ""
	if c.Metadata.CreationTimestamp == nil {
		now := s.now().UTC().Truncate(time.Second)
		c.Metadata.CreationTimestamp = &now
	}
	b, err := json.Marshal(&c)
	if err != nil {
		return err
	}
	return s.kv.CAS(ctx, key, b, index)
}

// DeleteProject removes a project.
func (s *Store) DeleteProject(ctx context.Context, name string) error {
	return s.kv.Delete(ctx, s.key(projectsDir, name))
}

// ─── Repositories ───────────────────────────────────────────────────────────

// RepoID is the stable key of a repository URL.
func RepoID(url string) string {
	sum := sha256.Sum256([]byte(strings.TrimSuffix(strings.TrimSpace(url), "/")))
	return hex.EncodeToString(sum[:8])
}

// ListRepositories returns every connected repository (no secrets).
func (s *Store) ListRepositories(ctx context.Context) ([]*v1alpha1.Repository, error) {
	entries, _, err := s.kv.List(ctx, s.key(repositoriesDir, ""))
	if err != nil {
		return nil, err
	}
	out := []*v1alpha1.Repository{}
	for _, e := range entries {
		var r v1alpha1.Repository
		if json.Unmarshal(e.Value, &r) == nil {
			out = append(out, &r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo < out[j].Repo })
	return out, nil
}

// GetRepository returns the repository with url, or ErrNotFound.
func (s *Store) GetRepository(ctx context.Context, url string) (*v1alpha1.Repository, error) {
	e, err := s.kv.Get(ctx, s.key(repositoriesDir, RepoID(url)))
	if err != nil {
		return nil, err
	}
	var r v1alpha1.Repository
	if err := json.Unmarshal(e.Value, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// PutRepository stores a repository; secret fields are stripped.
func (s *Store) PutRepository(ctx context.Context, r *v1alpha1.Repository) error {
	c := *r
	c.Password, c.SSHPrivateKey = "", ""
	b, err := json.Marshal(&c)
	if err != nil {
		return err
	}
	return s.kv.Put(ctx, s.key(repositoriesDir, RepoID(r.Repo)), b)
}

// DeleteRepository removes a repository and any stored credentials.
func (s *Store) DeleteRepository(ctx context.Context, url string) error {
	if err := s.kv.Delete(ctx, s.key(repositoriesDir, RepoID(url))); err != nil {
		return err
	}
	return s.kv.Delete(ctx, s.key(repoCredsDir, RepoID(url)))
}

// RepoCredentials is the secret material of a repository.
type RepoCredentials struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	SSHPrivateKey string `json:"sshPrivateKey,omitempty"`
}

// PutRepoCredentials stores credentials in the KV backend. Used only when
// Vault is not configured: protect the prefix with Consul ACLs.
func (s *Store) PutRepoCredentials(ctx context.Context, url string, c RepoCredentials) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.kv.Put(ctx, s.key(repoCredsDir, RepoID(url)), b)
}

// GetRepoCredentials returns credentials stored with PutRepoCredentials.
func (s *Store) GetRepoCredentials(ctx context.Context, url string) (*RepoCredentials, error) {
	e, err := s.kv.Get(ctx, s.key(repoCredsDir, RepoID(url)))
	if err != nil {
		return nil, err
	}
	var c RepoCredentials
	if err := json.Unmarshal(e.Value, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// ─── Accounts ───────────────────────────────────────────────────────────────

// Account is a local user.
type Account struct {
	Name              string     `json:"name"`
	PasswordHash      string     `json:"passwordHash,omitempty"`
	PasswordMtime     *time.Time `json:"passwordMtime,omitempty"`
	Enabled           bool       `json:"enabled"`
	Capabilities      []string   `json:"capabilities,omitempty"`
	Groups            []string   `json:"groups,omitempty"`
	MustResetPassword bool       `json:"mustResetPassword,omitempty"`
	// SessionGeneration is carried by the account's session tokens; a
	// password change increments it, which revokes the older sessions.
	SessionGeneration int64 `json:"sessionGeneration,omitempty"`
	// Tokens are the API tokens issued to the account (Argo CD's): a token
	// is valid while its ID is listed here.
	Tokens []APIToken `json:"tokens,omitempty"`
}

// APIToken records an API token; the token itself is handed out once and
// never stored.
type APIToken struct {
	ID        string     `json:"id"`
	IssuedAt  time.Time  `json:"issuedAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// ListAccounts returns every local account.
func (s *Store) ListAccounts(ctx context.Context) ([]*Account, error) {
	entries, _, err := s.kv.List(ctx, s.key(accountsDir, ""))
	if err != nil {
		return nil, err
	}
	out := []*Account{}
	for _, e := range entries {
		var a Account
		if json.Unmarshal(e.Value, &a) == nil {
			out = append(out, &a)
		}
	}
	return out, nil
}

// GetAccount returns a local account or ErrNotFound.
func (s *Store) GetAccount(ctx context.Context, name string) (*Account, error) {
	e, err := s.kv.Get(ctx, s.key(accountsDir, name))
	if err != nil {
		return nil, err
	}
	var a Account
	if err := json.Unmarshal(e.Value, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// PutAccount creates or replaces a local account.
func (s *Store) PutAccount(ctx context.Context, a *Account) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return s.kv.Put(ctx, s.key(accountsDir, a.Name), b)
}

// MutateAccount applies fn to an account and stores it, retrying when the
// account changed in between.
func (s *Store) MutateAccount(ctx context.Context, name string, fn func(*Account) error) (*Account, error) {
	key := s.key(accountsDir, name)
	for attempt := 0; ; attempt++ {
		e, err := s.kv.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		var a Account
		if err := json.Unmarshal(e.Value, &a); err != nil {
			return nil, err
		}
		if err := fn(&a); err != nil {
			return nil, err
		}
		b, err := json.Marshal(&a)
		if err != nil {
			return nil, err
		}
		err = s.kv.CAS(ctx, key, b, e.ModifyIndex)
		if errors.Is(err, ErrConflict) && attempt < 20 {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &a, nil
	}
}

// DeleteAccount removes a local account.
func (s *Store) DeleteAccount(ctx context.Context, name string) error {
	return s.kv.Delete(ctx, s.key(accountsDir, name))
}

// CreateAccountIfAbsent stores a only when no account of that name exists
// (several replicas may race to bootstrap the admin).
func (s *Store) CreateAccountIfAbsent(ctx context.Context, a *Account) (bool, error) {
	b, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	err = s.kv.CAS(ctx, s.key(accountsDir, a.Name), b, 0)
	if errors.Is(err, ErrConflict) {
		return false, nil
	}
	return err == nil, err
}

// ─── Settings ───────────────────────────────────────────────────────────────

// GetSetting returns a raw setting value or ErrNotFound.
func (s *Store) GetSetting(ctx context.Context, name string) ([]byte, error) {
	e, err := s.kv.Get(ctx, s.key(settingsDir, name))
	if err != nil {
		return nil, err
	}
	return e.Value, nil
}

// SetSettingIfAbsent stores value unless the setting exists, and returns the
// value that is stored afterwards (first writer wins).
func (s *Store) SetSettingIfAbsent(ctx context.Context, name string, value []byte) ([]byte, error) {
	err := s.kv.CAS(ctx, s.key(settingsDir, name), value, 0)
	if err != nil && !errors.Is(err, ErrConflict) {
		return nil, err
	}
	return s.GetSetting(ctx, name)
}

// ─── Events ─────────────────────────────────────────────────────────────────

// RecordEvent appends ev to the Application's event ring buffer; a repeat
// of the last event (same reason, message and object) bumps its count.
func (s *Store) RecordEvent(ctx context.Context, app string, ev v1alpha1.Event) error {
	key := s.key(eventsDir, app)
	now := s.now().UTC()
	if ev.FirstSeen.IsZero() {
		ev.FirstSeen = now
	}
	ev.LastSeen = now
	if ev.Count == 0 {
		ev.Count = 1
	}
	if ev.Type == "" {
		ev.Type = "Normal"
	}
	for attempt := 0; attempt < 10; attempt++ {
		var events []v1alpha1.Event
		var index uint64
		e, err := s.kv.Get(ctx, key)
		switch {
		case err == nil:
			index = e.ModifyIndex
			_ = json.Unmarshal(e.Value, &events)
		case !errors.Is(err, ErrNotFound):
			return err
		}
		repeated := false
		if n := len(events); n > 0 {
			last := &events[n-1]
			if last.Reason == ev.Reason && last.Message == ev.Message && last.Kind == ev.Kind && last.Name == ev.Name {
				last.Count++
				last.LastSeen = now
				repeated = true
			}
		}
		if !repeated {
			events = append(events, ev)
			if len(events) > maxEvents {
				events = events[len(events)-maxEvents:]
			}
		}
		b, err := json.Marshal(events)
		if err != nil {
			return err
		}
		err = s.kv.CAS(ctx, key, b, index)
		if errors.Is(err, ErrConflict) {
			continue
		}
		return err
	}
	return ErrConflict
}

// ListEvents returns an Application's events, newest first.
func (s *Store) ListEvents(ctx context.Context, app string) ([]v1alpha1.Event, error) {
	e, err := s.kv.Get(ctx, s.key(eventsDir, app))
	if errors.Is(err, ErrNotFound) {
		return []v1alpha1.Event{}, nil
	}
	if err != nil {
		return nil, err
	}
	var events []v1alpha1.Event
	if err := json.Unmarshal(e.Value, &events); err != nil {
		return nil, err
	}
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events, nil
}
