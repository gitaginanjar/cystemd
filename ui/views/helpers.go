// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package views

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Now is the clock used for relative times (tests replace it).
var Now = time.Now

// Ago renders a time relative to now ("5 minutes ago").
func Ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := Now().Sub(t)
	future := d < 0
	if future {
		d = -d
	}
	var s string
	switch {
	case d < 10*time.Second:
		return "just now"
	case d < time.Minute:
		s = plural(int(d.Seconds()), "second")
	case d < time.Hour:
		s = plural(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		s = plural(int(d.Hours()), "hour")
	case d < 60*24*time.Hour:
		s = plural(int(d.Hours()/24), "day")
	case d < 730*24*time.Hour:
		s = plural(int(d.Hours()/24/30), "month")
	default:
		s = plural(int(d.Hours()/24/365), "year")
	}
	if future {
		return "in " + s
	}
	return s + " ago"
}

// AgoPtr is Ago for optional times.
func AgoPtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return Ago(*t)
}

// Age is a compact age ("5m", "3h", "2d") for resource nodes.
func Age(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	d := Now().Sub(*t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// Plural is "1 application" / "3 applications".
func Plural(n int, word string) string { return plural(n, word) }

// Timestamp renders a time as UTC ("2026-10-15 16:50:01 UTC").
func Timestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// TimestampPtr is Timestamp for optional times.
func TimestampPtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return Timestamp(*t)
}

// Duration renders the time between two instants ("1m 12s").
func Duration(from time.Time, to *time.Time) string {
	end := Now()
	if to != nil {
		end = *to
	}
	d := end.Sub(from).Round(time.Second)
	if d < 0 {
		d = 0
	}
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// IsSHA reports whether s is a full commit hash.
func IsSHA(s string) bool { return shaRE.MatchString(s) }

// ShortRev shortens commit hashes to 7 characters; refs stay as they are.
func ShortRev(rev string) string {
	if IsSHA(rev) {
		return rev[:7]
	}
	if rev == "" {
		return "HEAD"
	}
	return rev
}

// RepoWebURL turns a Git remote into its web page on known hosts
// (git@github.com:org/repo.git → https://github.com/org/repo).
func RepoWebURL(repo string) string {
	r := strings.TrimSuffix(strings.TrimSpace(repo), ".git")
	switch {
	case strings.HasPrefix(r, "git@"):
		host, path, ok := strings.Cut(strings.TrimPrefix(r, "git@"), ":")
		if !ok {
			return ""
		}
		r = "https://" + host + "/" + path
	case strings.HasPrefix(r, "ssh://"):
		u, err := url.Parse(r)
		if err != nil {
			return ""
		}
		r = "https://" + u.Hostname() + u.Path
	case strings.HasPrefix(r, "https://"), strings.HasPrefix(r, "http://"):
		u, err := url.Parse(r)
		if err != nil {
			return ""
		}
		u.User = nil
		r = u.String()
	default:
		return ""
	}
	return r
}

// CommitURL links a revision to its page on GitHub, GitLab, Gitea or
// Bitbucket; empty for other hosts.
func CommitURL(repo, rev string) string {
	web := RepoWebURL(repo)
	if web == "" || rev == "" {
		return ""
	}
	u, err := url.Parse(web)
	if err != nil {
		return ""
	}
	host := u.Hostname()
	switch {
	case strings.Contains(host, "github"), strings.Contains(host, "gitea"), strings.Contains(host, "codeberg"):
		if IsSHA(rev) {
			return web + "/commit/" + rev
		}
		return web + "/tree/" + url.PathEscape(rev)
	case strings.Contains(host, "gitlab"):
		if IsSHA(rev) {
			return web + "/-/commit/" + rev
		}
		return web + "/-/tree/" + url.PathEscape(rev)
	case strings.Contains(host, "bitbucket"):
		if IsSHA(rev) {
			return web + "/commits/" + rev
		}
		return web + "/src/" + url.PathEscape(rev)
	}
	return ""
}

// HealthOf returns an Application's health code (Unknown when unset).
func HealthOf(a *v1.Application) string {
	if a.Status.Health.Status == "" {
		return string(v1.HealthStatusUnknown)
	}
	return string(a.Status.Health.Status)
}

// SyncOf returns an Application's sync code (Unknown when unset).
func SyncOf(a *v1.Application) string {
	if a.Status.Sync.Status == "" {
		return string(v1.SyncStatusUnknown)
	}
	return string(a.Status.Sync.Status)
}

// HealthCode returns a resource health code (Unknown when nil or unset).
func HealthCode(h *v1.HealthStatus) string {
	if h == nil || h.Status == "" {
		return string(v1.HealthStatusUnknown)
	}
	return string(h.Status)
}

// KindAbbr is the short kind label on tree nodes.
func KindAbbr(kind string) string {
	switch kind {
	case v1.KindApplication:
		return "app"
	case v1.KindJob:
		return "job"
	case v1.KindTaskGroup:
		return "group"
	case v1.KindAllocation:
		return "alloc"
	case v1.KindTask:
		return "task"
	case v1.KindDeployment:
		return "deploy"
	case v1.KindService:
		return "svc"
	case v1.KindConsulKV:
		return "kv"
	case v1.KindNode:
		return "node"
	}
	return strings.ToLower(kind)
}

// KindIcon is the outline icon of a kind.
func KindIcon(kind string) string {
	switch kind {
	case v1.KindApplication:
		return "apps"
	case v1.KindJob:
		return "job"
	case v1.KindTaskGroup:
		return "group"
	case v1.KindAllocation:
		return "cube"
	case v1.KindTask:
		return "task"
	case v1.KindDeployment:
		return "deploy"
	case v1.KindService:
		return "service"
	case v1.KindConsulKV:
		return "database"
	case v1.KindNode:
		return "server"
	}
	return "cube"
}

// AutoSyncText describes an Application's sync policy as Argo CD does.
func AutoSyncText(a *v1.Application) string {
	p := a.Spec.SyncPolicy
	if !p.IsAutomated() {
		return "Auto sync is not enabled."
	}
	var opts []string
	if p.Automated.Prune {
		opts = append(opts, "prune")
	}
	if p.Automated.SelfHeal {
		opts = append(opts, "self heal")
	}
	if len(opts) == 0 {
		return "Auto sync is enabled."
	}
	return "Auto sync is enabled (" + strings.Join(opts, ", ") + ")."
}

// Destination renders an Application destination ("in-cluster / default").
func Destination(d v1.ApplicationDestination) string {
	s := d.ClusterName()
	if d.Region != "" {
		s += " (" + d.Region + ")"
	}
	return s
}

// SortedKeys returns a map's keys in order.
func SortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Labels renders labels as k=v strings.
func Labels(m map[string]string) []string {
	var out []string
	for _, k := range SortedKeys(m) {
		out = append(out, k+"="+m[k])
	}
	return out
}

// Itoa formats an int.
func Itoa(n int) string { return strconv.Itoa(n) }

// Px formats a pixel length.
func Px(n int) string { return strconv.Itoa(n) + "px" }

// NodeStyle positions a tree node.
func NodeStyle(x, y int) templ.SafeCSS {
	return templ.SafeCSS("left:" + Px(x) + ";top:" + Px(y))
}

// SizeStyle sizes the tree canvas.
func SizeStyle(w, h int) templ.SafeCSS {
	return templ.SafeCSS("width:" + Px(w) + ";height:" + Px(h))
}

// U64 formats an optional version.
func U64(v *uint64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatUint(*v, 10)
}

// SafeURL marks a server-built, same-origin URL as safe for href.
func SafeURL(s string) templ.SafeURL { return templ.SafeURL(s) }

// OrphanedText describes how a project watches for orphaned jobs.
func OrphanedText(s *v1.OrphanedResourcesMonitorSettings) string {
	switch {
	case s == nil:
		return "Not monitored"
	case s.IsWarn():
		return "Monitored, with a warning"
	}
	return "Monitored"
}

// OrphanedParam keeps the orphaned resources shown across links.
func OrphanedParam(shown bool) string {
	if shown {
		return "true"
	}
	return ""
}

// OrDash shows "—" for an empty value.
func OrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// JoinOrDash lists values, or shows "—".
func JoinOrDash(v []string) string { return OrDash(strings.Join(v, ", ")) }

// YesNo shows a boolean.
func YesNo(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}

// ActiveNow reports whether a sync window is in effect.
func ActiveNow(w *v1.SyncWindow) bool { return w.Active(time.Now()) }

// WindowStatus is the status icon of a sync window state.
func WindowStatus(st v1.SyncWindowState) string {
	switch {
	case st.AutoSync:
		return "Succeeded"
	case st.CanSync:
		return "Suspended"
	}
	return "Failed"
}

// WindowText describes a sync window in a line.
func WindowText(w *v1.SyncWindow) string {
	s := w.Kind + " " + w.Schedule + " for " + w.Duration
	if w.TimeZone != "" {
		s += " (" + w.TimeZone + ")"
	}
	if w.ManualSync {
		s += ", manual syncs allowed"
	}
	return s
}

// SSOLoginURL starts a single sign-on with a Nomad auth method.
func SSOLoginURL(method, returnURL string) templ.SafeURL {
	q := url.Values{"method": {method}}
	if returnURL != "" {
		q.Set("return_url", returnURL)
	}
	return templ.SafeURL("/ui/auth/login?" + q.Encode())
}

// ExternalURL lets http(s) links through and drops anything else.
func ExternalURL(s string) templ.SafeURL {
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
		return templ.SafeURL(s)
	}
	return templ.SafeURL("#")
}

// OperationLabel is the Last Sync headline ("Sync OK", "Sync failed").
func OperationLabel(st *v1.OperationState) string {
	if st == nil {
		return ""
	}
	op := "Sync"
	if st.Operation.Sync != nil && st.Operation.Sync.RollbackID != nil {
		op = "Rollback"
	}
	if st.Operation.Sync != nil && st.Operation.Sync.DryRun {
		op = "Dry run"
	}
	switch st.Phase {
	case v1.OperationSucceeded:
		return op + " OK"
	case v1.OperationFailed, v1.OperationError:
		return op + " failed"
	case v1.OperationTerminating:
		return "Terminating"
	}
	return "Syncing"
}

// Initiator describes who started an operation.
func Initiator(i v1.OperationInitiator) string {
	if i.Automated {
		return "automated sync policy"
	}
	if i.Username == "" {
		return "unknown"
	}
	return i.Username
}

// FirstLine returns the first line of s.
func FirstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// Lines splits text into lines (for diff and YAML rendering).
func Lines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func sortStrings(s []string) { sort.Strings(s) }
