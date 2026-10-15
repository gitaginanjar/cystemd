// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gitaginanjar/cystemd/internal/apitest"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
	"github.com/gitaginanjar/cystemd/pkg/client"
)

func app(name, team string) *v1.Application {
	return &v1.Application{
		Metadata: v1.ObjectMeta{Name: name, Labels: map[string]string{"team": team}},
		Spec:     v1.ApplicationSpec{Source: v1.ApplicationSource{RepoURL: "https://git.example.com/ops.git", Path: "apps/" + name}},
	}
}

func TestAgainstServer(t *testing.T) {
	s := apitest.New(t)
	ctx := context.Background()
	c, err := client.New(s.URL, client.Options{UserAgent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Login(ctx, "admin", "wrong"); !client.IsUnauthorized(err) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := c.ListApplications(ctx, client.ListOptions{}); !client.IsUnauthorized(err) {
		t.Fatalf("no session: %v", err)
	}
	if c.Token, err = c.Login(ctx, "admin", s.Password); err != nil {
		t.Fatal(err)
	}
	info, err := c.UserInfo(ctx)
	if err != nil || info.Username != "admin" || !info.IsAdmin {
		t.Fatalf("user info: %+v %v", info, err)
	}
	if v, err := c.Version(ctx); err != nil || v.Version != "test" || v.Mode != "standalone" {
		t.Fatalf("version: %+v %v", v, err)
	}

	// Watch from the start: the stream sends what exists, then changes.
	events := make(chan string, 16)
	wctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = c.WatchApplications(wctx, "", func(typ string, a *v1.Application) error {
			events <- typ + " " + a.Metadata.Name
			return nil
		})
	}()

	for _, a := range []*v1.Application{app("web", "a"), app("api", "b")} {
		if _, err := c.CreateApplication(ctx, a, false); err != nil {
			t.Fatal(err)
		}
	}
	var ce *client.Error
	if _, err := c.CreateApplication(ctx, app("web", "a"), false); !errors.As(err, &ce) || ce.Code != 409 {
		t.Fatalf("duplicate: %v", err)
	}
	apps, err := c.ListApplications(ctx, client.ListOptions{Selector: "team=b"})
	if err != nil || len(apps) != 1 || apps[0].Metadata.Name != "api" {
		t.Fatalf("selector: %v %v", apps, err)
	}
	web, err := c.GetApplication(ctx, "web", "")
	if err != nil {
		t.Fatal(err)
	}
	web.Spec.Source.TargetRevision = "v2"
	if _, err := c.UpdateApplication(ctx, web); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteApplication(ctx, "api", false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetApplication(ctx, "api", ""); !client.IsNotFound(err) {
		t.Fatalf("deleted: %v", err)
	}
	want := map[string]bool{"ADDED web": false, "ADDED api": false, "MODIFIED web": false, "DELETED api": false}
	deadline := time.After(10 * time.Second)
	for missing := len(want); missing > 0; {
		select {
		case ev := <-events:
			if seen, ok := want[ev]; ok && !seen {
				want[ev] = true
				missing--
			}
		case <-deadline:
			t.Fatalf("watch events: %v", want)
		}
	}

	p := &v1.AppProject{Metadata: v1.ObjectMeta{Name: "team"}, Spec: v1.AppProjectSpec{SourceRepos: []string{"*"}}}
	if err := c.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Description = "changed"
	if err := c.UpdateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got, err := c.GetProject(ctx, "team"); err != nil || got.Spec.Description != "changed" {
		t.Fatalf("project: %+v %v", got, err)
	}
	if err := c.DeleteProject(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	if clusters, err := c.ListClusters(ctx); err != nil || len(clusters) != 1 || clusters[0].Name != v1.InClusterName {
		t.Fatalf("clusters: %+v %v", clusters, err)
	}
	if accts, err := c.ListAccounts(ctx); err != nil || len(accts) != 1 || accts[0].Name != "admin" {
		t.Fatalf("accounts: %+v %v", accts, err)
	}
}

func TestLogStreamAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/applications/web/allocations/a1/logs":
			if r.URL.Query().Get("tailLines") != "5" || r.URL.Query().Get("type") != "stderr" || r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "bad request", 400)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": keep-alive\n\n")
			fmt.Fprint(w, "event: log\ndata: {\"result\":{\"content\":\"one\",\"task\":\"t\"}}\n\n")
			fmt.Fprint(w, "data: {\"result\":{\"content\":\"two\",\"task\":\"t\"}}\n\n")
			fmt.Fprint(w, "data: {\"result\":{\"last\":true}}\n\n")
			fmt.Fprint(w, "data: {\"result\":{\"content\":\"after the end\"}}\n\n")
		case "/api/v1/applications/broken/allocations/a1/logs":
			fmt.Fprint(w, "data: {\"error\":\"alloc gone\"}\n\n")
		default:
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, "<html>proxy error</html>")
		}
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, client.Options{Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var lines []string
	err = c.Logs(ctx, "web", "a1", client.LogOptions{Stderr: true, TailLines: 5}, func(l client.LogLine) error {
		lines = append(lines, l.Task+":"+l.Content)
		return nil
	})
	if err != nil || strings.Join(lines, ",") != "t:one,t:two" {
		t.Fatalf("logs: %q %v", lines, err)
	}
	if err := c.Logs(ctx, "broken", "a1", client.LogOptions{}, func(client.LogLine) error { return nil }); err == nil || err.Error() != "alloc gone" {
		t.Fatalf("stream error: %v", err)
	}
	// An HTML error page is not shown as the message.
	_, err = c.GetApplication(ctx, "x", "")
	if err == nil || err.Error() != "server returned 502 Bad Gateway" {
		t.Fatalf("502: %v", err)
	}
	if _, err := client.New("://bad", client.Options{}); err == nil {
		t.Error("bad URL accepted")
	}
}
