// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	consulapi "github.com/hashicorp/consul/api"

	"github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

func testApp(name string) *v1alpha1.Application {
	return &v1alpha1.Application{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec: v1alpha1.ApplicationSpec{
			Project: "default",
			Source:  v1alpha1.ApplicationSource{RepoURL: "https://git.example.com/ops.git", Path: "apps/" + name},
		},
	}
}

// backends runs fn against the memory KV and, when a consul binary is on
// PATH (or CYSTEMD_TEST_CONSUL_ADDR is set), against a real Consul.
func backends(t *testing.T, fn func(t *testing.T, kv KV)) {
	t.Run("memory", func(t *testing.T) { fn(t, NewMemoryKV()) })
	client := testConsul(t)
	if client == nil {
		return
	}
	t.Run("consul", func(t *testing.T) { fn(t, NewConsulKV(client)) })
}

func testConsul(t *testing.T) *consulapi.Client {
	t.Helper()
	addr := os.Getenv("CYSTEMD_TEST_CONSUL_ADDR")
	if addr == "" {
		bin, err := exec.LookPath("consul")
		if err != nil {
			return nil
		}
		port := 18500 + os.Getpid()%1000
		cmd := exec.Command(bin, "agent", "-dev", "-bind=127.0.0.1", "-client=127.0.0.1",
			fmt.Sprintf("-http-port=%d", port), "-grpc-port=-1", "-dns-port=-1",
			fmt.Sprintf("-server-port=%d", port+1000), fmt.Sprintf("-serf-lan-port=%d", port+2000),
			"-serf-wan-port=-1")
		if err := cmd.Start(); err != nil {
			t.Logf("cannot start consul: %v", err)
			return nil
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
		addr = fmt.Sprintf("127.0.0.1:%d", port)
	}
	cfg := consulapi.DefaultConfig()
	cfg.Address = addr
	client, err := consulapi.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if leader, err := client.Status().Leader(); err == nil && leader != "" {
			return client
		}
		if time.Now().After(deadline) {
			t.Log("consul did not become ready; skipping consul backend")
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestApplicationCRUD(t *testing.T) {
	backends(t, func(t *testing.T, kv KV) {
		ctx := context.Background()
		s := New(kv, fmt.Sprintf("test-%d", time.Now().UnixNano()))

		created, err := s.CreateApplication(ctx, testApp("web"))
		if err != nil {
			t.Fatal(err)
		}
		if created.Metadata.ResourceVersion == "" || created.Metadata.Generation != 1 || created.Metadata.CreationTimestamp == nil {
			t.Fatalf("metadata = %+v", created.Metadata)
		}
		if _, err := s.CreateApplication(ctx, testApp("web")); !errors.Is(err, ErrExists) {
			t.Fatalf("duplicate create: %v", err)
		}

		// Status-only update keeps the generation.
		created.Status.Sync.Status = v1alpha1.SyncStatusSynced
		updated, err := s.UpdateApplication(ctx, created)
		if err != nil {
			t.Fatal(err)
		}
		if updated.Metadata.Generation != 1 || updated.Status.Sync.Status != v1alpha1.SyncStatusSynced {
			t.Fatalf("after status update: %+v", updated.Metadata)
		}

		// A stale write is rejected.
		created.Spec.Source.Path = "elsewhere"
		if _, err := s.UpdateApplication(ctx, created); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale update: %v", err)
		}

		// Spec change bumps the generation.
		updated.Spec.Source.Path = "elsewhere"
		updated, err = s.UpdateApplication(ctx, updated)
		if err != nil || updated.Metadata.Generation != 2 {
			t.Fatalf("spec update: gen=%d err=%v", updated.Metadata.Generation, err)
		}

		// MutateApplication retries through concurrent writers.
		done := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func(i int) {
				_, err := s.MutateApplication(ctx, "web", func(a *v1alpha1.Application) error {
					a.Spec.Info = append(a.Spec.Info, v1alpha1.Info{Name: fmt.Sprint(i), Value: "x"})
					return nil
				})
				done <- err
			}(i)
		}
		for i := 0; i < 2; i++ {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		got, _ := s.GetApplication(ctx, "web")
		if len(got.Spec.Info) != 2 {
			t.Fatalf("lost update: %+v", got.Spec.Info)
		}

		apps, err := s.ListApplications(ctx)
		if err != nil || len(apps) != 1 {
			t.Fatalf("list = %d, %v", len(apps), err)
		}
		if err := s.DeleteApplication(ctx, "web"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetApplication(ctx, "web"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("after delete: %v", err)
		}
	})
}

func TestWatchApplications(t *testing.T) {
	backends(t, func(t *testing.T, kv KV) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s := New(kv, fmt.Sprintf("watch-%d", time.Now().UnixNano()))
		if _, err := s.CreateApplication(ctx, testApp("a")); err != nil {
			t.Fatal(err)
		}
		events := s.WatchApplications(ctx)
		expect := func(typ AppEventType, name string) {
			t.Helper()
			select {
			case ev := <-events:
				if ev.Type != typ || ev.Application.Metadata.Name != name {
					t.Fatalf("got %s %s, want %s %s", ev.Type, ev.Application.Metadata.Name, typ, name)
				}
			case <-ctx.Done():
				t.Fatalf("timed out waiting for %s %s", typ, name)
			}
		}
		expect(EventAdded, "a")
		if _, err := s.CreateApplication(ctx, testApp("b")); err != nil {
			t.Fatal(err)
		}
		expect(EventAdded, "b")
		if _, err := s.MutateApplication(ctx, "a", func(a *v1alpha1.Application) error {
			a.Status.Health.Status = v1alpha1.HealthStatusHealthy
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		expect(EventModified, "a")
		if err := s.DeleteApplication(ctx, "b"); err != nil {
			t.Fatal(err)
		}
		expect(EventDeleted, "b")
	})
}

func TestEventsRingBuffer(t *testing.T) {
	ctx := context.Background()
	s := New(NewMemoryKV(), "cystemd")
	for i := 0; i < maxEvents+5; i++ {
		if err := s.RecordEvent(ctx, "web", v1alpha1.Event{Reason: "OperationStarted", Message: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	// A repeat folds into the last event.
	_ = s.RecordEvent(ctx, "web", v1alpha1.Event{Reason: "OperationStarted", Message: fmt.Sprint(maxEvents + 4)})
	events, err := s.ListEvents(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != maxEvents || events[0].Count != 2 || events[0].Message != fmt.Sprint(maxEvents+4) {
		t.Fatalf("len=%d first=%+v", len(events), events[0])
	}
}

func TestProjectsAndSettings(t *testing.T) {
	ctx := context.Background()
	s := New(NewMemoryKV(), "cystemd")
	projects, _ := s.ListProjects(ctx)
	if len(projects) != 1 || projects[0].Metadata.Name != "default" {
		t.Fatalf("implicit default project missing: %+v", projects)
	}
	p, err := s.GetProject(ctx, "default")
	if err != nil || !p.IsSourcePermitted("git@host:any/repo.git") {
		t.Fatalf("default project: %v", err)
	}
	first, _ := s.SetSettingIfAbsent(ctx, "signing-key", []byte("one"))
	second, _ := s.SetSettingIfAbsent(ctx, "signing-key", []byte("two"))
	if string(first) != "one" || string(second) != "one" {
		t.Fatalf("first writer must win: %q %q", first, second)
	}
}

func TestPutProject(t *testing.T) {
	ctx := context.Background()
	s := New(NewMemoryKV(), "cystemd")
	clock := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	p := &v1alpha1.AppProject{Metadata: v1alpha1.ObjectMeta{Name: "platform"}, Spec: v1alpha1.AppProjectSpec{SourceRepos: []string{"*"}}}
	if err := s.PutProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	read, err := s.GetProject(ctx, "platform")
	if err != nil || read.Metadata.CreationTimestamp == nil || !read.Metadata.CreationTimestamp.Equal(clock) {
		t.Fatalf("%v %+v", err, read)
	}

	// Updates keep the creation time; a resourceVersion makes them
	// conditional.
	clock = clock.Add(time.Hour)
	read.Spec.Description = "Platform team"
	if err := s.PutProject(ctx, read); err != nil {
		t.Fatal(err)
	}
	stale := *read
	stale.Spec.Description = "lost update"
	if err := s.PutProject(ctx, &stale); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write: %v", err)
	}
	again, _ := s.GetProject(ctx, "platform")
	if again.Spec.Description != "Platform team" || !again.Metadata.CreationTimestamp.Equal(clock.Add(-time.Hour)) {
		t.Fatalf("%+v", again)
	}
	unconditional := &v1alpha1.AppProject{Metadata: v1alpha1.ObjectMeta{Name: "platform"}, Spec: v1alpha1.AppProjectSpec{Description: "form"}}
	if err := s.PutProject(ctx, unconditional); err != nil {
		t.Fatal(err)
	}
}

func TestProjectStatusIsTheControlPlanes(t *testing.T) {
	ctx := context.Background()
	s := New(NewMemoryKV(), "cystemd")
	p := &v1alpha1.AppProject{Metadata: v1alpha1.ObjectMeta{Name: "payments"}, Spec: v1alpha1.AppProjectSpec{
		Roles: []v1alpha1.ProjectRole{{Name: "ci"}, {Name: "ops"}},
	}}
	// A client's status is ignored.
	p.Status.JWTTokensByRole = map[string]v1alpha1.JWTTokens{"ci": {Items: []v1alpha1.JWTToken{{ID: "forged", IssuedAt: 1}}}}
	if err := s.PutProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetProject(ctx, "payments"); len(got.Status.JWTTokensByRole) != 0 {
		t.Fatalf("client status stored: %+v", got.Status)
	}

	// MutateProject records tokens; a later replacement keeps them.
	add := func(role, id string) {
		t.Helper()
		if _, err := s.MutateProject(ctx, "payments", func(p *v1alpha1.AppProject) error {
			if p.Status.JWTTokensByRole == nil {
				p.Status.JWTTokensByRole = map[string]v1alpha1.JWTTokens{}
			}
			toks := p.Status.JWTTokensByRole[role]
			toks.Items = append(toks.Items, v1alpha1.JWTToken{ID: id, IssuedAt: 100})
			p.Status.JWTTokensByRole[role] = toks
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	add("ci", "t1")
	add("ops", "t2")
	replaced := &v1alpha1.AppProject{Metadata: v1alpha1.ObjectMeta{Name: "payments"}, Spec: v1alpha1.AppProjectSpec{
		Description: "re-applied", Roles: []v1alpha1.ProjectRole{{Name: "ci"}, {Name: "ops"}},
	}}
	if err := s.PutProject(ctx, replaced); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetProject(ctx, "payments")
	if got.Spec.Description != "re-applied" || len(got.RoleTokens("ci")) != 1 || len(got.RoleTokens("ops")) != 1 {
		t.Fatalf("tokens lost on replace: %+v", got.Status)
	}
	// A role's tokens go with it.
	got.Spec.Roles = got.Spec.Roles[:1]
	if err := s.PutProject(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetProject(ctx, "payments")
	if len(got.RoleTokens("ci")) != 1 || got.Status.JWTTokensByRole["ops"].Items != nil {
		t.Fatalf("after removing ops: %+v", got.Status)
	}

	// The default project, never stored, is stored on its first change.
	def, err := s.MutateProject(ctx, v1alpha1.DefaultProject, func(p *v1alpha1.AppProject) error {
		p.Spec.Roles = append(p.Spec.Roles, v1alpha1.ProjectRole{Name: "viewer"})
		return nil
	})
	if err != nil || def.Role("viewer") == nil || def.Metadata.ResourceVersion == "" || len(def.Spec.SourceRepos) != 1 {
		t.Fatalf("default: %+v %v", def, err)
	}
	if _, err := s.MutateProject(ctx, "nope", func(*v1alpha1.AppProject) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project: %v", err)
	}
}

func TestRepositoryNeverStoresSecrets(t *testing.T) {
	ctx := context.Background()
	kv := NewMemoryKV()
	s := New(kv, "cystemd")
	if err := s.PutRepository(ctx, &v1alpha1.Repository{Repo: "git@h:r.git", SSHPrivateKey: "KEY", Password: "pw"}); err != nil {
		t.Fatal(err)
	}
	entries, _, _ := kv.List(ctx, "cystemd/repositories/")
	if len(entries) != 1 || string(entries[0].Value) == "" {
		t.Fatal("repository not stored")
	}
	for _, secret := range []string{"KEY", "pw"} {
		if contains(string(entries[0].Value), secret) {
			t.Fatalf("secret %q persisted in the repository record", secret)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
