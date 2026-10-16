// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
	"github.com/gitaginanjar/cystemd/pkg/client"
)

// editProject applies edit to a project and saves it, retrying when the
// project changed in between.
func editProject(ctx context.Context, cl *client.Client, name string, edit func(*v1.AppProject) error) error {
	for attempt := 0; ; attempt++ {
		p, err := cl.GetProject(ctx, name)
		if err != nil {
			return err
		}
		if err := edit(p); err != nil {
			return err
		}
		err = cl.UpdateProject(ctx, p)
		var ce *client.Error
		if errors.As(err, &ce) && ce.Code == 409 && attempt < 3 {
			continue
		}
		return err
	}
}

func windowID(p *v1.AppProject, arg string) (int, error) {
	id, err := strconv.Atoi(arg)
	if err != nil || id < 0 || id >= len(p.Spec.SyncWindows) {
		return 0, fmt.Errorf("project %s has no sync window %q (see proj windows list)", p.Metadata.Name, arg)
	}
	return id, nil
}

func listFlag(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

type projWindowsListCommand struct{ *meta }

func (c *projWindowsListCommand) Synopsis() string { return "List a project's sync windows" }
func (c *projWindowsListCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj windows list [options] <project>

  Lists the sync windows of a project, with their IDs and whether they are
  active now.

Options:

  -o=<format>   wide (default), json or yaml.
` + c.connectionHelp()
}

func (c *projWindowsListCommand) Run(args []string) int {
	out := "wide"
	fs := c.flagSet("proj windows list", true)
	fs.StringVar(&out, "o", "wide", "")
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" proj windows list [options] <project>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	p, err := cl.GetProject(ctx, rest[0])
	if err != nil {
		return c.fail(err)
	}
	if done, err := output(c.out, out, p.Spec.SyncWindows); done {
		if err != nil {
			return c.fail(err)
		}
		return 0
	}
	t := newTable(c.out, "ID", "STATUS", "KIND", "SCHEDULE", "DURATION", "TIMEZONE", "APPLICATIONS", "NAMESPACES", "CLUSTERS", "MANUALSYNC")
	now := time.Now()
	for i, w := range p.Spec.SyncWindows {
		status := "Inactive"
		if w.Active(now) {
			status = "Active"
		}
		manual := "Disabled"
		if w.ManualSync {
			manual = "Enabled"
		}
		t.row(strconv.Itoa(i), status, w.Kind, w.Schedule, w.Duration, orDash(w.TimeZone), orDash(strings.Join(w.Applications, ",")),
			orDash(strings.Join(w.Namespaces, ",")), orDash(strings.Join(w.Clusters, ",")), manual)
	}
	t.flush()
	return 0
}

type projWindowsAddCommand struct{ *meta }

func (c *projWindowsAddCommand) Synopsis() string { return "Add a sync window to a project" }
func (c *projWindowsAddCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj windows add [options] <project>

  Adds a sync window. From each time its schedule fires, for its duration,
  an allow window lets syncs of the applications it matches through (and
  outside its allow windows, an application does not sync); a deny window
  stops them.

Options:

  -kind=<allow|deny>      Required.
  -schedule=<cron>        When it starts, as a cron expression ("0 22 * * *").
  -duration=<duration>    How long it lasts ("1h").
  -applications=<globs>   Applications it matches, comma-separated.
  -namespaces=<globs>     Destination namespaces it matches.
  -clusters=<globs>       Destination clusters it matches.
  -and-operator           Match all of the above rather than any.
  -manual-sync            Let manual syncs through anyway.
  -time-zone=<zone>       Time zone of the schedule (default UTC).
  -description=<text>     What the window is for.
` + c.connectionHelp()
}

func (c *projWindowsAddCommand) Run(args []string) int {
	var w v1.SyncWindow
	var apps, namespaces, clusters string
	fs := c.flagSet("proj windows add", true)
	fs.StringVar(&w.Kind, "kind", "", "")
	fs.StringVar(&w.Schedule, "schedule", "", "")
	fs.StringVar(&w.Duration, "duration", "", "")
	fs.StringVar(&apps, "applications", "", "")
	fs.StringVar(&namespaces, "namespaces", "", "")
	fs.StringVar(&clusters, "clusters", "", "")
	fs.BoolVar(&w.AndOperator, "and-operator", false, "")
	fs.BoolVar(&w.ManualSync, "manual-sync", false, "")
	fs.StringVar(&w.TimeZone, "time-zone", "", "")
	fs.StringVar(&w.Description, "description", "", "")
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" proj windows add [options] <project>")
	if !ok {
		return 1
	}
	w.Applications, w.Namespaces, w.Clusters = listFlag(apps), listFlag(namespaces), listFlag(clusters)
	if err := w.Validate(); err != nil {
		return c.fail(err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	id := 0
	err = editProject(ctx, cl, rest[0], func(p *v1.AppProject) error {
		p.Spec.SyncWindows = append(p.Spec.SyncWindows, &w)
		id = len(p.Spec.SyncWindows) - 1
		return nil
	})
	if err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("Sync window %d added to project %s.", id, rest[0]))
	return 0
}

type projWindowsDeleteCommand struct{ *meta }

func (c *projWindowsDeleteCommand) Synopsis() string { return "Delete a sync window of a project" }
func (c *projWindowsDeleteCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj windows delete <project> <id>

  Deletes a sync window, by the ID proj windows list shows.
` + c.connectionHelp()
}

func (c *projWindowsDeleteCommand) Run(args []string) int {
	fs := c.flagSet("proj windows delete", true)
	rest, ok := c.parse(fs, args, 2, c.opts.Name+" proj windows delete <project> <id>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	err = editProject(ctx, cl, rest[0], func(p *v1.AppProject) error {
		id, err := windowID(p, rest[1])
		if err != nil {
			return err
		}
		p.Spec.SyncWindows = append(p.Spec.SyncWindows[:id:id], p.Spec.SyncWindows[id+1:]...)
		return nil
	})
	if err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("Sync window %s deleted from project %s.", rest[1], rest[0]))
	return 0
}

// projWindowsManualCommand enables or disables a window's manual syncs.
type projWindowsManualCommand struct {
	*meta
	enable bool
}

func (c *projWindowsManualCommand) name() string {
	if c.enable {
		return "proj windows enable-manual-sync"
	}
	return "proj windows disable-manual-sync"
}

func (c *projWindowsManualCommand) Synopsis() string {
	if c.enable {
		return "Let manual syncs through a sync window"
	}
	return "Stop manual syncs at a sync window"
}

func (c *projWindowsManualCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` ` + c.name() + ` <project> <id>
` + c.connectionHelp()
}

func (c *projWindowsManualCommand) Run(args []string) int {
	fs := c.flagSet(c.name(), true)
	rest, ok := c.parse(fs, args, 2, c.opts.Name+" "+c.name()+" <project> <id>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	err = editProject(ctx, cl, rest[0], func(p *v1.AppProject) error {
		id, err := windowID(p, rest[1])
		if err != nil {
			return err
		}
		p.Spec.SyncWindows[id].ManualSync = c.enable
		return nil
	})
	if err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("Sync window %s of project %s updated.", rest[1], rest[0]))
	return 0
}
