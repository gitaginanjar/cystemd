// Copyright IBM Corp. 2015, 2026
// Copyright 2026 G.G (https://github.com/gitaginanjar) — CYSTEMD fork changes
// SPDX-License-Identifier: BUSL-1.1

// CYSTEMD fork: extension points of the Nomad agent.
//
// This file is not part of upstream Nomad. It is added by the CYSTEMD fork
// overlay (fork/nomad/overlay) and only declares exported hook points; every
// CYSTEMD feature (GitOps control plane, web UI, systemd driver) lives outside
// the Nomad tree and plugs in through these hooks from the cystemd binary's
// main package. Keeping the in-tree surface this small keeps the fork
// rebaseable onto new Nomad releases (see docs/FORK.md).

package agent

import (
	"net/http"
	"sync"

	"github.com/hashicorp/go-hclog"
)

// CystemdConfig is the `cystemd { ... }` block of the agent configuration.
//
//	cystemd {
//	  control_plane {
//	    enabled     = true
//	    config_file = "/etc/cystemd/server.hcl"
//	  }
//	}
type CystemdConfig struct {
	// ControlPlane runs the CYSTEMD GitOps control plane (API, controller and
	// web UI backend) inside this agent.
	ControlPlane *CystemdControlPlaneConfig `hcl:"control_plane"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys.
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// CystemdControlPlaneConfig configures the embedded control plane.
type CystemdControlPlaneConfig struct {
	// Enabled starts the control plane in this agent. Nil means "unset":
	// the control plane then runs only in -dev mode.
	Enabled *bool `hcl:"enabled"`

	// ConfigFile is the control-plane configuration (HCL), see
	// deploy/agent/server.hcl in the CYSTEMD repository.
	ConfigFile string `hcl:"config_file"`

	// ExtraKeysHCL is used by hcl to surface unexpected keys.
	ExtraKeysHCL []string `hcl:",unusedKeys" json:"-"`
}

// Copy returns a deep copy of c.
func (c *CystemdConfig) Copy() *CystemdConfig {
	if c == nil {
		return nil
	}
	nc := *c
	nc.ExtraKeysHCL = nil
	if c.ControlPlane != nil {
		cp := *c.ControlPlane
		cp.ExtraKeysHCL = nil
		if c.ControlPlane.Enabled != nil {
			cp.Enabled = boolPtr(*c.ControlPlane.Enabled)
		}
		nc.ControlPlane = &cp
	}
	return &nc
}

// Merge returns c overlaid with every field b sets.
func (c *CystemdConfig) Merge(b *CystemdConfig) *CystemdConfig {
	result := c.Copy()
	if result == nil {
		result = &CystemdConfig{}
	}
	if b == nil || b.ControlPlane == nil {
		return result
	}
	if result.ControlPlane == nil {
		result.ControlPlane = &CystemdControlPlaneConfig{}
	}
	if b.ControlPlane.Enabled != nil {
		result.ControlPlane.Enabled = boolPtr(*b.ControlPlane.Enabled)
	}
	if b.ControlPlane.ConfigFile != "" {
		result.ControlPlane.ConfigFile = b.ControlPlane.ConfigFile
	}
	return result
}

func boolPtr(b bool) *bool { return &b }

// HTTPExtension mounts additional handlers on the agent's HTTP listeners.
type HTTPExtension interface {
	// RegisterHTTP is called once per public HTTP listener, after Nomad
	// registered its own handlers on mux. agent exposes the agent
	// configuration and its server/client; addr is the listener address.
	RegisterHTTP(mux *http.ServeMux, agent RPCer, addr string, logger hclog.Logger)

	// Shutdown is called once when the agent shuts down.
	Shutdown()
}

var (
	extensionsMu   sync.Mutex
	httpExtensions []HTTPExtension

	// UIFileSystem, when non-nil, replaces the Nomad web UI served under
	// /ui/. Unknown paths fall back to index.html (single-page application).
	UIFileSystem http.FileSystem
)

// RegisterHTTPExtension adds e to the extensions mounted on every HTTP
// listener. Call it before the agent starts (from an init or main function).
func RegisterHTTPExtension(e HTTPExtension) {
	extensionsMu.Lock()
	defer extensionsMu.Unlock()
	httpExtensions = append(httpExtensions, e)
}

func registeredHTTPExtensions() []HTTPExtension {
	extensionsMu.Lock()
	defer extensionsMu.Unlock()
	return append([]HTTPExtension(nil), httpExtensions...)
}

// registerHTTPExtensions mounts every registered extension on s.
func registerHTTPExtensions(s *HTTPServer) {
	for _, e := range registeredHTTPExtensions() {
		e.RegisterHTTP(s.mux, s.agent, s.Addr, s.logger)
	}
}

// shutdownHTTPExtensions stops every registered extension.
func shutdownHTTPExtensions() {
	for _, e := range registeredHTTPExtensions() {
		e.Shutdown()
	}
}

// spaFileSystem serves index.html for paths that do not exist, so client-side
// routes (/ui/applications/foo) survive a page reload.
type spaFileSystem struct{ fs http.FileSystem }

func (s spaFileSystem) Open(name string) (http.File, error) {
	f, err := s.fs.Open(name)
	if err != nil {
		return s.fs.Open("/index.html")
	}
	return f, nil
}
