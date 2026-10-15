// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/gitaginanjar/cystemd/internal/config"
	"github.com/gitaginanjar/cystemd/internal/git"
	"github.com/gitaginanjar/cystemd/internal/store"
	"github.com/gitaginanjar/cystemd/internal/vault"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Credentials resolves and stores repository credentials. Secret material
// goes to Vault (KV v2, through Vault Agent) when Vault is enabled, and to
// the store otherwise (protect the Consul prefix with ACLs then).
type Credentials struct {
	Store  *store.Store
	Vault  *vault.Client
	Config *config.Config
}

func (c *Credentials) vaultPath(repoURL string) string {
	return path.Join(c.Config.Vault.RepoCredsPath, store.RepoID(repoURL))
}

// staticRepo returns the config-file repository block matching repoURL.
func (c *Credentials) staticRepo(repoURL string) *config.RepositoryBlock {
	for _, rb := range c.Config.Repositories {
		if git.NormalizeURL(rb.URL) == git.NormalizeURL(repoURL) {
			return rb
		}
	}
	return nil
}

// Resolve implements git.CredentialsFunc.
func (c *Credentials) Resolve(ctx context.Context, repoURL string) (*git.Credentials, error) {
	if rb := c.staticRepo(repoURL); rb != nil {
		creds := &git.Credentials{Username: rb.Username, Insecure: rb.Insecure}
		if rb.PasswordFile != "" {
			pw, err := config.Secret("", rb.PasswordFile)
			if err != nil {
				return nil, err
			}
			creds.Password = pw
		}
		if rb.SSHPrivateKeyFile != "" {
			key, err := os.ReadFile(rb.SSHPrivateKeyFile)
			if err != nil {
				return nil, err
			}
			creds.SSHPrivateKey = key
		}
		return creds, nil
	}
	repo, err := c.Store.GetRepository(ctx, repoURL)
	if err == nil && repo.HasCredentials {
		stored, err := c.load(ctx, repoURL)
		if err != nil {
			return nil, err
		}
		return &git.Credentials{Username: stored.Username, Password: stored.Password,
			SSHPrivateKey: []byte(stored.SSHPrivateKey), Insecure: repo.Insecure}, nil
	}
	insecure := err == nil && repo.Insecure
	// SSH without stored credentials: the server-wide default key (the v4
	// GIT_SSH_KEY_PRIVATE setting).
	if c.Config.Git.SSHPrivateKeyFile != "" && git.IsSSH(repoURL) {
		key, err := os.ReadFile(c.Config.Git.SSHPrivateKeyFile)
		if err != nil {
			return nil, err
		}
		return &git.Credentials{SSHPrivateKey: key, Insecure: insecure}, nil
	}
	if insecure {
		return &git.Credentials{Insecure: true}, nil
	}
	return nil, nil
}

func (c *Credentials) load(ctx context.Context, repoURL string) (*store.RepoCredentials, error) {
	if c.Vault != nil {
		data, err := c.Vault.Read(ctx, c.vaultPath(repoURL))
		if err != nil {
			return nil, fmt.Errorf("read repository credentials from Vault: %w", err)
		}
		str := func(k string) string { s, _ := data[k].(string); return s }
		return &store.RepoCredentials{Username: str("username"), Password: str("password"), SSHPrivateKey: str("sshPrivateKey")}, nil
	}
	return c.Store.GetRepoCredentials(ctx, repoURL)
}

func (c *Credentials) save(ctx context.Context, repoURL string, creds store.RepoCredentials) error {
	if c.Vault != nil {
		return c.Vault.Write(ctx, c.vaultPath(repoURL), map[string]any{
			"username": creds.Username, "password": creds.Password, "sshPrivateKey": creds.SSHPrivateKey,
		})
	}
	return c.Store.PutRepoCredentials(ctx, repoURL, creds)
}

func (c *Credentials) remove(ctx context.Context, repoURL string) {
	if c.Vault != nil {
		_ = c.Vault.Delete(ctx, c.vaultPath(repoURL))
	}
}

func (s *Server) handleListRepos(w http.ResponseWriter, r *http.Request) {
	if !s.can(w, r, "repositories", "get", "*") {
		return
	}
	repos, err := s.Store.ListRepositories(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	seen := map[string]bool{}
	for _, rp := range repos {
		seen[git.NormalizeURL(rp.Repo)] = true
	}
	for _, rb := range s.Config.Repositories {
		if seen[git.NormalizeURL(rb.URL)] {
			continue
		}
		repos = append(repos, &v1.Repository{Repo: rb.URL, Name: rb.Name, Type: "git", Insecure: rb.Insecure,
			HasCredentials:  rb.SSHPrivateKeyFile != "" || rb.PasswordFile != "",
			ConnectionState: v1.ConnectionState{Status: v1.ConnectionUnknown, Message: "declared in the server configuration"}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": repos})
}

func (s *Server) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	var req v1.Repository
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	req.Repo = strings.TrimSpace(req.Repo)
	if req.Repo == "" {
		writeError(w, http.StatusBadRequest, "repo is required")
		return
	}
	if !s.can(w, r, "repositories", "create", req.Repo) {
		return
	}
	creds := store.RepoCredentials{Username: req.Username, Password: req.Password, SSHPrivateKey: req.SSHPrivateKey}
	gc := &git.Credentials{Username: req.Username, Password: req.Password, SSHPrivateKey: []byte(req.SSHPrivateKey), Insecure: req.Insecure}
	now := time.Now().UTC().Truncate(time.Second)
	req.ConnectionState = v1.ConnectionState{Status: v1.ConnectionSuccessful, AttemptedAt: &now}
	if err := s.Git.TestConnection(r.Context(), req.Repo, gc); err != nil {
		if r.URL.Query().Get("force") != "true" {
			s.audit(r, "repositories/create", req.Repo, "FAILED", err.Error())
			writeError(w, http.StatusBadRequest, "unable to connect to repository: "+err.Error())
			return
		}
		req.ConnectionState = v1.ConnectionState{Status: v1.ConnectionFailed, Message: err.Error(), AttemptedAt: &now}
	}
	if req.Type == "" {
		req.Type = "git"
	}
	req.HasCredentials = creds != (store.RepoCredentials{})
	if req.HasCredentials {
		if err := s.creds.save(r.Context(), req.Repo, creds); err != nil {
			s.fail(w, err)
			return
		}
	}
	if err := s.Store.PutRepository(r.Context(), &req); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "repositories/create", req.Repo, "ALLOWED", "")
	req.Password, req.SSHPrivateKey = "", ""
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if !s.can(w, r, "repositories", "delete", repo) {
		return
	}
	if s.creds.staticRepo(repo) != nil {
		writeError(w, http.StatusBadRequest, "repository is declared in the server configuration")
		return
	}
	if err := s.Store.DeleteRepository(r.Context(), repo); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	s.creds.remove(r.Context(), repo)
	s.audit(r, "repositories/delete", repo, "ALLOWED", "")
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) handleRepoRefs(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if !s.can(w, r, "repositories", "get", repo) {
		return
	}
	branches, tags, err := s.Git.Refs(r.Context(), repo)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": branches, "tags": tags})
}
