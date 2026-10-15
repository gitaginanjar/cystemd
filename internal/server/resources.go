// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/internal/diff"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

func jobYAML(j *api.Job) (string, error) {
	m, err := diff.Normalize(j)
	if err != nil {
		return "", err
	}
	return diff.ToYAML(m), nil
}

func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "get")
	if app == nil {
		return
	}
	tree, err := s.Live.Tree(r.Context(), app)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tree)
}

func (s *Server) handleManagedResources(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "get")
	if app == nil {
		return
	}
	items, err := s.Live.ManagedResources(r.Context(), app)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// resourceQuery identifies a tree node from query parameters.
type resourceQuery struct{ kind, namespace, name, uid string }

func resourceParams(r *http.Request) resourceQuery {
	q := r.URL.Query()
	return resourceQuery{kind: q.Get("kind"), namespace: q.Get("namespace"), name: q.Get("resourceName"), uid: q.Get("uid")}
}

func (s *Server) handleResource(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "get")
	if app == nil {
		return
	}
	rq := resourceParams(r)
	if rq.kind == v1.KindAllocation || rq.kind == v1.KindTask {
		allocID := strings.TrimPrefix(strings.TrimPrefix(rq.uid, "alloc:"), "task:")
		allocID, _, _ = strings.Cut(allocID, "/")
		if _, _, err := s.Live.OwnedAllocation(r.Context(), app, allocID); err != nil {
			s.fail(w, err)
			return
		}
	}
	manifest, err := s.Live.LiveManifest(r.Context(), app, rq.kind, rq.namespace, rq.name, rq.uid)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"manifest": manifest})
}

func (s *Server) handleListActions(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "get")
	if app == nil {
		return
	}
	rq := resourceParams(r)
	writeJSON(w, http.StatusOK, map[string]any{"actions": s.Live.Actions(rq.kind, nil)})
}

type actionRequest struct {
	Action string `json:"action"`
}

func (s *Server) handleRunAction(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "action")
	if app == nil {
		return
	}
	var req actionRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	rq := resourceParams(r)
	target := fmt.Sprintf("%s %s/%s (%s)", rq.kind, rq.namespace, rq.name, req.Action)
	if err := s.Live.RunAction(r.Context(), app, rq.kind, rq.namespace, rq.name, rq.uid, req.Action); err != nil {
		s.audit(r, "applications/action", appObject(app), "FAILED", target+": "+err.Error())
		s.fail(w, err)
		return
	}
	s.audit(r, "applications/action", appObject(app), "ALLOWED", target)
	_ = s.Store.RecordEvent(r.Context(), app.Metadata.Name, v1.Event{
		Type: "Normal", Reason: "ResourceActionRan", Username: identity(r).Username,
		Message: fmt.Sprintf("%s ran action %s on %s %s", identity(r).Username, req.Action, rq.kind, rq.name),
		Kind:    rq.kind, Name: rq.name, Namespace: rq.namespace,
	})
	writeJSON(w, http.StatusOK, map[string]any{})
}

// LogEntry is one streamed log line (Argo CD's LogEntry).
type LogEntry struct {
	Content   string    `json:"content"`
	TimeStamp time.Time `json:"timeStamp"`
	AllocID   string    `json:"allocId"`
	Task      string    `json:"task"`
	Last      bool      `json:"last,omitempty"`
}

// handleLogs streams an allocation task's logs as server-sent events.
// tailLines approximates `tail -n` by starting near the end of the file.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "get")
	if app == nil {
		return
	}
	if !s.can(w, r, "logs", "get", appObject(app)) {
		return
	}
	alloc, cluster, err := s.Live.OwnedAllocation(r.Context(), app, r.PathValue("alloc"))
	if err != nil {
		s.fail(w, err)
		return
	}
	q := r.URL.Query()
	task := q.Get("task")
	if task == "" {
		for name := range alloc.TaskStates {
			if task == "" || name < task {
				task = name
			}
		}
	}
	logType := q.Get("type")
	if logType != "stderr" {
		logType = "stdout"
	}
	follow := q.Get("follow") == "true"
	tail, _ := strconv.Atoi(q.Get("tailLines"))
	if tail <= 0 {
		tail = 500
	}
	offset := int64(tail) * 240

	sse, ok := startSSE(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	frames, errs := cluster.Logs(ctx, alloc, task, logType, "end", offset, follow)

	first := true
	var pending []byte
	emit := func(line string) {
		_ = sse.send(map[string]any{"result": LogEntry{Content: line, TimeStamp: time.Now().UTC(), AllocID: alloc.ID, Task: task}})
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	idle := time.NewTimer(2 * time.Second)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			sse.comment("keepalive")
		case <-idle.C:
			if !follow {
				if len(pending) > 0 {
					emit(string(pending))
				}
				_ = sse.send(map[string]any{"result": LogEntry{Last: true, AllocID: alloc.ID, Task: task, TimeStamp: time.Now().UTC()}})
				return
			}
		case err, ok := <-errs:
			if ok && err != nil {
				_ = sse.send(map[string]any{"error": err.Error()})
				return
			}
		case f, ok := <-frames:
			if !ok {
				if len(pending) > 0 {
					emit(string(pending))
				}
				_ = sse.send(map[string]any{"result": LogEntry{Last: true, AllocID: alloc.ID, Task: task, TimeStamp: time.Now().UTC()}})
				return
			}
			if f == nil || len(f.Data) == 0 {
				continue
			}
			idle.Reset(2 * time.Second)
			data := append(pending, f.Data...)
			if first && offset > 0 {
				// Started mid-file: drop the partial first line.
				if i := bytes.IndexByte(data, '\n'); i >= 0 {
					data = data[i+1:]
				}
				first = false
			}
			sc := bufio.NewScanner(bytes.NewReader(data))
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			var lines []string
			for sc.Scan() {
				lines = append(lines, sc.Text())
			}
			pending = nil
			if len(data) > 0 && data[len(data)-1] != '\n' && len(lines) > 0 {
				pending = []byte(lines[len(lines)-1])
				lines = lines[:len(lines)-1]
			}
			for _, l := range lines {
				emit(l)
			}
		}
	}
}

// sseWriter writes server-sent events.
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func startSSE(w http.ResponseWriter) (*sseWriter, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &sseWriter{w: w, f: f}, true
}

func (s *sseWriter) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}

func (s *sseWriter) comment(c string) {
	_, _ = fmt.Fprintf(s.w, ": %s\n\n", c)
	s.f.Flush()
}
