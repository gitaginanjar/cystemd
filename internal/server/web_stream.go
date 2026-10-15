// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Live updates: the UI listens to a server-sent-events stream that only
// says "something changed"; the page then re-requests its live regions
// (htmx `hx-trigger="sse:<event>"`), so all rendering stays in the normal
// request path. Changes are coalesced so a burst costs one refresh.
const (
	changeDebounce = 750 * time.Millisecond
	treePoll       = 5 * time.Second
	clockRefresh   = time.Minute // keeps "5 minutes ago" texts current
)

// notifier sends one coalesced event per burst of changes.
type notifier struct {
	sse     *sseWriter
	event   string
	timer   *time.Timer
	pending bool
}

func newNotifier(sse *sseWriter, event string) *notifier {
	t := time.NewTimer(time.Hour)
	t.Stop()
	return &notifier{sse: sse, event: event, timer: t}
}

func (n *notifier) changed() {
	if !n.pending {
		n.pending = true
		n.timer.Reset(changeDebounce)
	}
}

func (n *notifier) fire() error {
	n.pending = false
	return n.sse.event(n.event, "changed")
}

// knownVersions snapshots resource versions so the watch's initial replay
// (every application as ADDED) does not count as a change.
func (s *Server) knownVersions(r *http.Request) map[string]string {
	known := map[string]string{}
	if apps, err := s.Store.ListApplications(r.Context()); err == nil {
		for _, a := range apps {
			known[a.Metadata.Name] = a.Metadata.ResourceVersion
		}
	}
	return known
}

// webStreamApps notifies the applications page: event "apps".
func (s *Server) webStreamApps(w http.ResponseWriter, r *http.Request) {
	known := s.knownVersions(r)
	sse, ok := startSSE(w)
	if !ok {
		return
	}
	n := newNotifier(sse, "apps")
	events := s.Store.WatchApplications(r.Context())
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	clock := time.NewTicker(clockRefresh)
	defer clock.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			sse.comment("keepalive")
		case <-clock.C:
			n.changed()
		case <-n.timer.C:
			if n.fire() != nil {
				return
			}
		case ev, ok := <-events:
			if !ok {
				return
			}
			name := ev.Application.Metadata.Name
			if ev.Type == store.EventDeleted {
				if _, was := known[name]; was {
					delete(known, name)
					n.changed()
				}
				continue
			}
			if known[name] == ev.Application.Metadata.ResourceVersion {
				continue
			}
			_, was := known[name]
			known[name] = ev.Application.Metadata.ResourceVersion
			// Hidden applications only matter when they were shown before
			// (a project change can hide one).
			if was || s.visible(r, ev.Application) {
				n.changed()
			}
		}
	}
}

// webStreamApp notifies an application's details page: event "app", when
// the Application changes or its live resource tree does (allocations move
// between reconciliations, so the tree is polled).
func (s *Server) webStreamApp(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r, r.PathValue("name"), "get")
	if err != nil {
		http.Error(w, err.Error(), errorCode(err))
		return
	}
	sse, ok := startSSE(w)
	if !ok {
		return
	}
	name, version := app.Metadata.Name, app.Metadata.ResourceVersion
	n := newNotifier(sse, "app")
	treeHash := s.treeHash(r, app)
	events := s.Store.WatchApplications(r.Context())
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	poll := time.NewTicker(treePoll)
	defer poll.Stop()
	clock := time.NewTicker(clockRefresh)
	defer clock.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			sse.comment("keepalive")
		case <-clock.C:
			n.changed()
		case <-n.timer.C:
			if n.fire() != nil {
				return
			}
		case <-poll.C:
			if h := s.treeHash(r, app); h != treeHash {
				treeHash = h
				n.changed()
			}
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Application.Metadata.Name != name {
				continue
			}
			if ev.Type == store.EventDeleted {
				// The page reloads into "not found".
				_ = sse.event("app", "deleted")
				return
			}
			if ev.Application.Metadata.ResourceVersion == version {
				continue
			}
			version = ev.Application.Metadata.ResourceVersion
			app = ev.Application
			n.changed()
		}
	}
}

func (s *Server) treeHash(r *http.Request, app *v1.Application) [32]byte {
	tree, err := s.liveTree(r.Context(), app)
	if err != nil {
		return sha256.Sum256([]byte(err.Error()))
	}
	b, _ := json.Marshal(tree)
	return sha256.Sum256(b)
}
