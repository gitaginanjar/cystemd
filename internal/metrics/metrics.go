// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package metrics exposes the control plane's Prometheus metrics, named
// after Argo CD's (cystemd_app_info ≈ argocd_app_info) so existing
// dashboards and alerts translate directly.
package metrics

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Metrics is the control plane's registry.
type Metrics struct {
	Registry *prometheus.Registry

	AppInfo           *prometheus.GaugeVec
	SyncTotal         *prometheus.CounterVec
	ReconcileDuration *prometheus.HistogramVec
	HTTPRequests      *prometheus.CounterVec
	HTTPDuration      *prometheus.HistogramVec
	Audit             *prometheus.CounterVec
	Leader            prometheus.Gauge
	GitRequests       *prometheus.CounterVec

	mu   sync.Mutex
	apps map[string]prometheus.Labels
}

// New registers every metric on a fresh registry.
func New() *Metrics {
	r := prometheus.NewRegistry()
	m := &Metrics{
		Registry: r,
		AppInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cystemd_app_info",
			Help: "Information about Applications: always 1, the labels carry the state.",
		}, []string{"name", "project", "repo", "dest_cluster", "dest_namespace", "sync_status", "health_status", "autosync_enabled", "source_type"}),
		SyncTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cystemd_app_sync_total",
			Help: "Sync operations by phase.",
		}, []string{"name", "phase"}),
		ReconcileDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cystemd_app_reconcile_seconds",
			Help:    "Duration of Application reconciliations.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64},
		}, []string{"name"}),
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cystemd_http_requests_total",
			Help: "API requests by route, method and status.",
		}, []string{"route", "method", "status"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "cystemd_http_request_duration_seconds",
			Help:    "API request duration by route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route"}),
		Audit: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cystemd_audit_total",
			Help: "Audited API actions by result (allowed, denied, failed).",
		}, []string{"result"}),
		Leader: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "cystemd_controller_leader",
			Help: "1 when this replica runs the controller.",
		}),
		GitRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cystemd_git_request_total",
			Help: "Git requests by operation and result.",
		}, []string{"op", "result"}),
		apps: map[string]prometheus.Labels{},
	}
	r.MustRegister(m.AppInfo, m.SyncTotal, m.ReconcileDuration, m.HTTPRequests, m.HTTPDuration,
		m.Audit, m.Leader, m.GitRequests,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

// ObserveApp sets the app-info series of app (replacing the previous one).
func (m *Metrics) ObserveApp(app *v1.Application) {
	labels := prometheus.Labels{
		"name":             app.Metadata.Name,
		"project":          app.Spec.Project,
		"repo":             app.Spec.Source.RepoURL,
		"dest_cluster":     app.Spec.Destination.ClusterName(),
		"dest_namespace":   app.Spec.Destination.NamespaceOrDefault(),
		"sync_status":      string(app.Status.Sync.Status),
		"health_status":    string(app.Status.Health.Status),
		"autosync_enabled": boolLabel(app.Spec.SyncPolicy.IsAutomated()),
		"source_type":      string(app.Spec.Source.Type()),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.apps[app.Metadata.Name]; ok {
		m.AppInfo.Delete(prev)
	}
	m.apps[app.Metadata.Name] = labels
	m.AppInfo.With(labels).Set(1)
}

// ForgetApp removes a deleted Application's series.
func (m *Metrics) ForgetApp(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.apps[name]; ok {
		m.AppInfo.Delete(prev)
		delete(m.apps, name)
	}
	m.SyncTotal.DeletePartialMatch(prometheus.Labels{"name": name})
	m.ReconcileDuration.DeletePartialMatch(prometheus.Labels{"name": name})
}

// Handler serves the registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

func boolLabel(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
