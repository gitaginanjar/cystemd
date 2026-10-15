// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// ApplicationWatchEvent is one streamed change (Argo CD's shape).
type ApplicationWatchEvent struct {
	Type        string          `json:"type"`
	Application *v1.Application `json:"application"`
}

// handleStreamApps streams Application changes: every visible Application
// as ADDED first, then changes as they happen.
func (s *Server) handleStreamApps(w http.ResponseWriter, r *http.Request) {
	sse, ok := startSSE(w)
	if !ok {
		return
	}
	match := listFilter(r)
	visible := map[string]bool{}
	events := s.Store.WatchApplications(r.Context())
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			sse.comment("keepalive")
		case ev, ok := <-events:
			if !ok {
				return
			}
			name := ev.Application.Metadata.Name
			if ev.Type == store.EventDeleted {
				if visible[name] {
					delete(visible, name)
					_ = sse.send(map[string]any{"result": ApplicationWatchEvent{Type: string(ev.Type), Application: ev.Application}})
				}
				continue
			}
			if !match(ev.Application) || !s.visible(r, ev.Application) {
				// Became invisible (project change): tell the client.
				if visible[name] {
					delete(visible, name)
					_ = sse.send(map[string]any{"result": ApplicationWatchEvent{Type: string(store.EventDeleted), Application: ev.Application}})
				}
				continue
			}
			typ := string(ev.Type)
			if !visible[name] {
				typ = string(store.EventAdded)
			}
			visible[name] = true
			if err := sse.send(map[string]any{"result": ApplicationWatchEvent{Type: typ, Application: ev.Application}}); err != nil {
				return
			}
		}
	}
}

// handleStreamTree streams an Application's resource tree whenever the
// Application changes, and every few seconds (allocations move between
// reconciliations).
func (s *Server) handleStreamTree(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "get")
	if app == nil {
		return
	}
	sse, ok := startSSE(w)
	if !ok {
		return
	}
	name := app.Metadata.Name
	var last []byte
	push := func(a *v1.Application) bool {
		tree, err := s.Live.Tree(r.Context(), a)
		if err != nil {
			return sse.send(map[string]any{"error": err.Error()}) == nil
		}
		b, _ := json.Marshal(tree)
		if string(b) == string(last) {
			return true
		}
		last = b
		return sse.send(map[string]any{"result": tree}) == nil
	}
	if !push(app) {
		return
	}
	events := s.Store.WatchApplications(r.Context())
	poll := time.NewTicker(5 * time.Second)
	defer poll.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Application.Metadata.Name != name {
				continue
			}
			if ev.Type == store.EventDeleted {
				return
			}
			app = ev.Application
			if !push(app) {
				return
			}
		case <-poll.C:
			if !push(app) {
				return
			}
		}
	}
}
