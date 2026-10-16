// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Contexts are saved servers and sessions, like kubectl and argocd keep:
// $XDG_CONFIG_HOME/cystemd/config.json (0600, it holds session tokens).
type cliConfig struct {
	Current  string                 `json:"current,omitempty"`
	Contexts map[string]*cliContext `json:"contexts,omitempty"`
}

type cliContext struct {
	Server   string `json:"server"`
	Token    string `json:"token,omitempty"`
	User     string `json:"user,omitempty"`
	CACert   string `json:"caCert,omitempty"`
	Insecure bool   `json:"insecure,omitempty"`
}

func configPath() (string, error) {
	if p := os.Getenv("CYSTEMD_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cystemd", "config.json"), nil
}

func loadConfig() (*cliConfig, error) {
	cfg := &cliConfig{Contexts: map[string]*cliContext{}}
	p, err := configPath()
	if err != nil {
		return cfg, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", p, err)
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]*cliContext{}
	}
	return cfg, nil
}

func (c *cliConfig) save() error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// context returns the named context, or the current one; never nil.
func (c *cliConfig) context(name string) *cliContext {
	if name == "" {
		name = c.Current
	}
	if ctx, ok := c.Contexts[name]; ok {
		return ctx
	}
	return &cliContext{}
}

func (c *cliConfig) names() []string {
	out := make([]string, 0, len(c.Contexts))
	for n := range c.Contexts {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
