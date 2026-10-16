// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/go-hclog"
	nomadagent "github.com/hashicorp/nomad/command/agent"

	"github.com/gitaginanjar/cystemd/internal/config"
	"github.com/gitaginanjar/cystemd/internal/controlplane"
	"github.com/gitaginanjar/cystemd/internal/server"
	"github.com/gitaginanjar/cystemd/ui"
)

// embedded runs the CYSTEMD control plane inside the agent, through the
// fork's hooks: its API under /api/ next to Nomad's /v1/ on every HTTP
// listener, and its web UI as the agent's /ui/. It starts on agents whose
// configuration enables it (`cystemd { control_plane { enabled = true } }`)
// and in -dev mode.
type embedded struct {
	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc

	cp  atomic.Pointer[controlplane.ControlPlane]
	api atomic.Pointer[http.Handler]
	ui  atomic.Pointer[http.Handler]
	// state is shown on /ui/ while the control plane is not serving.
	state atomic.Pointer[string]
}

var controlPlaneExt = &embedded{}

func (e *embedded) setState(s string) { e.state.Store(&s) }

// RegisterHTTP implements nomadagent.HTTPExtension.
func (e *embedded) RegisterHTTP(mux *http.ServeMux, agent nomadagent.RPCer, addr string, logger hclog.Logger) {
	mux.Handle("/api/", http.HandlerFunc(e.serveAPI))

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started || !isTCP(addr) {
		return
	}
	e.started = true
	cfg := agent.GetConfig()
	if !controlPlaneEnabled(cfg) {
		e.setState("The CYSTEMD control plane is not enabled on this agent. It runs on the server agents whose configuration has cystemd { control_plane { enabled = true } }: open the UI there.")
		return
	}
	e.setState("The CYSTEMD control plane is starting…")
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	go e.start(ctx, cfg, addr, logger.Named("cystemd"))
}

// Shutdown implements nomadagent.HTTPExtension.
func (e *embedded) Shutdown() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel != nil {
		e.cancel()
	}
	if cp := e.cp.Load(); cp != nil {
		cp.Close()
	}
}

func controlPlaneEnabled(cfg *nomadagent.Config) bool {
	if c := cfg.Cystemd; c != nil && c.ControlPlane != nil && c.ControlPlane.Enabled != nil {
		return *c.ControlPlane.Enabled
	}
	return cfg.DevMode
}

func isTCP(addr string) bool {
	_, _, err := net.SplitHostPort(addr)
	return err == nil && !strings.HasPrefix(addr, "/")
}

func (e *embedded) start(ctx context.Context, cfg *nomadagent.Config, addr string, logger hclog.Logger) {
	cpCfg, err := controlPlaneConfig(cfg, addr)
	if err != nil {
		e.fail(logger, err)
		return
	}
	slogger := slog.New(&hclogHandler{logger: logger})
	cp, err := controlplane.New(ctx, cpCfg, controlplane.Options{
		Logger: slogger,
		Build:  server.BuildInfo{Version: cystemdVersion, GitCommit: cystemdCommit, Mode: "agent"},
	})
	if err != nil {
		e.fail(logger, err)
		return
	}
	api, web := cp.Server.APIHandler(), cp.Server.UIHandler()
	e.api.Store(&api)
	e.ui.Store(&web)
	e.cp.Store(cp)
	logger.Info("control plane started", "nomad", cpCfg.Nomad.Address, "data_dir", cpCfg.DataDir,
		"consul", cpCfg.ConsulEnabled(), "vault", cpCfg.VaultEnabled())
	cp.Run(ctx)
}

func (e *embedded) fail(logger hclog.Logger, err error) {
	logger.Error("control plane failed to start", "error", err)
	e.setState("The CYSTEMD control plane failed to start: " + err.Error())
}

// controlPlaneConfig reads the control-plane file and fills what it leaves
// unset from the agent's own configuration.
func controlPlaneConfig(cfg *nomadagent.Config, addr string) (*config.Config, error) {
	var src []byte
	file := ""
	if c := cfg.Cystemd; c != nil && c.ControlPlane != nil && c.ControlPlane.ConfigFile != "" {
		file = c.ControlPlane.ConfigFile
		var err error
		if src, err = os.ReadFile(file); err != nil {
			return nil, fmt.Errorf("control plane configuration: %w", err)
		}
	}
	return config.ParseEmbedded(src, file, embeddedDefaults(cfg, addr))
}

func embeddedDefaults(cfg *nomadagent.Config, addr string) *config.EmbeddedDefaults {
	d := &config.EmbeddedDefaults{NomadRegion: cfg.Region}
	if cfg.DataDir != "" {
		d.DataDir = filepath.Join(cfg.DataDir, "cystemd")
	} else {
		// -dev agents have no data dir.
		d.DataDir = filepath.Join(os.TempDir(), "cystemd-dev")
	}
	host, port, _ := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
		if ip.To4() == nil {
			host = "::1"
		}
	}
	scheme := "http"
	if t := cfg.TLSConfig; t != nil && t.EnableHTTP {
		scheme = "https"
		d.NomadCACert = t.CAFile
		if t.VerifyHTTPSClient {
			d.NomadClientCert, d.NomadClientKey = t.CertFile, t.KeyFile
		}
	}
	d.NomadAddress = scheme + "://" + net.JoinHostPort(host, port)
	if len(cfg.Consuls) > 0 && cfg.Consuls[0] != nil {
		d.ConsulAddress, d.ConsulToken = cfg.Consuls[0].Addr, cfg.Consuls[0].Token
	}
	return d
}

func (e *embedded) serveAPI(w http.ResponseWriter, r *http.Request) {
	if h := e.api.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	msg := "control plane not running"
	if s := e.state.Load(); s != nil {
		msg = *s
	}
	fmt.Fprintf(w, `{"error":%q,"code":503,"message":%q}`+"\n", msg, msg)
}

// serveUI is the agent's /ui/: the control plane's web UI, or a status
// page while it is not serving.
func (e *embedded) serveUI(w http.ResponseWriter, r *http.Request) {
	if h := e.ui.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, ui.Prefix) {
		ui.Handler().ServeHTTP(w, r)
		return
	}
	msg := "The CYSTEMD control plane is starting…"
	if s := e.state.Load(); s != nil {
		msg = *s
	}
	refresh := ""
	if strings.HasSuffix(msg, "starting…") {
		refresh = `<meta http-equiv="refresh" content="2">`
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprintf(w, `<!DOCTYPE html><html lang="en" data-theme="light"><head><meta charset="utf-8">%s<title>CYSTEMD</title>
<link rel="stylesheet" href="%s"></head><body><div class="content"><div class="empty white-box">
<img src="%s" width="96" height="96" alt=""><div class="empty__title">CYSTEMD</div><div class="empty__text">%s</div>
<a class="btn btn--outline" href="/v1/agent/self">Nomad agent API</a></div></div></body></html>`,
		refresh, ui.Asset("css/cystemd.css"), ui.Asset("img/emblem.png"), html.EscapeString(msg))
}

// hclogHandler sends the control plane's slog records to the agent's
// logger, so they land in the agent's log output.
type hclogHandler struct {
	logger hclog.Logger
	attrs  []any
	group  string
}

func (h *hclogHandler) Enabled(_ context.Context, l slog.Level) bool {
	return h.logger.IsTrace() || hclogLevel(l) >= h.logger.GetLevel()
}

func (h *hclogHandler) Handle(_ context.Context, r slog.Record) error {
	args := append([]any(nil), h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		args = append(args, h.key(a.Key), a.Value.Any())
		return true
	})
	h.logger.Log(hclogLevel(r.Level), r.Message, args...)
	return nil
}

func (h *hclogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *h
	n.attrs = append([]any(nil), h.attrs...)
	for _, a := range attrs {
		n.attrs = append(n.attrs, h.key(a.Key), a.Value.Any())
	}
	return &n
}

func (h *hclogHandler) WithGroup(name string) slog.Handler {
	n := *h
	if n.group != "" {
		name = n.group + "." + name
	}
	n.group = name
	return &n
}

func (h *hclogHandler) key(k string) string {
	if h.group == "" {
		return k
	}
	return h.group + "." + k
}

func hclogLevel(l slog.Level) hclog.Level {
	switch {
	case l >= slog.LevelError:
		return hclog.Error
	case l >= slog.LevelWarn:
		return hclog.Warn
	case l >= slog.LevelInfo:
		return hclog.Info
	default:
		return hclog.Debug
	}
}
