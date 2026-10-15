// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	nomadagent "github.com/hashicorp/nomad/command/agent"
	structsconfig "github.com/hashicorp/nomad/nomad/structs/config"
)

func TestEmbeddedDefaults(t *testing.T) {
	cfg := &nomadagent.Config{
		Region:  "eu",
		DataDir: "/var/lib/nomad",
		TLSConfig: &structsconfig.TLSConfig{
			EnableHTTP: true, VerifyHTTPSClient: true,
			CAFile: "/etc/nomad/ca.pem", CertFile: "/etc/nomad/server.pem", KeyFile: "/etc/nomad/server-key.pem",
		},
		Consuls: []*structsconfig.ConsulConfig{{Addr: "127.0.0.1:8501", Token: "t"}},
	}
	d := embeddedDefaults(cfg, "0.0.0.0:4646")
	if d.NomadAddress != "https://127.0.0.1:4646" || d.DataDir != "/var/lib/nomad/cystemd" || d.NomadRegion != "eu" ||
		d.NomadCACert != "/etc/nomad/ca.pem" || d.NomadClientCert != "/etc/nomad/server.pem" || d.ConsulAddress != "127.0.0.1:8501" {
		t.Fatalf("defaults = %+v", d)
	}
	d = embeddedDefaults(&nomadagent.Config{DevMode: true, TLSConfig: &structsconfig.TLSConfig{}}, "[::]:4646")
	if d.NomadAddress != "http://[::1]:4646" || !strings.HasSuffix(d.DataDir, "cystemd-dev") || d.NomadClientCert != "" {
		t.Fatalf("dev defaults = %+v", d)
	}
	if !isTCP("127.0.0.1:4646") || isTCP("/run/nomad.sock") {
		t.Fatal("isTCP")
	}
	enabled := true
	if !controlPlaneEnabled(&nomadagent.Config{DevMode: true}) || controlPlaneEnabled(&nomadagent.Config{}) ||
		!controlPlaneEnabled(&nomadagent.Config{Cystemd: &nomadagent.CystemdConfig{ControlPlane: &nomadagent.CystemdControlPlaneConfig{Enabled: &enabled}}}) {
		t.Fatal("controlPlaneEnabled")
	}
}

func TestHCLogHandler(t *testing.T) {
	var buf bytes.Buffer
	l := hclog.New(&hclog.LoggerOptions{Output: &buf, Level: hclog.Info})
	s := slog.New(&hclogHandler{logger: l}).With("component", "api")
	s.Debug("hidden")
	s.WithGroup("req").Warn("slow", "ms", 1200)
	out := buf.String()
	if strings.Contains(out, "hidden") || !strings.Contains(out, "[WARN]") || !strings.Contains(out, "component=api") || !strings.Contains(out, "req.ms=1200") {
		t.Fatalf("log output: %q", out)
	}
}
