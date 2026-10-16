// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package views renders the CYSTEMD web UI: templ components that take the
// view models below and produce HTML. They hold no logic beyond formatting;
// handlers in internal/server load data, check permissions and pick what to
// render (a full page, or the fragment an htmx request targets).
package views

import (
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/gitaginanjar/cystemd/internal/diff"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Page is the frame of every signed-in page.
type Page struct {
	Title   string
	Nav     string // active sidebar entry: applications, settings, user-info
	Crumbs  []Crumb
	Heading string // uppercase page title at the top right
	User    User
	Version string
	Theme   string // light, dark or auto
	// Panel is the open sliding panel, if any.
	Panel templ.Component
	// Toasts are notifications shown on load (flash messages).
	Toasts []Toast
}

// Crumb is one breadcrumb; the last one is the current page.
type Crumb struct{ Label, Href string }

// User is the signed-in user.
type User struct {
	Name    string
	IsAdmin bool
	Local   bool
}

// ─── applications list ──────────────────────────────────────────────────

// AppsQuery is the state of the applications page: view mode and filters,
// all carried in the URL so views can be bookmarked and shared.
type AppsQuery struct {
	View       string // tiles, list or summary
	Search     string
	Sync       []string
	Health     []string
	Projects   []string
	Clusters   []string
	Namespaces []string
	Labels     string // k=v[,k=v]
	Favorites  bool
}

// Values encodes the query.
func (q AppsQuery) Values() url.Values {
	v := url.Values{}
	if q.View != "" && q.View != "tiles" {
		v.Set("view", q.View)
	}
	if q.Search != "" {
		v.Set("search", q.Search)
	}
	for _, s := range q.Sync {
		v.Add("sync", s)
	}
	for _, s := range q.Health {
		v.Add("health", s)
	}
	for _, s := range q.Projects {
		v.Add("proj", s)
	}
	for _, s := range q.Clusters {
		v.Add("cluster", s)
	}
	for _, s := range q.Namespaces {
		v.Add("ns", s)
	}
	if q.Labels != "" {
		v.Set("labels", q.Labels)
	}
	if q.Favorites {
		v.Set("fav", "1")
	}
	return v
}

// Href returns the applications URL for this query in another view mode.
func (q AppsQuery) Href(view string) string {
	q.View = view
	return withQuery("/ui/applications", q.Values())
}

// Filtered reports whether any filter is active.
func (q AppsQuery) Filtered() bool {
	return q.Search != "" || len(q.Sync) > 0 || len(q.Health) > 0 || len(q.Projects) > 0 ||
		len(q.Clusters) > 0 || len(q.Namespaces) > 0 || q.Labels != "" || q.Favorites
}

func withQuery(path string, v url.Values) string {
	if len(v) == 0 {
		return path
	}
	return path + "?" + v.Encode()
}

// Facet is one filter value with the number of applications having it.
type Facet struct {
	Value   string
	Count   int
	Checked bool
}

// AppFacets are the filter choices of the applications page.
type AppFacets struct {
	Sync, Health, Projects, Clusters, Namespaces []Facet
}

// AppsPage is the applications list.
type AppsPage struct {
	Page
	Q         AppsQuery
	Apps      []*v1.Application
	Total     int
	Facets    AppFacets
	Favorites map[string]bool
	CanCreate bool
}

// AppHref is the details URL of an Application.
func AppHref(name string) string { return "/ui/applications/" + url.PathEscape(name) }

// AccountHref is the page of a local account.
func AccountHref(name string) string { return "/ui/settings/accounts/" + url.PathEscape(name) }

// ─── application details ────────────────────────────────────────────────

// AppPerms are the caller's rights on one Application.
type AppPerms struct {
	Sync, Update, Delete, Action, Logs bool
}

// Commit is revision metadata shown next to a revision.
type Commit struct {
	SHA     string
	Author  string
	Message string
	Date    time.Time
	URL     string
}

// AppPage is the application details page.
type AppPage struct {
	Page
	App        *v1.Application
	View       string // tree, list or nodes
	Tree       *Tree
	NodeGroups []NodeGroup
	SyncCommit *Commit
	LastCommit *Commit
	Perms      AppPerms
	Filter     string
	TreeErr    string
	// Windows is how the project's sync windows stand for the app.
	Windows v1.SyncWindowState
	// Orphaned counts the orphaned jobs (-1: the project does not watch
	// for them); ShowOrphaned puts them in the tree.
	Orphaned     int
	ShowOrphaned bool
}

// Href builds a URL on the details page keeping the view mode.
func (p *AppPage) Href(params ...string) string {
	return AppURL(p.App.Metadata.Name, p.View, params...)
}

// AppURL builds a details-page URL: view mode plus key/value parameters
// (panel, tab, node...).
func AppURL(name, view string, params ...string) string {
	v := url.Values{}
	if view != "" && view != "tree" {
		v.Set("view", view)
	}
	for i := 0; i+1 < len(params); i += 2 {
		if params[i+1] != "" {
			v.Set(params[i], params[i+1])
		}
	}
	return withQuery(AppHref(name), v)
}

// Tree is a laid-out resource tree.
type Tree struct {
	Width, Height int
	Nodes         []TreeNode
	Edges         []string // SVG path data
}

// TreeNode is one positioned node.
type TreeNode struct {
	X, Y      int
	UID       string
	Kind      string
	Namespace string
	Name      string
	Health    string
	HealthMsg string
	Sync      string // managed resources only
	Info      []string
	Age       string
	Root      bool
	Orphan    bool // requires pruning
	Hook      bool // a resource hook's job
	Unmanaged bool // an orphaned job, which no application tracks
	More      int  // a "+N more" placeholder
	Depth     int
	Href      string
	Selected  bool
	Actions   []NodeAction
	LogsHref  string
}

// NodeAction is a resource action in a node's menu.
type NodeAction struct {
	Name    string
	Label   string
	Post    string // URL the action posts to
	Confirm string
	Danger  bool
}

// NodeGroup is a Nomad client node and the allocations on it.
type NodeGroup struct {
	Node   string
	Allocs []TreeNode
}

// ─── sliding panels ─────────────────────────────────────────────────────

// Tab is a panel tab.
type Tab struct {
	Label  string
	Href   string
	Active bool
}

// AppDetailsPanel is the APP DETAILS panel.
type AppDetailsPanel struct {
	App       *v1.Application
	Tab       string // summary, parameters, manifest, events
	Tabs      []Tab
	Edit      bool
	Form      AppForm
	Manifest  string
	Events    []v1.Event
	Perms     AppPerms
	Error     string
	CloseHref string
	Options   FormOptions
	EditHref  string
	ViewHref  string
}

// FormOptions are the choices offered in application forms.
type FormOptions struct {
	Projects []string
	Clusters []string
	Repos    []string
}

// AppForm is the editable shape of an Application.
type AppForm struct {
	Name            string
	Project         string
	Labels          string // one k=v per line
	RepoURL         string
	Revision        string
	Path            string
	DestName        string
	DestNamespace   string
	DestRegion      string
	Automated       bool
	Prune           bool
	SelfHeal        bool
	SyncOptions     map[string]bool
	SourceType      string // Nomad or Systemd
	ResourceVersion string

	NomadFiles           string
	NomadVarFiles        string
	NomadVars            string // one k=v per line
	ApplicationsFiles    string
	ApplicationsProjects string
	NomadConsulKV        string

	SystemdValuesFile      string
	SystemdSelector        string
	SystemdConfigKey       string
	SystemdVersionKey      string
	SystemdCPUKey          string
	SystemdMemoryKey       string
	SystemdUnit            string
	SystemdPackage         string
	SystemdServiceConfig   string
	SystemdWorkdir         string
	SystemdReplicas        string
	SystemdNodePool        string
	SystemdNodeSelector    string
	SystemdDatacenters     string
	SystemdSecrets         string // one vaultPath[:destination] per line
	SystemdSecretsDelivery string
	SystemdVaultRole       string
	SystemdConfigDelivery  string
	SystemdChangeMode      string
	SystemdServicePort     string
	SystemdServiceName     string
	SystemdCheckType       string
	SystemdCheckPath       string
}

// SyncOptionChoices are the sync options offered in forms and the sync
// panel, with Argo CD's labels.
var SyncOptionChoices = []struct{ Option, Label, Help string }{
	{v1.SyncOptionCreateNamespace, "Auto-create namespace", "Create the Nomad namespace when it does not exist."},
	{v1.SyncOptionPruneLast, "Prune last", "Prune after every other resource synced and became healthy."},
	{v1.SyncOptionApplyOutOfSyncOnly, "Apply out of sync only", "Skip resources that are already synced."},
	{v1.SyncOptionPreserveCounts, "Preserve counts", "Keep the live task group counts (autoscaled jobs)."},
}

// NewAppPanel is the NEW APP panel.
type NewAppPanel struct {
	Form      AppForm
	YAML      string
	Mode      string // form or yaml
	Error     string
	Options   FormOptions
	CloseHref string
}

// DiffPanel is the APP DIFF panel.
type DiffPanel struct {
	App       *v1.Application
	Diffs     []ResourceDiff
	Compact   bool
	Error     string
	CloseHref string
	ToggleURL string
}

// ResourceDiff is one resource in the APP DIFF panel.
type ResourceDiff struct {
	Kind, Namespace, Name string
	Modified              bool
	Pruning               bool
	Missing               bool
	Lines                 []diff.Line
	PlanDiff              string
}

// SyncPanel is the SYNC panel.
type SyncPanel struct {
	App       *v1.Application
	Resources []SyncResource
	Error     string
	CloseHref string
}

// SyncResource is one selectable resource in the SYNC panel.
type SyncResource struct {
	Kind, Namespace, Name, Sync, Health string
	Pruning                             bool
}

// Key identifies the resource in the sync form.
func (r SyncResource) Key() string { return r.Kind + "/" + r.Namespace + "/" + r.Name }

// OperationPanel is the SYNC STATUS panel.
type OperationPanel struct {
	App       *v1.Application
	CanSync   bool
	CloseHref string
}

// HistoryPanel is the HISTORY AND ROLLBACK panel.
type HistoryPanel struct {
	App       *v1.Application
	Entries   []HistoryEntry
	CanSync   bool
	CloseHref string
}

// HistoryEntry is one deployment.
type HistoryEntry struct {
	v1.RevisionHistory
	Commit   *Commit
	Active   bool
	Duration string
	Until    *time.Time
}

// DeletePanel confirms deleting an Application.
type DeletePanel struct {
	App       *v1.Application
	Error     string
	CloseHref string
}

// NodePanel is a resource node's panel.
type NodePanel struct {
	App       *v1.Application
	Node      TreeNode
	Tab       string // summary, live, desired, diff, events, logs
	Tabs      []Tab
	Summary   [][2]string
	Manifest  string
	Desired   string
	Diff      []diff.Line
	PlanDiff  string
	Events    []NodeEvent
	Logs      *LogsView
	Error     string
	CloseHref string
}

// NodeEvent is a Nomad task event or a CYSTEMD event about a resource.
type NodeEvent struct {
	Time    time.Time
	Type    string
	Reason  string
	Message string
	Source  string
}

// LogsView configures the log viewer.
type LogsView struct {
	Tasks     []string
	Task      string
	Type      string // stdout or stderr
	Follow    bool
	Tail      int
	StreamURL string
	FormURL   string
	Download  string
}

// ─── settings and user pages ────────────────────────────────────────────

// SettingsPage is the settings index.
type SettingsPage struct {
	Page
	Health []ComponentHealth
	Leader bool
	Mode   string
}

// ComponentHealth is a dependency's health on the settings page.
type ComponentHealth struct {
	Name    string
	Kind    string
	Healthy bool
	Message string
}

// ReposPage lists repositories.
type ReposPage struct {
	Page
	Repos     []*v1.Repository
	CanCreate bool
	CanDelete bool
	Error     string
}

// RepoForm is the CONNECT REPO form.
type RepoForm struct {
	Method        string // https or ssh
	Repo          string
	Name          string
	Project       string
	Username      string
	Password      string
	SSHPrivateKey string
	Insecure      bool
	Force         bool
}

// ConnectRepoPanel connects a repository.
type ConnectRepoPanel struct {
	Form      RepoForm
	Projects  []string
	Error     string
	CloseHref string
}

// ProjectsPage lists projects.
type ProjectsPage struct {
	Page
	Projects  []*v1.AppProject
	AppCounts map[string]int
	CanCreate bool
	Error     string
}

// ProjectForm edits a project.
type ProjectForm struct {
	Name         string
	Description  string
	SourceRepos  string // one per line
	Destinations string // cluster[/region]/namespace per line
}

// ProjectPanel creates or edits a project.
type ProjectPanel struct {
	Form      ProjectForm
	New       bool
	Error     string
	CloseHref string
}

// ProjectPage shows one project.
type ProjectPage struct {
	Page
	Project   *v1.AppProject
	Apps      []*v1.Application
	CanUpdate bool
	CanDelete bool
	Error     string
}

// ClusterView describes a cluster.
type ClusterView struct {
	Name         string
	Server       string
	Version      string
	Region       string
	Regions      []string
	Namespaces   []string
	NodePools    []string
	Nodes        int
	ReadyNodes   int
	Applications int
	Status       string
	Message      string
}

// NodeView describes a Nomad client node.
type NodeView struct {
	ID, Name, Datacenter, NodePool, Status, Address, Version string
	Eligible, Drain                                          bool
	Drivers                                                  []string
}

// HasDriver reports whether the node runs a healthy driver.
func (n NodeView) HasDriver(d string) bool {
	for _, x := range n.Drivers {
		if x == d {
			return true
		}
	}
	return false
}

// ClustersPage lists clusters.
type ClustersPage struct {
	Page
	Clusters []ClusterView
	Error    string
}

// ClusterPage shows one cluster and its nodes.
type ClusterPage struct {
	Page
	Cluster ClusterView
	Nodes   []NodeView
	Error   string
}

// AccountView is a local account.
type AccountView struct {
	Name          string
	Enabled       bool
	Capabilities  []string
	Groups        []string
	PasswordMtime *time.Time
	Tokens        []TokenView
}

// HasCapability reports whether the account has a capability (login,
// apiKey).
func (a AccountView) HasCapability(c string) bool { return slices.Contains(a.Capabilities, c) }

// TokenView is an API token (its ID: the token is shown once).
type TokenView struct {
	ID        string
	IssuedAt  time.Time
	ExpiresAt *time.Time
}

// AccountsPage lists local accounts.
type AccountsPage struct {
	Page
	Accounts []AccountView
	Error    string
}

// AccountPage shows a local account and its API tokens.
type AccountPage struct {
	Page
	Account AccountView
	Tokens  AccountTokens
}

// AccountTokens is the API tokens section of an account page.
type AccountTokens struct {
	Account string
	// APIKey: the account has the apiKey capability.
	APIKey bool
	// CanUpdate: the caller may generate and delete the tokens.
	CanUpdate bool
	Tokens    []TokenView
	// NewToken is a token just generated, shown this once.
	NewToken, NewID string
	Error           string
	// ExpiresIn and ID keep the form's values after an error.
	ExpiresIn, ID string
}

// AppearancePage picks the theme.
type AppearancePage struct {
	Page
}

// UserInfoPage shows the signed-in user.
type UserInfoPage struct {
	Page
	Username string
	Provider string
	Groups   []string
	Roles    []string
	Account  *AccountView
}

// PasswordPanel changes the user's password.
type PasswordPanel struct {
	Error     string
	Done      bool
	CloseHref string
}

// LoginPage is the sign-in page.
type LoginPage struct {
	Version         string
	ReturnURL       string
	Error           string
	Username        string
	LocalEnabled    bool
	NomadTokenLogin bool
	// SSOMethods are Nomad OIDC auth methods to log in with.
	SSOMethods []string
	Theme      string
}

// ErrorPage is a full-page error.
type ErrorPage struct {
	Page
	Code    int
	Message string
}

// Toast is a notification.
type Toast struct {
	Kind    string // success, error, warning
	Message string
}

// Checked reports whether value is in list.
func Checked(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

// TrimLines trims every line and drops empty ones.
func TrimLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
