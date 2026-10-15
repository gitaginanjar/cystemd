// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gitaginanjar/cystemd/internal/git"
	"github.com/gitaginanjar/cystemd/internal/nomad"
	"github.com/gitaginanjar/cystemd/internal/render"
	"github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Repos resolves revisions and renders manifests (Argo CD's repo-server).
type Repos interface {
	// Resolve turns a revision into a commit hash.
	Resolve(ctx context.Context, repoURL, revision string) (string, error)
	// Manifests renders source at sha for app's destination cluster.
	Manifests(ctx context.Context, app *v1alpha1.Application, source v1alpha1.ApplicationSource, sha string, noCache bool) (*render.Manifests, error)
	// Commit returns commit metadata.
	Commit(ctx context.Context, repoURL, sha string) (*git.CommitMetadata, error)
}

// RepoService implements Repos with the git client, a renderer and a
// manifest cache keyed by commit and source spec (commits are immutable).
type RepoService struct {
	Git      *git.Client
	Clusters *nomad.Clusters
	Options  render.Options

	mu    sync.Mutex
	cache map[string]cachedManifests

	// lsRemote caches revision resolution briefly, so a burst of refreshes
	// (UI, events) does not hammer the Git server.
	revMu    sync.Mutex
	revCache map[string]cachedRevision
}

type cachedManifests struct {
	m  *render.Manifests
	at time.Time
}

type cachedRevision struct {
	sha string
	at  time.Time
}

const (
	manifestCacheTTL  = time.Hour
	manifestCacheSize = 2048
	revisionCacheTTL  = 10 * time.Second
)

func (r *RepoService) Resolve(ctx context.Context, repoURL, revision string) (string, error) {
	key := git.NormalizeURL(repoURL) + "@" + revision
	r.revMu.Lock()
	if r.revCache == nil {
		r.revCache = map[string]cachedRevision{}
	}
	if c, ok := r.revCache[key]; ok && time.Since(c.at) < revisionCacheTTL {
		r.revMu.Unlock()
		return c.sha, nil
	}
	r.revMu.Unlock()
	sha, err := r.Git.LsRemote(ctx, repoURL, revision)
	if err != nil {
		return "", err
	}
	r.revMu.Lock()
	r.revCache[key] = cachedRevision{sha: sha, at: time.Now()}
	r.revMu.Unlock()
	return sha, nil
}

// InvalidateRevision drops cached revision resolutions of repoURL (hard
// refresh, webhook).
func (r *RepoService) InvalidateRevision(repoURL string) {
	prefix := git.NormalizeURL(repoURL) + "@"
	r.revMu.Lock()
	defer r.revMu.Unlock()
	for k := range r.revCache {
		if strings.HasPrefix(k, prefix) {
			delete(r.revCache, k)
		}
	}
}

// InvalidateRevisionBefore drops the cached resolutions of repoURL made
// before t. A refresh wants Git's current state; refreshes arriving
// together (a webhook refreshing every application of a repository)
// still share one lookup.
func (r *RepoService) InvalidateRevisionBefore(repoURL string, t time.Time) {
	prefix := git.NormalizeURL(repoURL) + "@"
	r.revMu.Lock()
	defer r.revMu.Unlock()
	for k, c := range r.revCache {
		if strings.HasPrefix(k, prefix) && c.at.Before(t) {
			delete(r.revCache, k)
		}
	}
}

func manifestKey(app *v1alpha1.Application, source v1alpha1.ApplicationSource, sha string) string {
	b, _ := json.Marshal(struct {
		Name string
		Src  v1alpha1.ApplicationSource
		Dst  v1alpha1.ApplicationDestination
		SHA  string
	}{app.Metadata.Name, source, app.Spec.Destination, sha})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (r *RepoService) Manifests(ctx context.Context, app *v1alpha1.Application, source v1alpha1.ApplicationSource, sha string, noCache bool) (*render.Manifests, error) {
	key := manifestKey(app, source, sha)
	if !noCache {
		r.mu.Lock()
		if c, ok := r.cache[key]; ok && time.Since(c.at) < manifestCacheTTL {
			r.mu.Unlock()
			return c.m, nil
		}
		r.mu.Unlock()
	}
	cluster, err := r.Clusters.Get(app.Spec.Destination.Name)
	if err != nil {
		return nil, err
	}
	renderer := &render.Renderer{Parse: cluster.ParseHCL, Opts: r.Options}
	renderApp := app.DeepCopy()
	renderApp.Spec.Source = source
	var out *render.Manifests
	err = r.Git.Checkout(ctx, source.RepoURL, sha, func(root string) error {
		dir := filepath.Join(root, filepath.Clean("/"+source.Path))
		m, err := renderer.Render(ctx, renderApp, dir)
		out = m
		return err
	})
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache == nil {
		r.cache = map[string]cachedManifests{}
	}
	if len(r.cache) >= manifestCacheSize {
		for k, c := range r.cache {
			if time.Since(c.at) > manifestCacheTTL/2 {
				delete(r.cache, k)
			}
		}
		if len(r.cache) >= manifestCacheSize {
			r.cache = map[string]cachedManifests{}
		}
	}
	r.cache[key] = cachedManifests{m: out, at: time.Now()}
	return out, nil
}

func (r *RepoService) Commit(ctx context.Context, repoURL, sha string) (*git.CommitMetadata, error) {
	return r.Git.Commit(ctx, repoURL, sha)
}
