// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package controller is the CYSTEMD application controller (Argo CD's
// application-controller): it compares every Application's desired state
// (Git, rendered) with the live state (Nomad, Consul), records sync status
// and health, executes sync operations, and auto-syncs when the policy says
// so. It runs on the elected leader only; the API server writes operation
// requests into the store and the controller picks them up.
package controller

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/internal/metrics"
	"github.com/gitaginanjar/cystemd/internal/nomad"
	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// RefreshAnnotation requests a refresh: "normal" re-compares, "hard" also
// re-renders (Argo CD's argocd.argoproj.io/refresh).
const RefreshAnnotation = "cystemd.io/refresh"

// Config tunes the controller.
type Config struct {
	// ReconcileInterval is the periodic full comparison (default 3m).
	ReconcileInterval time.Duration
	// SelfHealTimeout is the minimum delay between self-heal syncs (5s).
	SelfHealTimeout time.Duration
	// Workers process Applications concurrently (default 10).
	Workers int
	// EventDebounce coalesces Nomad events per Application (default 2s).
	EventDebounce time.Duration
}

func (c *Config) defaults() {
	if c.ReconcileInterval <= 0 {
		c.ReconcileInterval = 3 * time.Minute
	}
	if c.SelfHealTimeout <= 0 {
		c.SelfHealTimeout = 5 * time.Second
	}
	if c.Workers <= 0 {
		c.Workers = 10
	}
	if c.EventDebounce <= 0 {
		c.EventDebounce = 2 * time.Second
	}
}

// Controller reconciles Applications.
type Controller struct {
	store    *store.Store
	clusters *nomad.Clusters
	repos    Repos
	consul   ConsulKV
	checks   ConsulChecks
	logger   *slog.Logger
	metrics  *metrics.Metrics
	cfg      Config
	now      func() time.Time

	plans planCache
	queue *workQueue

	// jobOwners routes Nomad events: "<ns>/<job>" → Application.
	ownersMu  sync.RWMutex
	jobOwners map[string]string
}

// Options builds a Controller.
type Options struct {
	Store    *store.Store
	Clusters *nomad.Clusters
	Repos    Repos
	// Consul is optional: without it, Applications cannot manage keys.
	Consul  ConsulKV
	Checks  ConsulChecks
	Logger  *slog.Logger
	Metrics *metrics.Metrics
	Config  Config
}

// New returns a controller.
func New(o Options) *Controller {
	o.Config.defaults()
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Metrics == nil {
		o.Metrics = metrics.New()
	}
	return &Controller{
		store: o.Store, clusters: o.Clusters, repos: o.Repos, consul: o.Consul, checks: o.Checks,
		logger: o.Logger.With("component", "controller"), metrics: o.Metrics, cfg: o.Config,
		now: time.Now, queue: newWorkQueue(), jobOwners: map[string]string{},
	}
}

// Run reconciles until ctx ends. Call it on the leader only.
func (c *Controller) Run(ctx context.Context) {
	c.logger.Info("controller started", "reconcile_interval", c.cfg.ReconcileInterval, "workers", c.cfg.Workers)
	c.metrics.Leader.Set(1)
	defer c.metrics.Leader.Set(0)
	queue := newWorkQueue()
	c.queue = queue
	defer queue.shutdown()

	var wg sync.WaitGroup
	for i := 0; i < c.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				name, hard, ok := queue.get()
				if !ok {
					return
				}
				c.processApp(ctx, name, hard)
				queue.done(name)
			}
		}()
	}

	// Store changes: new Applications, spec edits, operations, refresh
	// requests and deletions all arrive here.
	go func() {
		for ev := range c.store.WatchApplications(ctx) {
			if ev.Type == store.EventDeleted {
				continue
			}
			queue.add(ev.Application.Metadata.Name, ev.Application.Metadata.Annotations[RefreshAnnotation] == "hard")
		}
	}()

	// Live changes: Nomad's event stream, per cluster.
	for _, name := range c.clusters.Names() {
		cluster, _ := c.clusters.Get(name)
		go c.watchNomad(ctx, name, cluster)
	}

	// Periodic resync.
	ticker := time.NewTicker(c.cfg.ReconcileInterval)
	defer ticker.Stop()
	c.enqueueAll(ctx)
	for {
		select {
		case <-ctx.Done():
			queue.shutdown()
			wg.Wait()
			c.logger.Info("controller stopped")
			return
		case <-ticker.C:
			c.enqueueAll(ctx)
		}
	}
}

func (c *Controller) enqueueAll(ctx context.Context) {
	apps, err := c.store.ListApplications(ctx)
	if err != nil {
		c.logger.Warn("list applications", "error", err)
		return
	}
	for _, a := range apps {
		c.queue.add(a.Metadata.Name, false)
	}
}

// Refresh enqueues an Application (used in-process by the API when the
// controller runs in the same replica).
func (c *Controller) Refresh(name string, hard bool) { c.queue.add(name, hard) }

// processApp is one reconciliation of an Application.
func (c *Controller) processApp(ctx context.Context, name string, hard bool) {
	start := c.now()
	app, err := c.store.GetApplication(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		c.logger.Warn("get application", "app", name, "error", err)
		return
	}
	defer func() { c.metrics.ReconcileDuration.WithLabelValues(name).Observe(time.Since(start).Seconds()) }()

	if app.Metadata.DeletionTimestamp != nil {
		c.finalizeDeletion(ctx, app)
		return
	}
	if refresh := app.Metadata.Annotations[RefreshAnnotation]; refresh != "" {
		hard = hard || refresh == "hard"
	}

	cmp, cmpErr := c.compare(ctx, app, hard)

	// Execute a requested operation, then compare again so the status
	// reflects its effect.
	if app.Operation != nil {
		app = c.runOperation(ctx, app, cmp)
		if app == nil {
			return
		}
		cmp, cmpErr = c.compare(ctx, app, false)
	} else if cmpErr == nil {
		if op := c.autoSyncOperation(app, cmp); op != nil {
			if updated, err := c.store.MutateApplication(ctx, name, func(a *v1.Application) error {
				if a.Operation != nil {
					return errAlreadyRequested
				}
				a.Operation = op
				return nil
			}); err == nil {
				c.recordEvent(ctx, updated, "Normal", "OperationStarted", "Initiated automated sync to "+short(cmp.revision), "")
				app = c.runOperation(ctx, updated, cmp)
				if app == nil {
					return
				}
				cmp, cmpErr = c.compare(ctx, app, false)
			}
		}
	}

	c.persistStatus(ctx, app, cmp, cmpErr)
}

var errAlreadyRequested = errors.New("an operation is already requested")

// persistStatus writes the comparison into the Application's status.
func (c *Controller) persistStatus(ctx context.Context, app *v1.Application, cmp *comparison, cmpErr error) {
	now := c.now().UTC().Truncate(time.Second)
	updated, err := c.store.MutateApplication(ctx, app.Metadata.Name, func(a *v1.Application) error {
		st := &a.Status
		st.ReconciledAt = &now
		st.SourceType = a.Spec.Source.Type()
		conditions := []v1.ApplicationCondition{}
		if cmpErr != nil {
			st.Sync.Status = v1.SyncStatusUnknown
			st.Health = v1.HealthStatus{Status: v1.HealthStatusUnknown, Message: cmpErr.Error()}
			conditions = append(conditions, condition(v1.ConditionComparisonError, "%v", cmpErr))
		} else {
			st.Sync = v1.SyncStatus{
				Status:     cmp.sync,
				Revision:   cmp.revision,
				ComparedTo: v1.ComparedTo{Source: a.Spec.Source, Destination: a.Spec.Destination},
			}
			st.Health = cmp.health
			st.Resources = cmp.resources
			st.Summary.Images = cmp.manifests.Images
			st.ObservedAt = &now
			conditions = append(conditions, cmp.conditions...)
		}
		// Keep sync-error conditions from the last operation visible.
		if os := st.OperationState; os != nil && (os.Phase == v1.OperationFailed || os.Phase == v1.OperationError) &&
			cmp != nil && os.SyncResult != nil && os.SyncResult.Revision == cmp.revision {
			conditions = append(conditions, condition(v1.ConditionSyncError, "%s", os.Message))
		}
		st.Conditions = mergeConditions(st.Conditions, conditions)
		if a.Metadata.Annotations[RefreshAnnotation] != "" {
			delete(a.Metadata.Annotations, RefreshAnnotation)
		}
		return nil
	})
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			c.logger.Warn("persist status", "app", app.Metadata.Name, "error", err)
		}
		return
	}
	c.trackOwners(updated)
	c.metrics.ObserveApp(updated)
}

// mergeConditions keeps the transition time of conditions that persist.
func mergeConditions(prev, next []v1.ApplicationCondition) []v1.ApplicationCondition {
	seen := map[string]*time.Time{}
	for _, p := range prev {
		seen[p.Type+"\x00"+p.Message] = p.LastTransitionTime
	}
	for i := range next {
		if t, ok := seen[next[i].Type+"\x00"+next[i].Message]; ok && t != nil {
			next[i].LastTransitionTime = t
		}
	}
	if len(next) == 0 {
		return nil
	}
	return next
}

// trackOwners records which jobs an Application manages (event routing).
func (c *Controller) trackOwners(app *v1.Application) {
	c.ownersMu.Lock()
	defer c.ownersMu.Unlock()
	for k, v := range c.jobOwners {
		if v == app.Metadata.Name {
			delete(c.jobOwners, k)
		}
	}
	for _, r := range app.Status.Resources {
		if r.Kind == v1.KindJob {
			c.jobOwners[r.Namespace+"/"+r.Name] = app.Metadata.Name
		}
	}
}

func (c *Controller) ownerOf(namespace, job string) (string, bool) {
	c.ownersMu.RLock()
	defer c.ownersMu.RUnlock()
	app, ok := c.jobOwners[namespace+"/"+job]
	return app, ok
}

// watchNomad follows a cluster's event stream and refreshes the owning
// Application of every changed job, allocation or deployment.
func (c *Controller) watchNomad(ctx context.Context, cluster string, client nomad.API) {
	topics := map[api.Topic][]string{api.TopicJob: {"*"}, api.TopicAllocation: {"*"}, api.TopicDeployment: {"*"}}
	var index uint64
	backoff := time.Second
	debounce := newDebouncer(c.cfg.EventDebounce, func(app string) { c.queue.add(app, false) })
	defer debounce.stop()
	for ctx.Err() == nil {
		stream, err := client.Events(ctx, topics, index)
		if err != nil {
			c.logger.Debug("nomad event stream unavailable", "cluster", cluster, "error", err)
			sleep(ctx, backoff)
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		for events := range stream {
			if events.Err != nil {
				break
			}
			if events.IsHeartbeat() {
				continue
			}
			index = events.Index
			for _, e := range events.Events {
				ns, job := eventJob(e)
				if job == "" {
					continue
				}
				if app, ok := c.ownerOf(ns, job); ok {
					debounce.trigger(app)
				}
			}
		}
		sleep(ctx, backoff)
	}
}

// eventJob extracts the namespace and job ID an event concerns.
func eventJob(e api.Event) (string, string) {
	switch e.Topic {
	case api.TopicJob:
		if j, err := e.Job(); err == nil && j != nil {
			return deref(j.Namespace), deref(j.ID)
		}
	case api.TopicAllocation:
		if a, err := e.Allocation(); err == nil && a != nil {
			return a.Namespace, a.JobID
		}
	case api.TopicDeployment:
		if d, err := e.Deployment(); err == nil && d != nil {
			return d.Namespace, d.JobID
		}
	}
	return "", ""
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (c *Controller) recordEvent(ctx context.Context, app *v1.Application, typ, reason, message, user string) {
	err := c.store.RecordEvent(ctx, app.Metadata.Name, v1.Event{
		Type: typ, Reason: reason, Message: message, Username: user,
		Kind: v1.KindApplication, Name: app.Metadata.Name,
	})
	if err != nil {
		c.logger.Debug("record event", "app", app.Metadata.Name, "error", err)
	}
}
