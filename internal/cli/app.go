// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gitaginanjar/cystemd/internal/diff"
	"github.com/gitaginanjar/cystemd/internal/manifest"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
	"github.com/gitaginanjar/cystemd/pkg/client"
)

func health(a *v1.Application) string {
	if a.Status.Health.Status == "" {
		return "Unknown"
	}
	return string(a.Status.Health.Status)
}

func syncStatus(a *v1.Application) string {
	if a.Status.Sync.Status == "" {
		return "Unknown"
	}
	return string(a.Status.Sync.Status)
}

func syncPolicy(a *v1.Application) string {
	p := a.Spec.SyncPolicy
	if !p.IsAutomated() {
		return "Manual"
	}
	var opts []string
	if p.Automated.Prune {
		opts = append(opts, "Prune")
	}
	if p.Automated.SelfHeal {
		opts = append(opts, "SelfHeal")
	}
	if len(opts) == 0 {
		return "Automated"
	}
	return "Automated (" + strings.Join(opts, ", ") + ")"
}

// ─── app list ───────────────────────────────────────────────────────────

type appListCommand struct{ *meta }

func (c *appListCommand) Synopsis() string { return "List applications" }
func (c *appListCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` app list [options]

  Lists the applications you may see.

Options:

  -project=<name>     Only this project (repeatable).
  -selector=<k=v,..>  Only applications with these labels.
  -o=<format>         wide, name, json or yaml.
` + c.connectionHelp()
}

func (c *appListCommand) Run(args []string) int {
	var projects stringList
	var selector, out string
	fs := c.flagSet("app list", true)
	fs.Var(&projects, "project", "")
	fs.StringVar(&selector, "selector", "", "")
	fs.StringVar(&selector, "l", "", "")
	fs.StringVar(&out, "o", "", "")
	if _, ok := c.parse(fs, args, 0, c.opts.Name+" app list [options]"); !ok {
		return 1
	}
	if err := validOutput(out, "wide", "name"); err != nil {
		return c.fail(err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	apps, err := cl.ListApplications(ctx, client.ListOptions{Projects: projects, Selector: selector})
	if err != nil {
		return c.fail(err)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Metadata.Name < apps[j].Metadata.Name })
	if done, err := output(c.out, out, apps); done {
		if err != nil {
			return c.fail(err)
		}
		return 0
	}
	if out == "name" {
		for _, a := range apps {
			fmt.Fprintln(c.out, a.Metadata.Name)
		}
		return 0
	}
	header := []string{"NAME", "CLUSTER", "NAMESPACE", "PROJECT", "STATUS", "HEALTH", "SYNCPOLICY", "CONDITIONS"}
	if out == "wide" {
		header = append(header, "REPO", "PATH", "TARGET", "REVISION")
	}
	t := newTable(c.out, header...)
	for _, a := range apps {
		conds := "<none>"
		if len(a.Status.Conditions) > 0 {
			var types []string
			for _, cd := range a.Status.Conditions {
				types = append(types, cd.Type)
			}
			conds = strings.Join(types, ",")
		}
		row := []string{a.Metadata.Name, a.Spec.Destination.ClusterName(), a.Spec.Destination.NamespaceOrDefault(),
			a.Spec.Project, syncStatus(a), health(a), syncPolicy(a), conds}
		if out == "wide" {
			row = append(row, a.Spec.Source.RepoURL, orDash(a.Spec.Source.Path), a.Spec.Source.Revision(), shortRev(a.Status.Sync.Revision))
		}
		t.row(row...)
	}
	t.flush()
	return 0
}

// ─── app get ────────────────────────────────────────────────────────────

type appGetCommand struct{ *meta }

func (c *appGetCommand) Synopsis() string { return "Show an application" }
func (c *appGetCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` app get [options] <name>

  Shows an application: its source, destination, status and resources.

Options:

  -refresh         Compare with Git and Nomad first.
  -hard-refresh    Also re-read the repository (no cache).
  -o=<format>      json, yaml or tree (the live resource tree).
` + c.connectionHelp()
}

func (c *appGetCommand) Run(args []string) int {
	var refresh, hard bool
	var out string
	fs := c.flagSet("app get", true)
	fs.BoolVar(&refresh, "refresh", false, "")
	fs.BoolVar(&hard, "hard-refresh", false, "")
	fs.StringVar(&out, "o", "", "")
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" app get [options] <name>")
	if !ok {
		return 1
	}
	if err := validOutput(out, "tree"); err != nil {
		return c.fail(err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	mode := ""
	switch {
	case hard:
		mode = "hard"
	case refresh:
		mode = "normal"
	}
	app, err := cl.GetApplication(ctx, rest[0], mode)
	if err != nil {
		return c.fail(err)
	}
	if mode != "" {
		// The comparison runs in the controller: wait for it.
		if app, err = waitFor(ctx, cl, app.Metadata.Name, 30*time.Second, func(a *v1.Application) bool {
			return a.Metadata.Annotations["cystemd.io/refresh"] == ""
		}); err != nil {
			return c.fail(err)
		}
	}
	if done, err := output(c.out, out, app); done {
		if err != nil {
			return c.fail(err)
		}
		return 0
	}
	if out == "tree" {
		tree, err := cl.ResourceTree(ctx, app.Metadata.Name)
		if err != nil {
			return c.fail(err)
		}
		printTree(c.out, app, tree)
		return 0
	}
	c.printApp(ctx, app, cl)
	return 0
}

func (c *appGetCommand) printApp(ctx context.Context, a *v1.Application, cl *client.Client) {
	t := newTable(c.out)
	t.row("Name:", a.Metadata.Name)
	t.row("Project:", a.Spec.Project)
	t.row("Cluster:", a.Spec.Destination.ClusterName())
	t.row("Namespace:", a.Spec.Destination.NamespaceOrDefault())
	t.row("URL:", cl.BaseURL+"/ui/applications/"+a.Metadata.Name)
	t.row("Repo:", a.Spec.Source.RepoURL)
	t.row("Target:", a.Spec.Source.Revision())
	t.row("Path:", orDash(a.Spec.Source.Path))
	t.row("Source type:", string(a.Spec.Source.Type()))
	t.row("Sync policy:", syncPolicy(a))
	if st, err := cl.SyncWindows(ctx, a.Metadata.Name); err == nil && st.Summary() != "" {
		t.row("Sync window:", st.Summary())
	}
	rev := ""
	if a.Status.Sync.Revision != "" {
		rev = " to " + a.Spec.Source.Revision() + " (" + shortRev(a.Status.Sync.Revision) + ")"
	}
	t.row("Sync status:", syncStatus(a)+rev)
	h := health(a)
	if a.Status.Health.Message != "" {
		h += " (" + a.Status.Health.Message + ")"
	}
	t.row("Health status:", h)
	if st := a.Status.OperationState; st != nil {
		t.row("Last operation:", fmt.Sprintf("%s %s (%s ago): %s", operationName(st), st.Phase, ago(st.StartedAt), st.Message))
	}
	t.flush()
	for _, cd := range a.Status.Conditions {
		fmt.Fprintf(c.out, "\nCONDITION %s: %s\n", cd.Type, cd.Message)
	}
	if len(a.Status.Resources) > 0 {
		fmt.Fprintln(c.out)
		printResources(c.out, a.Status.Resources)
	}
}

func operationName(st *v1.OperationState) string {
	if st.Operation.Sync != nil && st.Operation.Sync.RollbackID != nil {
		return "Rollback"
	}
	if st.Operation.Sync != nil && st.Operation.Sync.DryRun {
		return "Dry run"
	}
	return "Sync"
}

func printResources(w io.Writer, rs []v1.ResourceStatus) {
	t := newTable(w, "KIND", "NAMESPACE", "NAME", "STATUS", "HEALTH", "MESSAGE")
	for _, r := range rs {
		h, msg := "", ""
		if r.Health != nil {
			h, msg = string(r.Health.Status), r.Health.Message
		}
		status := string(r.Status)
		switch {
		case r.Hook:
			status = "Hook"
		case r.RequiresPruning:
			status += " (requires pruning)"
		}
		t.row(r.Kind, orDash(r.Namespace), r.Name, status, orDash(h), msg)
	}
	t.flush()
}

// printTree prints the resource tree indented under the application.
func printTree(w io.Writer, a *v1.Application, tree *client.ResourceTree) {
	children := map[string][]client.ResourceNode{}
	for _, n := range tree.Nodes {
		parent := ""
		if len(n.ParentRefs) > 0 {
			parent = n.ParentRefs[0].UID
		}
		children[parent] = append(children[parent], n)
	}
	fmt.Fprintf(w, "%s (%s, %s)\n", a.Metadata.Name, health(a), syncStatus(a))
	var walk func(uid, indent string)
	walk = func(uid, indent string) {
		kids := children[uid]
		for i, n := range kids {
			branch, next := "├─ ", "│  "
			if i == len(kids)-1 {
				branch, next = "└─ ", "   "
			}
			h := ""
			if n.Health != nil {
				h = " " + string(n.Health.Status)
			}
			var info []string
			for _, it := range n.Info {
				if it.Name != "ID" {
					info = append(info, it.Name+"="+it.Value)
				}
			}
			fmt.Fprintf(w, "%s%s%s %s%s  %s\n", indent, branch, n.Kind, n.Name, h, strings.Join(info, " "))
			walk(n.UID, indent+next)
		}
	}
	walk("", "")
}

// ─── app create / set ───────────────────────────────────────────────────

// appFlags are the spec fields settable from flags.
type appFlags struct {
	repo, path, revision, project              string
	destName, destNamespace, destRegion        string
	syncPolicy                                 string
	autoPrune, selfHeal                        bool
	syncOptions, labels, vars, varFiles, files stringList
	consulKV                                   string
	values, selector, unit, nodeSelector       string
	replicas                                   int
}

func (f *appFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.repo, "repo", "", "")
	fs.StringVar(&f.path, "path", "", "")
	fs.StringVar(&f.revision, "revision", "", "")
	fs.StringVar(&f.project, "project", "", "")
	fs.StringVar(&f.destName, "dest-name", "", "")
	fs.StringVar(&f.destNamespace, "dest-namespace", "", "")
	fs.StringVar(&f.destRegion, "dest-region", "", "")
	fs.StringVar(&f.syncPolicy, "sync-policy", "", "")
	fs.BoolVar(&f.autoPrune, "auto-prune", false, "")
	fs.BoolVar(&f.selfHeal, "self-heal", false, "")
	fs.Var(&f.syncOptions, "sync-option", "")
	fs.Var(&f.labels, "label", "")
	fs.Var(&f.vars, "var", "")
	fs.Var(&f.varFiles, "var-file", "")
	fs.Var(&f.files, "job-file", "")
	fs.StringVar(&f.consulKV, "consul-kv", "", "")
	fs.StringVar(&f.values, "systemd-values", "", "")
	fs.StringVar(&f.selector, "selector", "", "")
	fs.StringVar(&f.unit, "unit", "", "")
	fs.StringVar(&f.nodeSelector, "node-selector", "", "")
	fs.IntVar(&f.replicas, "replicas", -1, "")
}

const appFlagsHelp = `
Spec options:

  -repo=<url>              Git repository.
  -path=<dir>              Directory in the repository.
  -revision=<ref>          Branch, tag or commit (default HEAD).
  -project=<name>          Project (default "default").
  -dest-name=<cluster>     Destination cluster (default in-cluster).
  -dest-namespace=<ns>     Nomad namespace (default "default").
  -dest-region=<region>    Nomad region.
  -sync-policy=<policy>    "automated" or "none".
  -auto-prune              With automated sync: delete what Git removed.
  -self-heal               With automated sync: revert changes made outside Git.
  -sync-option=<opt>       CreateNamespace=true, PruneLast=true,
                           ApplyOutOfSyncOnly=true, PreserveCounts=true
                           (repeatable).
  -label=<k=v>             Label (repeatable).

  Nomad source:
  -job-file=<glob>         Jobspec files (repeatable; default *.nomad.hcl...).
  -var-file=<path>         HCL2 variable file (repeatable, later wins).
  -var=<name=value>        Variable (repeatable).
  -consul-kv=<path>        File listing the Consul keys the application owns.

  systemd source (a v4-style values file):
  -systemd-values=<path>   Values file; selects the systemd source.
  -selector=<name>         Replaces ${selector} in the value keys.
  -unit=<name>             systemd unit (default: the application name).
  -node-selector=<pool>    Run on nodes with meta cystemd_selector_<pool>=true.
  -replicas=<n>            Instances (0: every eligible node).
`

// apply writes the flags that were set onto app.
func (f *appFlags) apply(app *v1.Application, set map[string]bool) error {
	src := &app.Spec.Source
	if set["repo"] {
		src.RepoURL = f.repo
	}
	if set["path"] {
		src.Path = f.path
	}
	if set["revision"] {
		src.TargetRevision = f.revision
	}
	if set["project"] {
		app.Spec.Project = f.project
	}
	if set["dest-name"] {
		app.Spec.Destination.Name = f.destName
	}
	if set["dest-namespace"] {
		app.Spec.Destination.Namespace = f.destNamespace
	}
	if set["dest-region"] {
		app.Spec.Destination.Region = f.destRegion
	}
	if set["sync-policy"] || set["auto-prune"] || set["self-heal"] || set["sync-option"] {
		if app.Spec.SyncPolicy == nil {
			app.Spec.SyncPolicy = &v1.SyncPolicy{}
		}
		p := app.Spec.SyncPolicy
		switch f.syncPolicy {
		case "automated", "auto", "automatic":
			if p.Automated == nil {
				p.Automated = &v1.SyncPolicyAutomated{}
			}
		case "none", "manual":
			p.Automated = nil
		case "":
		default:
			return fmt.Errorf("-sync-policy %q: automated or none", f.syncPolicy)
		}
		if (set["auto-prune"] || set["self-heal"]) && p.Automated == nil {
			return errors.New("-auto-prune and -self-heal need -sync-policy=automated")
		}
		if set["auto-prune"] {
			p.Automated.Prune = f.autoPrune
		}
		if set["self-heal"] {
			p.Automated.SelfHeal = f.selfHeal
		}
		if set["sync-option"] {
			p.SyncOptions = append([]string(nil), f.syncOptions...)
		}
	}
	if set["label"] {
		labels, err := keyValues(f.labels, "-label")
		if err != nil {
			return err
		}
		app.Metadata.Labels = labels
	}
	if set["systemd-values"] || set["selector"] || set["unit"] || set["node-selector"] || set["replicas"] {
		src.Nomad = nil
		if src.Systemd == nil {
			src.Systemd = &v1.SystemdSource{}
		}
		s := src.Systemd
		if set["systemd-values"] {
			s.ValuesFile = f.values
		}
		if set["selector"] {
			s.Selector = f.selector
		}
		if set["unit"] {
			s.Unit = f.unit
		}
		if set["node-selector"] {
			s.NodeSelector = f.nodeSelector
		}
		if set["replicas"] {
			r := f.replicas
			s.Replicas = &r
		}
	}
	if set["var"] || set["var-file"] || set["job-file"] || set["consul-kv"] {
		if src.Systemd != nil {
			return errors.New("Nomad source options cannot be combined with a systemd source")
		}
		if src.Nomad == nil {
			src.Nomad = &v1.NomadSource{}
		}
		if set["var"] {
			vars, err := keyValues(f.vars, "-var")
			if err != nil {
				return err
			}
			src.Nomad.Vars = vars
		}
		if set["var-file"] {
			src.Nomad.VarFiles = append([]string(nil), f.varFiles...)
		}
		if set["job-file"] {
			src.Nomad.Files = append([]string(nil), f.files...)
		}
		if set["consul-kv"] {
			src.Nomad.ConsulKV = f.consulKV
		}
	}
	return nil
}

func visited(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

type appCreateCommand struct{ *meta }

func (c *appCreateCommand) Synopsis() string { return "Create applications (from flags or manifests)" }
func (c *appCreateCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` app create [options] <name>
       ` + c.opts.Name + ` app create -f <manifest.yaml> [-upsert]

  Creates an application from flags, or the applications (and projects)
  of a manifest file ("-" reads standard input).

Options:

  -f=<file>     Manifest file (YAML or JSON, several documents).
  -upsert       Replace the spec of applications that exist.
` + appFlagsHelp + c.connectionHelp()
}

func (c *appCreateCommand) Run(args []string) int {
	var file string
	var upsert bool
	var af appFlags
	fs := c.flagSet("app create", true)
	fs.StringVar(&file, "f", "", "")
	fs.BoolVar(&upsert, "upsert", false, "")
	af.register(fs)
	rest, ok := c.parse(fs, args, -1, c.opts.Name+" app create [options] <name>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	if file != "" {
		if len(rest) > 0 {
			c.ui.Error("Give either -f or a name, not both.")
			return 1
		}
		objs, err := manifest.ReadFile(file)
		if err != nil {
			return c.fail(err)
		}
		// Projects first: applications may use them. One that exists is
		// kept unless -upsert, so the same file can be applied again.
		for _, o := range objs {
			if o.Project == nil {
				continue
			}
			result, err := createProject(ctx, cl, o.Project, upsert)
			if errors.Is(err, errExists) {
				c.ui.Warn(fmt.Sprintf("project '%s' already exists: unchanged (-upsert replaces it)", o.Project.Metadata.Name))
				continue
			}
			if err != nil {
				return c.fail(fmt.Errorf("project %s: %w", o.Project.Metadata.Name, err))
			}
			c.ui.Output(fmt.Sprintf("project '%s' %s", o.Project.Metadata.Name, result))
		}
		for _, o := range objs {
			if o.Application == nil {
				continue
			}
			if _, err := cl.CreateApplication(ctx, o.Application, upsert); err != nil {
				return c.fail(fmt.Errorf("application %s: %w", o.Application.Metadata.Name, err))
			}
			c.ui.Output(fmt.Sprintf("application '%s' %s", o.Application.Metadata.Name, createdOrUpserted(upsert)))
		}
		return 0
	}
	if len(rest) != 1 {
		c.ui.Error("Usage: " + c.opts.Name + " app create [options] <name>")
		return 1
	}
	app := &v1.Application{Metadata: v1.ObjectMeta{Name: rest[0]}}
	if err := af.apply(app, visited(fs)); err != nil {
		return c.fail(err)
	}
	if _, err := cl.CreateApplication(ctx, app, upsert); err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("application '%s' %s", app.Metadata.Name, createdOrUpserted(upsert)))
	return 0
}

func createdOrUpserted(upsert bool) string {
	if upsert {
		return "created or updated"
	}
	return "created"
}

type appSetCommand struct{ *meta }

func (c *appSetCommand) Synopsis() string { return "Change an application's spec" }
func (c *appSetCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` app set [options] <name>

  Changes the given fields of an application's spec; others stay.
` + appFlagsHelp + c.connectionHelp()
}

func (c *appSetCommand) Run(args []string) int {
	var af appFlags
	fs := c.flagSet("app set", true)
	af.register(fs)
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" app set [options] <name>")
	if !ok {
		return 1
	}
	set := visited(fs)
	for _, conn := range []string{"address", "token", "ca-cert", "tls-skip-verify", "context"} {
		delete(set, conn)
	}
	if len(set) == 0 {
		c.ui.Error("Nothing to change: give at least one spec option.")
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	app, err := cl.GetApplication(ctx, rest[0], "")
	if err != nil {
		return c.fail(err)
	}
	if err := af.apply(app, set); err != nil {
		return c.fail(err)
	}
	if _, err := cl.UpdateApplication(ctx, app); err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("application '%s' updated", app.Metadata.Name))
	return 0
}

// ─── app delete ─────────────────────────────────────────────────────────

type appDeleteCommand struct{ *meta }

func (c *appDeleteCommand) Synopsis() string { return "Delete an application" }
func (c *appDeleteCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` app delete [options] <name>

  Deletes an application. By default its Nomad jobs and Consul keys are
  deleted first (cascade).

Options:

  -cascade=false    Delete only the application; jobs and keys keep running.
  -y                Do not ask for confirmation.
  -wait             Wait until the deletion completes.
` + c.connectionHelp()
}

func (c *appDeleteCommand) Run(args []string) int {
	cascade, yes, wait := true, false, false
	fs := c.flagSet("app delete", true)
	fs.BoolVar(&cascade, "cascade", true, "")
	fs.BoolVar(&yes, "y", false, "")
	fs.BoolVar(&wait, "wait", false, "")
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" app delete [options] <name>")
	if !ok {
		return 1
	}
	name := rest[0]
	if !yes {
		what := "and its jobs and Consul keys"
		if !cascade {
			what = "(its jobs and keys keep running)"
		}
		answer, err := c.ui.Ask(fmt.Sprintf("Delete application '%s' %s? Type the name to confirm:", name, what))
		if err != nil {
			return c.fail(err)
		}
		if strings.TrimSpace(answer) != name {
			c.ui.Error("Not confirmed.")
			return 1
		}
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	if err := cl.DeleteApplication(ctx, name, cascade); err != nil {
		return c.fail(err)
	}
	if wait && cascade {
		for {
			if _, err := cl.GetApplication(ctx, name, ""); client.IsNotFound(err) {
				break
			} else if err != nil {
				return c.fail(err)
			}
			select {
			case <-ctx.Done():
				return c.fail(ctx.Err())
			case <-time.After(time.Second):
			}
		}
	}
	c.ui.Output(fmt.Sprintf("application '%s' deleted", name))
	return 0
}

// ─── sync, rollback, wait ───────────────────────────────────────────────

// waitFor polls the application until done reports true.
func waitFor(ctx context.Context, cl *client.Client, name string, timeout time.Duration, done func(*v1.Application) bool) (*v1.Application, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	for {
		app, err := cl.GetApplication(ctx, name, "")
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("timed out waiting for %s", name)
			}
			return nil, err
		}
		if done(app) {
			return app, nil
		}
		select {
		case <-ctx.Done():
			return app, fmt.Errorf("timed out waiting for %s (%s, %s)", name, syncStatus(app), health(app))
		case <-time.After(time.Second):
		}
	}
}

// waitOperation waits for the operation requested at since to finish and
// prints its result; it returns the exit code.
func (m *meta) waitOperation(ctx context.Context, cl *client.Client, name string, since time.Time, timeout time.Duration) int {
	app, err := waitFor(ctx, cl, name, timeout, func(a *v1.Application) bool {
		st := a.Status.OperationState
		return a.Operation == nil && st != nil && !st.StartedAt.Before(since.Add(-2*time.Second)) && st.Phase.Completed()
	})
	if err != nil {
		return m.fail(err)
	}
	st := app.Status.OperationState
	if st.SyncResult != nil && len(st.SyncResult.Resources) > 0 {
		t := newTable(m.out, "KIND", "NAMESPACE", "NAME", "RESULT", "MESSAGE")
		for _, r := range st.SyncResult.Resources {
			t.row(r.Kind, orDash(r.Namespace), r.Name, string(r.Status), r.Message)
		}
		t.flush()
		fmt.Fprintln(m.out)
	}
	t := newTable(m.out)
	t.row("Operation:", operationName(st))
	if st.SyncResult != nil {
		t.row("Revision:", st.SyncResult.Revision)
	}
	t.row("Phase:", string(st.Phase))
	t.row("Message:", st.Message)
	if st.FinishedAt != nil {
		t.row("Duration:", st.FinishedAt.Sub(st.StartedAt).Round(time.Second).String())
	}
	t.flush()
	if st.Phase != v1.OperationSucceeded {
		return 1
	}
	return 0
}

// parseResource reads KIND:NAMESPACE:NAME (namespace may be empty).
func parseResource(s string) (v1.SyncOperationResource, error) {
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 2:
		return v1.SyncOperationResource{Kind: parts[0], Name: parts[1]}, nil
	case 3:
		return v1.SyncOperationResource{Kind: parts[0], Namespace: parts[1], Name: parts[2]}, nil
	}
	return v1.SyncOperationResource{}, fmt.Errorf("-resource %q: expected KIND:NAMESPACE:NAME", s)
}

type appSyncCommand struct{ *meta }

func (c *appSyncCommand) Synopsis() string { return "Sync applications to their target state" }
func (c *appSyncCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` app sync [options] <name>...

  Syncs applications with Git and waits for the result (unless -async).

Options:

  -revision=<ref>          Sync once to another branch, tag or commit.
  -prune                   Delete jobs and keys no longer in Git.
  -dry-run                 Plan only.
  -resource=<K:NS:NAME>    Sync only this resource (repeatable), e.g.
                           Job:default:web or ConsulKV::config/web/flags.
  -sync-option=<opt>       Sync option (repeatable), e.g. PruneLast=true.
  -retry-limit=<n>         Retry a failed sync up to n times.
  -async                   Do not wait for the result.
  -timeout=<duration>      How long to wait (default 10m).
` + c.connectionHelp()
}

func (c *appSyncCommand) Run(args []string) int {
	var revision string
	var prune, dryRun, async bool
	var resources, options stringList
	var retry int
	var timeout time.Duration
	fs := c.flagSet("app sync", true)
	fs.StringVar(&revision, "revision", "", "")
	fs.BoolVar(&prune, "prune", false, "")
	fs.BoolVar(&dryRun, "dry-run", false, "")
	fs.Var(&resources, "resource", "")
	fs.Var(&options, "sync-option", "")
	fs.IntVar(&retry, "retry-limit", 0, "")
	fs.BoolVar(&async, "async", false, "")
	fs.DurationVar(&timeout, "timeout", 10*time.Minute, "")
	names, ok := c.parse(fs, args, -1, c.opts.Name+" app sync [options] <name>...")
	if !ok {
		return 1
	}
	if len(names) == 0 {
		c.ui.Error("Usage: " + c.opts.Name + " app sync [options] <name>...")
		return 1
	}
	req := client.SyncRequest{Revision: revision, Prune: prune, DryRun: dryRun, SyncOptions: options}
	for _, r := range resources {
		res, err := parseResource(r)
		if err != nil {
			return c.fail(err)
		}
		req.Resources = append(req.Resources, res)
	}
	if retry > 0 {
		req.Retry = &v1.RetryStrategy{Limit: int64(retry)}
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	code := 0
	for _, name := range names {
		since := time.Now()
		if _, err := cl.Sync(ctx, name, req); err != nil {
			code = c.fail(fmt.Errorf("%s: %w", name, err))
			continue
		}
		if async {
			c.ui.Output(fmt.Sprintf("sync of '%s' requested", name))
			continue
		}
		if len(names) > 1 {
			c.ui.Output("== " + name)
		}
		if rc := c.waitOperation(ctx, cl, name, since, timeout); rc != 0 {
			code = rc
		}
	}
	return code
}

type appRollbackCommand struct{ *meta }

func (c *appRollbackCommand) Synopsis() string { return "Roll back to a previous deployment" }
func (c *appRollbackCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` app rollback [options] <name> <history-id>

  Reverts the application's jobs to the versions a past sync produced
  (see 'app history'). Not available while auto-sync is enabled.

Options:

  -prune                Delete jobs that did not exist then.
  -dry-run              Plan only.
  -async                Do not wait for the result.
  -timeout=<duration>   How long to wait (default 10m).
` + c.connectionHelp()
}

func (c *appRollbackCommand) Run(args []string) int {
	var prune, dryRun, async bool
	var timeout time.Duration
	fs := c.flagSet("app rollback", true)
	fs.BoolVar(&prune, "prune", false, "")
	fs.BoolVar(&dryRun, "dry-run", false, "")
	fs.BoolVar(&async, "async", false, "")
	fs.DurationVar(&timeout, "timeout", 10*time.Minute, "")
	rest, ok := c.parse(fs, args, 2, c.opts.Name+" app rollback [options] <name> <history-id>")
	if !ok {
		return 1
	}
	id, err := strconv.ParseInt(rest[1], 10, 64)
	if err != nil {
		return c.fail(fmt.Errorf("history id %q: %w", rest[1], err))
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	since := time.Now()
	if _, err := cl.Rollback(ctx, rest[0], id, prune, dryRun); err != nil {
		return c.fail(err)
	}
	if async {
		c.ui.Output(fmt.Sprintf("rollback of '%s' to %d requested", rest[0], id))
		return 0
	}
	return c.waitOperation(ctx, cl, rest[0], since, timeout)
}

type appWaitCommand struct{ *meta }

func (c *appWaitCommand) Synopsis() string { return "Wait for applications to be synced and healthy" }
func (c *appWaitCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` app wait [options] <name>...

  Waits until the applications are synced, healthy and have no operation
  running (or only the conditions given).

Options:

  -sync                 Wait for Synced.
  -health               Wait for Healthy.
  -operation            Wait for no running operation.
  -suspended            Wait for Suspended (instead of Healthy).
  -degraded             Wait for Degraded (instead of Healthy).
  -timeout=<duration>   How long to wait (default 10m, 0 for ever).
` + c.connectionHelp()
}

func (c *appWaitCommand) Run(args []string) int {
	var wantSync, wantHealth, wantOp, suspended, degraded bool
	var timeout time.Duration
	fs := c.flagSet("app wait", true)
	fs.BoolVar(&wantSync, "sync", false, "")
	fs.BoolVar(&wantHealth, "health", false, "")
	fs.BoolVar(&wantOp, "operation", false, "")
	fs.BoolVar(&suspended, "suspended", false, "")
	fs.BoolVar(&degraded, "degraded", false, "")
	fs.DurationVar(&timeout, "timeout", 10*time.Minute, "")
	names, ok := c.parse(fs, args, -1, c.opts.Name+" app wait [options] <name>...")
	if !ok || len(names) == 0 {
		if ok {
			c.ui.Error("Usage: " + c.opts.Name + " app wait [options] <name>...")
		}
		return 1
	}
	if !wantSync && !wantHealth && !wantOp && !suspended && !degraded {
		wantSync, wantHealth, wantOp = true, true, true
	}
	wantHealthCode := v1.HealthStatusHealthy
	switch {
	case suspended:
		wantHealthCode, wantHealth = v1.HealthStatusSuspended, true
	case degraded:
		wantHealthCode, wantHealth = v1.HealthStatusDegraded, true
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	for _, name := range names {
		app, err := waitFor(ctx, cl, name, timeout, func(a *v1.Application) bool {
			running := a.Operation != nil || (a.Status.OperationState != nil && !a.Status.OperationState.Phase.Completed())
			return (!wantSync || a.Status.Sync.Status == v1.SyncStatusSynced) &&
				(!wantHealth || a.Status.Health.Status == wantHealthCode) &&
				(!wantOp || !running)
		})
		if err != nil {
			return c.fail(err)
		}
		c.ui.Output(fmt.Sprintf("%s: %s, %s", name, syncStatus(app), health(app)))
	}
	return 0
}

type appTerminateCommand struct{ *meta }

func (c *appTerminateCommand) Synopsis() string { return "Stop an application's running operation" }
func (c *appTerminateCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` app terminate-op <name>
` + c.connectionHelp()
}

func (c *appTerminateCommand) Run(args []string) int {
	fs := c.flagSet("app terminate-op", true)
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" app terminate-op <name>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	if err := cl.TerminateOperation(ctx, rest[0]); err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("operation of '%s' is terminating", rest[0]))
	return 0
}

// ─── diff, history, manifests, resources ────────────────────────────────

type appDiffCommand struct{ *meta }

func (c *appDiffCommand) Synopsis() string {
	return "Show the differences between Git and the live state"
}
func (c *appDiffCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` app diff [options] <name>

  Prints, for each resource that differs, a unified diff from the live
  state to the desired state in Git, and Nomad's plan. Exits with 1 when
  there are differences, 0 when there are none (like diff(1)).

Options:

  -exit-code=false   Exit 0 even when there are differences.
` + c.connectionHelp()
}

func (c *appDiffCommand) Run(args []string) int {
	exitCode := true
	fs := c.flagSet("app diff", true)
	fs.BoolVar(&exitCode, "exit-code", true, "")
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" app diff [options] <name>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	diffs, err := cl.ManagedResources(ctx, rest[0])
	if err != nil {
		return c.fail(err)
	}
	changed := 0
	for _, d := range diffs {
		if !d.Modified {
			continue
		}
		changed++
		id := d.Kind + " " + d.Name
		if d.Namespace != "" {
			id = d.Kind + " " + d.Namespace + "/" + d.Name
		}
		target := d.TargetState
		if d.RequiresPruning {
			target = ""
			id += " (requires pruning)"
		}
		fmt.Fprintf(c.out, "===== %s =====\n", id)
		if u := diff.Unified("live", "desired", d.NormalizedLiveState, target, 3); u != "" {
			fmt.Fprint(c.out, u)
		}
		if d.PlanDiff != "" {
			fmt.Fprintln(c.out, "----- nomad plan -----")
			fmt.Fprintln(c.out, strings.TrimRight(d.PlanDiff, "\n"))
		}
		fmt.Fprintln(c.out)
	}
	if changed == 0 {
		c.ui.Output("No differences.")
		return 0
	}
	if exitCode {
		return 1
	}
	return 0
}

type appHistoryCommand struct{ *meta }

func (c *appHistoryCommand) Synopsis() string { return "List an application's deployments" }
func (c *appHistoryCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` app history [options] <name>

Options:

  -o=<format>   json or yaml.
` + c.connectionHelp()
}

func (c *appHistoryCommand) Run(args []string) int {
	var out string
	fs := c.flagSet("app history", true)
	fs.StringVar(&out, "o", "", "")
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" app history [options] <name>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	app, err := cl.GetApplication(ctx, rest[0], "")
	if err != nil {
		return c.fail(err)
	}
	if done, err := output(c.out, out, app.Status.History); done {
		if err != nil {
			return c.fail(err)
		}
		return 0
	}
	t := newTable(c.out, "ID", "DATE", "REVISION", "INITIATED BY", "JOB VERSIONS")
	for i := len(app.Status.History) - 1; i >= 0; i-- {
		h := app.Status.History[i]
		by := h.InitiatedBy.Username
		if h.InitiatedBy.Automated {
			by = "automated"
		}
		var versions []string
		for _, k := range sortedVersionKeys(h.JobVersions) {
			versions = append(versions, fmt.Sprintf("%s@%d", k, h.JobVersions[k]))
		}
		t.row(strconv.FormatInt(h.ID, 10), h.DeployedAt.Local().Format("2006-01-02 15:04:05"),
			h.Source.Revision()+" ("+shortRev(h.Revision)+")", orDash(by), strings.Join(versions, " "))
	}
	t.flush()
	return 0
}

func sortedVersionKeys(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type appManifestsCommand struct{ *meta }

func (c *appManifestsCommand) Synopsis() string { return "Print an application's rendered manifests" }
func (c *appManifestsCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` app manifests [options] <name>

  Prints the jobs and Consul keys the application's source renders to.

Options:

  -revision=<ref>   Render another revision.
` + c.connectionHelp()
}

func (c *appManifestsCommand) Run(args []string) int {
	var revision string
	fs := c.flagSet("app manifests", true)
	fs.StringVar(&revision, "revision", "", "")
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" app manifests [options] <name>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	m, err := cl.Manifests(ctx, rest[0], revision)
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprintf(c.out, "# %s source at %s\n", m.SourceType, m.Revision)
	for i, doc := range m.Manifests {
		if i > 0 {
			fmt.Fprintln(c.out, "---")
		}
		fmt.Fprintln(c.out, strings.TrimRight(doc, "\n"))
	}
	return 0
}

type appResourcesCommand struct{ *meta }

func (c *appResourcesCommand) Synopsis() string { return "List an application's managed resources" }
func (c *appResourcesCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` app resources [options] <name>

Options:

  -o=<format>   json, yaml or tree (with the live children).
` + c.connectionHelp()
}

func (c *appResourcesCommand) Run(args []string) int {
	var out string
	fs := c.flagSet("app resources", true)
	fs.StringVar(&out, "o", "", "")
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" app resources [options] <name>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	app, err := cl.GetApplication(ctx, rest[0], "")
	if err != nil {
		return c.fail(err)
	}
	if out == "tree" {
		tree, err := cl.ResourceTree(ctx, rest[0])
		if err != nil {
			return c.fail(err)
		}
		printTree(c.out, app, tree)
		return 0
	}
	if done, err := output(c.out, out, app.Status.Resources); done {
		if err != nil {
			return c.fail(err)
		}
		return 0
	}
	printResources(c.out, app.Status.Resources)
	return 0
}

// ─── logs, actions ──────────────────────────────────────────────────────

type appLogsCommand struct{ *meta }

func (c *appLogsCommand) Synopsis() string { return "Print the logs of an application's tasks" }
func (c *appLogsCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` app logs [options] <name>

  Prints the logs of the application's allocations; with several, lines
  are prefixed with the allocation and task.

Options:

  -job=<id>          Only this job's allocations.
  -group=<name>      Only this task group's allocations.
  -alloc=<id>        Only this allocation (ID prefix or name).
  -task=<name>       The task (default: the first one).
  -stderr            stderr instead of stdout.
  -f                 Follow the logs.
  -tail=<n>          Start with the last n lines (default 100).
` + c.connectionHelp()
}

func (c *appLogsCommand) Run(args []string) int {
	var job, group, alloc, task string
	var stderr, follow bool
	var tail int
	fs := c.flagSet("app logs", true)
	fs.StringVar(&job, "job", "", "")
	fs.StringVar(&group, "group", "", "")
	fs.StringVar(&alloc, "alloc", "", "")
	fs.StringVar(&task, "task", "", "")
	fs.BoolVar(&stderr, "stderr", false, "")
	fs.BoolVar(&follow, "f", false, "")
	fs.BoolVar(&follow, "follow", false, "")
	fs.IntVar(&tail, "tail", 100, "")
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" app logs [options] <name>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	tree, err := cl.ResourceTree(ctx, rest[0])
	if err != nil {
		return c.fail(err)
	}
	type target struct{ id, label string }
	var targets []target
	for _, n := range tree.Nodes {
		if n.Kind != v1.KindAllocation || n.InfoValue("Status") != "running" && alloc == "" {
			continue
		}
		id := strings.TrimPrefix(n.UID, "alloc:")
		if alloc != "" && !strings.HasPrefix(id, alloc) && n.Name != alloc {
			continue
		}
		jobGroup := strings.SplitN(n.Name, "[", 2)[0] // job.group
		if job != "" && !strings.HasPrefix(jobGroup, job+".") {
			continue
		}
		if group != "" && !strings.HasSuffix(jobGroup, "."+group) {
			continue
		}
		targets = append(targets, target{id: id, label: n.Name})
	}
	if len(targets) == 0 {
		return c.fail(errors.New("no matching running allocations"))
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make(chan error, len(targets))
	for _, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := cl.Logs(ctx, rest[0], t.id, client.LogOptions{Task: task, Stderr: stderr, Follow: follow, TailLines: tail}, func(l client.LogLine) error {
				mu.Lock()
				defer mu.Unlock()
				if len(targets) > 1 {
					fmt.Fprintf(c.out, "[%s/%s] %s\n", t.label, l.Task, l.Content)
				} else {
					fmt.Fprintln(c.out, l.Content)
				}
				return nil
			})
			if err != nil && ctx.Err() == nil {
				errs <- fmt.Errorf("%s: %w", t.label, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	code := 0
	for err := range errs {
		code = c.fail(err)
	}
	return code
}

type appActionsCommand struct{ *meta }

func (c *appActionsCommand) Synopsis() string {
	return "Run a resource action (restart, stop, promote...)"
}
func (c *appActionsCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` app actions [options] <name> <action> <kind>/<resource>

  Runs an action on one of the application's resources:

    Job          restart (rolling), stop, start
    Allocation   restart, stop
    Task         restart, signal-hup  (resource: <allocation>/<task>)
    Deployment   promote, pause, resume, fail

  For example: app actions web restart Job/web
               app actions web stop Allocation/web.web[1]
` + c.connectionHelp()
}

func (c *appActionsCommand) Run(args []string) int {
	fs := c.flagSet("app actions", true)
	rest, ok := c.parse(fs, args, 3, c.opts.Name+" app actions [options] <name> <action> <kind>/<resource>")
	if !ok {
		return 1
	}
	kind, res, found := strings.Cut(rest[2], "/")
	if !found {
		return c.fail(fmt.Errorf("%q: expected <kind>/<resource>", rest[2]))
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	tree, err := cl.ResourceTree(ctx, rest[0])
	if err != nil {
		return c.fail(err)
	}
	var node *client.ResourceNode
	for i, n := range tree.Nodes {
		if !strings.EqualFold(n.Kind, kind) {
			continue
		}
		name := n.Name
		if n.Kind == v1.KindTask && len(n.ParentRefs) > 0 {
			name = parentName(tree, n.ParentRefs[0].UID) + "/" + n.Name
		}
		if name == res || strings.TrimPrefix(n.UID, strings.ToLower(n.Kind)+":") == res {
			node = &tree.Nodes[i]
			break
		}
	}
	if node == nil {
		return c.fail(fmt.Errorf("no %s %q in %s", kind, res, rest[0]))
	}
	if err := cl.RunAction(ctx, rest[0], node.ResourceRef, rest[1]); err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("%s: %s %s", rest[1], node.Kind, node.Name))
	return 0
}

func parentName(tree *client.ResourceTree, uid string) string {
	for _, n := range tree.Nodes {
		if n.UID == uid {
			return n.Name
		}
	}
	return ""
}
