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
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/internal/diff"
	"github.com/gitaginanjar/cystemd/internal/nomad"
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

// resourceManifest returns a tree node's live object as YAML.
func (s *Server) resourceManifest(r *http.Request, app *v1.Application, rq resourceQuery) (string, error) {
	if rq.kind == v1.KindAllocation || rq.kind == v1.KindTask {
		allocID := strings.TrimPrefix(strings.TrimPrefix(rq.uid, "alloc:"), "task:")
		allocID, _, _ = strings.Cut(allocID, "/")
		if _, _, err := s.Live.OwnedAllocation(r.Context(), app, allocID); err != nil {
			return "", err
		}
	}
	return s.Live.LiveManifest(r.Context(), app, rq.kind, rq.namespace, rq.name, rq.uid)
}

func (s *Server) handleResource(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "get")
	if app == nil {
		return
	}
	manifest, err := s.resourceManifest(r, app, resourceParams(r))
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

// runResourceAction runs a resource action (the caller needs "action" on
// the Application, checked by loadApp).
func (s *Server) runResourceAction(r *http.Request, app *v1.Application, rq resourceQuery, action string) error {
	target := fmt.Sprintf("%s %s/%s (%s)", rq.kind, rq.namespace, rq.name, action)
	if err := s.Live.RunAction(r.Context(), app, rq.kind, rq.namespace, rq.name, rq.uid, action); err != nil {
		s.audit(r, "applications/action", appObject(app), "FAILED", target+": "+err.Error())
		return err
	}
	s.audit(r, "applications/action", appObject(app), "ALLOWED", target)
	_ = s.Store.RecordEvent(r.Context(), app.Metadata.Name, v1.Event{
		Type: "Normal", Reason: "ResourceActionRan", Username: identity(r).Username,
		Message: fmt.Sprintf("%s ran action %s on %s %s", identity(r).Username, action, rq.kind, rq.name),
		Kind:    rq.kind, Name: rq.name, Namespace: rq.namespace,
	})
	return nil
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
	if err := s.runResourceAction(r, app, resourceParams(r), req.Action); err != nil {
		s.fail(w, err)
		return
	}
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

// logRequest selects a task's log stream.
type logRequest struct {
	task    string
	logType string // stdout or stderr
	follow  bool
	tail    int // roughly how many of the last lines to start with
}

func parseLogRequest(q url.Values, alloc *api.Allocation) logRequest {
	lr := logRequest{task: q.Get("task"), logType: q.Get("type"), follow: q.Get("follow") == "true"}
	if lr.task == "" || alloc.TaskStates[lr.task] == nil {
		lr.task = ""
		for name := range alloc.TaskStates {
			if lr.task == "" || name < lr.task {
				lr.task = name
			}
		}
	}
	if lr.logType != "stderr" {
		lr.logType = "stdout"
	}
	lr.tail, _ = strconv.Atoi(q.Get("tailLines"))
	if lr.tail <= 0 {
		lr.tail, _ = strconv.Atoi(q.Get("tail"))
	}
	if lr.tail <= 0 || lr.tail > 50000 {
		lr.tail = 500
	}
	return lr
}

// readLogs streams a task's log lines to emit, a batch per frame, until the
// log ends (when not following), ctx is done or emit fails. emit(nil) is a
// keep-alive tick. Starting near the end of a file, the partial first line
// is dropped.
func readLogs(ctx context.Context, cluster nomad.API, alloc *api.Allocation, lr logRequest, emit func(lines []string) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	frames, errs := cluster.Logs(ctx, alloc, lr.task, lr.logType, "end", int64(lr.tail)*240, lr.follow)
	first := true
	var pending []byte
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		line := string(pending)
		pending = nil
		return emit([]string{line})
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	idle := time.NewTimer(2 * time.Second)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-heartbeat.C:
			if err := emit(nil); err != nil {
				return err
			}
		case <-idle.C:
			if !lr.follow {
				return flush()
			}
		case err, ok := <-errs:
			if ok && err != nil {
				return err
			}
		case f, ok := <-frames:
			if !ok {
				return flush()
			}
			if f == nil || len(f.Data) == 0 {
				continue
			}
			idle.Reset(2 * time.Second)
			data := append(pending, f.Data...)
			if first {
				// Frame offsets mark where the data ends in the file.
				if start := f.Offset - int64(len(f.Data)); start > 0 {
					if i := bytes.IndexByte(data, '\n'); i >= 0 {
						data = data[i+1:]
					}
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
			if len(lines) > 0 {
				if err := emit(lines); err != nil {
					return err
				}
			}
		}
	}
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
	lr := parseLogRequest(r.URL.Query(), alloc)
	sse, ok := startSSE(w)
	if !ok {
		return
	}
	err = readLogs(r.Context(), cluster, alloc, lr, func(lines []string) error {
		if lines == nil {
			sse.comment("keepalive")
			return nil
		}
		for _, l := range lines {
			if err := sse.send(map[string]any{"result": LogEntry{Content: l, TimeStamp: time.Now().UTC(), AllocID: alloc.ID, Task: lr.task}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && r.Context().Err() == nil {
		_ = sse.send(map[string]any{"error": err.Error()})
		return
	}
	_ = sse.send(map[string]any{"result": LogEntry{Last: true, AllocID: alloc.ID, Task: lr.task, TimeStamp: time.Now().UTC()}})
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

// event writes a named event; data may span several lines.
func (s *sseWriter) event(name, data string) error { return s.eventID(name, data, "") }

// eventID writes a named event with an id (sent back as Last-Event-ID when
// the browser reconnects).
func (s *sseWriter) eventID(name, data, id string) error {
	var b strings.Builder
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	b.WriteString("event: " + name + "\n")
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: " + line + "\n")
	}
	b.WriteString("\n")
	if _, err := fmt.Fprint(s.w, b.String()); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}
