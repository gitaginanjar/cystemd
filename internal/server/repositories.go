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

// listRepos returns the connected repositories, those declared in the
// server configuration included (needs repositories/get).
func (s *Server) listRepos(r *http.Request) ([]*v1.Repository, error) {
	if err := s.authorize(r, "repositories", "get", "*"); err != nil {
		return nil, err
	}
	repos, err := s.Store.ListRepositories(r.Context())
	if err != nil {
		return nil, err
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
	return repos, nil
}

func (s *Server) handleListRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.listRepos(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": repos})
}

// createRepo connects a repository after testing the connection (force
// stores it even when the test fails). Secrets go to Vault or the store and
// are cleared from the returned value.
func (s *Server) createRepo(r *http.Request, req *v1.Repository, force bool) (*v1.Repository, error) {
	req.Repo = strings.TrimSpace(req.Repo)
	if req.Repo == "" {
		return nil, badRequest("repo is required")
	}
	if err := s.authorize(r, "repositories", "create", req.Repo); err != nil {
		return nil, err
	}
	creds := store.RepoCredentials{Username: req.Username, Password: req.Password, SSHPrivateKey: req.SSHPrivateKey}
	gc := &git.Credentials{Username: req.Username, Password: req.Password, SSHPrivateKey: []byte(req.SSHPrivateKey), Insecure: req.Insecure}
	now := time.Now().UTC().Truncate(time.Second)
	req.ConnectionState = v1.ConnectionState{Status: v1.ConnectionSuccessful, AttemptedAt: &now}
	if err := s.Git.TestConnection(r.Context(), req.Repo, gc); err != nil {
		if !force {
			s.audit(r, "repositories/create", req.Repo, "FAILED", err.Error())
			return nil, badRequest("unable to connect to repository: %v", err)
		}
		req.ConnectionState = v1.ConnectionState{Status: v1.ConnectionFailed, Message: err.Error(), AttemptedAt: &now}
	}
	if req.Type == "" {
		req.Type = "git"
	}
	req.HasCredentials = creds != (store.RepoCredentials{})
	if req.HasCredentials {
		if err := s.creds.save(r.Context(), req.Repo, creds); err != nil {
			return nil, err
		}
	}
	req.Password, req.SSHPrivateKey = "", ""
	if err := s.Store.PutRepository(r.Context(), req); err != nil {
		return nil, err
	}
	s.audit(r, "repositories/create", req.Repo, "ALLOWED", "")
	return req, nil
}

func (s *Server) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	var req v1.Repository
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	out, err := s.createRepo(r, &req, r.URL.Query().Get("force") == "true")
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteRepo disconnects a repository and removes its stored credentials.
func (s *Server) deleteRepo(r *http.Request, repo string) error {
	if err := s.authorize(r, "repositories", "delete", repo); err != nil {
		return err
	}
	if s.creds.staticRepo(repo) != nil {
		return badRequest("repository is declared in the server configuration")
	}
	if err := s.Store.DeleteRepository(r.Context(), repo); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	s.creds.remove(r.Context(), repo)
	s.audit(r, "repositories/delete", repo, "ALLOWED", "")
	return nil
}

func (s *Server) handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	if err := s.deleteRepo(r, r.URL.Query().Get("repo")); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// repoRefs lists a repository's branches and tags.
func (s *Server) repoRefs(r *http.Request, repo string) (branches, tags []string, err error) {
	if err := s.authorize(r, "repositories", "get", repo); err != nil {
		return nil, nil, err
	}
	return s.Git.Refs(r.Context(), repo)
}

func (s *Server) handleRepoRefs(w http.ResponseWriter, r *http.Request) {
	branches, tags, err := s.repoRefs(r, r.URL.Query().Get("repo"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": branches, "tags": tags})
}
