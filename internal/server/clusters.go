// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Cluster describes a destination Nomad cluster (Settings → Clusters).
type Cluster struct {
	Name            string             `json:"name"`
	Server          string             `json:"server"`
	Version         string             `json:"version,omitempty"`
	Region          string             `json:"region,omitempty"`
	Regions         []string           `json:"regions,omitempty"`
	Namespaces      []string           `json:"namespaces,omitempty"`
	NodePools       []string           `json:"nodePools,omitempty"`
	Nodes           int                `json:"nodes"`
	ReadyNodes      int                `json:"readyNodes"`
	Applications    int                `json:"applications"`
	ConnectionState v1.ConnectionState `json:"connectionState"`
}

func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	if !s.can(w, r, "clusters", "get", "*") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	apps, _ := s.Store.ListApplications(ctx)
	out := []Cluster{}
	for _, name := range s.Clusters.Names() {
		c, _ := s.Clusters.Get(name)
		cl := Cluster{Name: name, Server: c.Address()}
		now := time.Now().UTC().Truncate(time.Second)
		cl.ConnectionState = v1.ConnectionState{Status: v1.ConnectionSuccessful, AttemptedAt: &now}
		if info, err := c.Info(ctx); err != nil {
			cl.ConnectionState = v1.ConnectionState{Status: v1.ConnectionFailed, Message: err.Error(), AttemptedAt: &now}
		} else {
			cl.Version, cl.Region, cl.Regions = info.Version, info.Region, info.Regions
		}
		if ns, err := c.Namespaces(ctx); err == nil {
			cl.Namespaces = ns
		}
		if pools, err := c.NodePools(ctx); err == nil {
			for _, p := range pools {
				cl.NodePools = append(cl.NodePools, p.Name)
			}
		}
		if nodes, err := c.Nodes(ctx); err == nil {
			cl.Nodes = len(nodes)
			for _, n := range nodes {
				if n.Status == "ready" {
					cl.ReadyNodes++
				}
			}
		}
		for _, a := range apps {
			if a.Spec.Destination.ClusterName() == name {
				cl.Applications++
			}
		}
		out = append(out, cl)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// Node is a Nomad client node.
type Node struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Datacenter string   `json:"datacenter"`
	NodePool   string   `json:"nodePool"`
	Status     string   `json:"status"`
	Eligible   bool     `json:"eligible"`
	Drain      bool     `json:"drain"`
	Address    string   `json:"address"`
	Version    string   `json:"version"`
	Drivers    []string `json:"drivers"`
}

func (s *Server) handleClusterNodes(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.can(w, r, "clusters", "get", name) {
		return
	}
	c, err := s.Clusters.Get(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	nodes, err := c.Nodes(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := []Node{}
	for _, n := range nodes {
		var drivers []string
		for d, info := range n.Drivers {
			if info != nil && info.Detected && info.Healthy {
				drivers = append(drivers, d)
			}
		}
		sort.Strings(drivers)
		out = append(out, Node{
			ID: n.ID, Name: n.Name, Datacenter: n.Datacenter, NodePool: n.NodePool, Status: n.Status,
			Eligible: n.SchedulingEligibility == "eligible", Drain: n.Drain, Address: n.Address,
			Version: n.Version, Drivers: drivers,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleBadge renders an SVG status badge (Argo CD's /api/badge).
func (s *Server) handleBadge(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	health, sync := "Unknown", "Unknown"
	if app, err := s.Store.GetApplication(r.Context(), name); err == nil {
		if app.Status.Health.Status != "" {
			health = string(app.Status.Health.Status)
		}
		if app.Status.Sync.Status != "" {
			sync = string(app.Status.Sync.Status)
		}
	}
	colors := map[string]string{
		"Healthy": "#18be94", "Progressing": "#0dadea", "Degraded": "#e96d76", "Suspended": "#766f94",
		"Missing": "#f4c030", "Unknown": "#8fa4b1", "Synced": "#18be94", "OutOfSync": "#f4c030",
	}
	lw, rw := textWidth(health)+20, textWidth(sync)+20
	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s: %s">
<rect width="%d" height="20" fill="%s"/><rect x="%d" width="%d" height="20" fill="%s"/>
<g fill="#fff" text-anchor="middle" font-family="Verdana,DejaVu Sans,sans-serif" font-size="11">
<text x="%d" y="14">%s</text><text x="%d" y="14">%s</text></g></svg>`,
		lw+rw, health, sync, lw, colors[health], lw, rw, colors[sync], lw/2, health, lw+rw/2, sync)
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(svg))
}

func textWidth(s string) int { return 7 * len(strings.TrimSpace(s)) }
