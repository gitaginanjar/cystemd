// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package git is the control plane's repository client (Argo CD's
// repo-server role): one local clone per repository, fetched on demand,
// checked out at the exact commit a render needs. Clones are serialised per
// repository; renders run on the checked-out tree under that lock.
package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Credentials authenticate to a repository.
type Credentials struct {
	Username      string
	Password      string
	SSHPrivateKey []byte
	// Insecure skips TLS and SSH host-key verification.
	Insecure bool
}

// CredentialsFunc returns the credentials of a repository URL (nil: none).
type CredentialsFunc func(ctx context.Context, repoURL string) (*Credentials, error)

// Client fetches repositories into a cache directory.
type Client struct {
	cacheDir       string
	credentials    CredentialsFunc
	knownHostsFile string
	logger         *slog.Logger
	fetchTimeout   time.Duration

	locks sync.Map // repo URL → *sync.Mutex
}

// Options configures a Client.
type Options struct {
	CacheDir string
	// Credentials resolves per-repository credentials.
	Credentials CredentialsFunc
	// KnownHostsFile verifies SSH host keys (accept-new: unknown hosts are
	// recorded, changed keys rejected). Empty disables verification.
	KnownHostsFile string
	FetchTimeout   time.Duration
	Logger         *slog.Logger
}

// NewClient returns a repository client.
func NewClient(o Options) *Client {
	if o.FetchTimeout <= 0 {
		o.FetchTimeout = 2 * time.Minute
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Credentials == nil {
		o.Credentials = func(context.Context, string) (*Credentials, error) { return nil, nil }
	}
	return &Client{
		cacheDir:       o.CacheDir,
		credentials:    o.Credentials,
		knownHostsFile: o.KnownHostsFile,
		logger:         o.Logger,
		fetchTimeout:   o.FetchTimeout,
	}
}

func (c *Client) lock(repoURL string) func() {
	m, _ := c.locks.LoadOrStore(NormalizeURL(repoURL), &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// NormalizeURL makes equivalent URLs compare equal (case, trailing slash
// and .git suffix).
func NormalizeURL(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")
	return strings.ToLower(u)
}

func (c *Client) repoDir(repoURL string) string {
	sum := sha256.Sum256([]byte(NormalizeURL(repoURL)))
	return filepath.Join(c.cacheDir, hex.EncodeToString(sum[:10]))
}

func (c *Client) auth(ctx context.Context, repoURL string) (transport.AuthMethod, bool, error) {
	creds, err := c.credentials(ctx, repoURL)
	if err != nil {
		return nil, false, fmt.Errorf("repository credentials: %w", err)
	}
	return authMethod(repoURL, creds, c.knownHostsFile)
}

// fullSHA matches a full commit hash.
var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// LsRemote resolves revision (branch, tag, HEAD or commit) to a commit hash
// without fetching objects.
func (c *Client) LsRemote(ctx context.Context, repoURL, revision string) (string, error) {
	if revision == "" {
		revision = "HEAD"
	}
	if fullSHA.MatchString(revision) {
		return revision, nil
	}
	refs, err := c.listRefs(ctx, repoURL)
	if err != nil {
		return "", err
	}
	hash, unpeeledTag, err := resolveRef(refs, revision)
	if err != nil || !unpeeledTag {
		return hash, err
	}
	// The server did not advertise the peeled commit of an annotated tag:
	// peel it from the local clone.
	return c.peel(ctx, repoURL, hash)
}

// peel resolves a tag object hash to the commit it (transitively) points to.
func (c *Client) peel(ctx context.Context, repoURL, hash string) (string, error) {
	unlock := c.lock(repoURL)
	defer unlock()
	repo, err := c.openOrClone(ctx, repoURL)
	if err != nil {
		return "", err
	}
	h := plumbing.NewHash(hash)
	if _, err := repo.TagObject(h); err != nil {
		if err := c.fetch(ctx, repo, repoURL); err != nil {
			return "", err
		}
	}
	for depth := 0; depth < 5; depth++ {
		tag, err := repo.TagObject(h)
		if err != nil {
			return h.String(), nil // not a tag object: a lightweight tag's commit
		}
		h = tag.Target
	}
	return "", fmt.Errorf("tag %s nests too deeply", hash)
}

func (c *Client) listRefs(ctx context.Context, repoURL string) ([]*plumbing.Reference, error) {
	auth, insecure, err := c.auth(ctx, repoURL)
	if err != nil {
		return nil, err
	}
	remote := gogit.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: "origin", URLs: []string{repoURL}})
	ctx, cancel := context.WithTimeout(ctx, c.fetchTimeout)
	defer cancel()
	refs, err := remote.ListContext(ctx, &gogit.ListOptions{Auth: auth, InsecureSkipTLS: insecure})
	if err != nil {
		return nil, fmt.Errorf("list refs of %s: %w", repoURL, err)
	}
	return refs, nil
}

// resolveRef picks revision out of refs: HEAD, then a branch, then a tag
// (annotated tags resolve to their peeled "^{}" entry when advertised; else
// unpeeledTag reports that the hash may be a tag object). Abbreviated commit
// hashes are rejected: they cannot be verified without fetching.
func resolveRef(refs []*plumbing.Reference, revision string) (hash string, unpeeledTag bool, err error) {
	byName := map[plumbing.ReferenceName]*plumbing.Reference{}
	for _, r := range refs {
		byName[r.Name()] = r
	}
	resolve := func(name plumbing.ReferenceName) (string, bool) {
		for depth := 0; depth < 5; depth++ {
			r, ok := byName[name]
			if !ok {
				return "", false
			}
			if r.Type() == plumbing.SymbolicReference {
				name = r.Target()
				continue
			}
			return r.Hash().String(), true
		}
		return "", false
	}
	if revision == "HEAD" {
		if h, ok := resolve(plumbing.HEAD); ok {
			return h, false, nil
		}
	}
	if h, ok := resolve(plumbing.NewBranchReferenceName(revision)); ok {
		return h, false, nil
	}
	tag := plumbing.NewTagReferenceName(revision)
	if h, ok := resolve(plumbing.ReferenceName(string(tag) + "^{}")); ok {
		return h, false, nil
	}
	if h, ok := resolve(tag); ok {
		return h, true, nil
	}
	if strings.HasPrefix(revision, "refs/") {
		if h, ok := resolve(plumbing.ReferenceName(revision)); ok {
			return h, strings.HasPrefix(revision, "refs/tags/"), nil
		}
	}
	return "", false, fmt.Errorf("revision %q not found in repository", revision)
}

// Refs lists the branches and tags of a repository (for the UI).
func (c *Client) Refs(ctx context.Context, repoURL string) (branches, tags []string, err error) {
	refs, err := c.listRefs(ctx, repoURL)
	if err != nil {
		return nil, nil, err
	}
	for _, r := range refs {
		switch {
		case r.Name().IsBranch():
			branches = append(branches, r.Name().Short())
		case r.Name().IsTag() && !strings.HasSuffix(string(r.Name()), "^{}"):
			tags = append(tags, r.Name().Short())
		}
	}
	sort.Strings(branches)
	sort.Strings(tags)
	return branches, tags, nil
}

// Checkout fetches the repository and checks out sha, then calls fn with the
// work tree while holding the repository lock. fn must not keep the path.
func (c *Client) Checkout(ctx context.Context, repoURL, sha string, fn func(dir string) error) error {
	unlock := c.lock(repoURL)
	defer unlock()

	repo, err := c.openOrClone(ctx, repoURL)
	if err != nil {
		return err
	}
	hash := plumbing.NewHash(sha)
	if _, err := repo.CommitObject(hash); err != nil {
		if err := c.fetch(ctx, repo, repoURL); err != nil {
			return err
		}
		if _, err := repo.CommitObject(hash); err != nil {
			return fmt.Errorf("commit %s not found in %s after fetch: %w", sha, repoURL, err)
		}
	}
	wt, err := repo.Worktree()
	if err != nil {
		return err
	}
	if err := wt.Checkout(&gogit.CheckoutOptions{Hash: hash, Force: true}); err != nil {
		return fmt.Errorf("checkout %s: %w", sha, err)
	}
	// Remove untracked files a previous checkout may have left.
	if err := wt.Clean(&gogit.CleanOptions{Dir: true}); err != nil {
		return fmt.Errorf("clean work tree: %w", err)
	}
	return fn(wt.Filesystem.Root())
}

// Commit returns the metadata of sha (fetching when needed).
func (c *Client) Commit(ctx context.Context, repoURL, sha string) (*CommitMetadata, error) {
	unlock := c.lock(repoURL)
	defer unlock()
	repo, err := c.openOrClone(ctx, repoURL)
	if err != nil {
		return nil, err
	}
	commit, err := repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		if ferr := c.fetch(ctx, repo, repoURL); ferr != nil {
			return nil, ferr
		}
		if commit, err = repo.CommitObject(plumbing.NewHash(sha)); err != nil {
			return nil, err
		}
	}
	return commitMetadata(repo, commit), nil
}

// CommitMetadata describes a commit (Argo CD's RevisionMetadata).
type CommitMetadata struct {
	Author  string    `json:"author"`
	Date    time.Time `json:"date"`
	Message string    `json:"message"`
	Tags    []string  `json:"tags,omitempty"`
}

func commitMetadata(repo *gogit.Repository, commit *object.Commit) *CommitMetadata {
	md := &CommitMetadata{
		Author:  fmt.Sprintf("%s <%s>", commit.Author.Name, commit.Author.Email),
		Date:    commit.Author.When.UTC(),
		Message: strings.TrimSpace(commit.Message),
	}
	if iter, err := repo.Tags(); err == nil {
		_ = iter.ForEach(func(r *plumbing.Reference) error {
			target := r.Hash()
			if tag, err := repo.TagObject(target); err == nil {
				target = tag.Target
			}
			if target == commit.Hash {
				md.Tags = append(md.Tags, r.Name().Short())
			}
			return nil
		})
	}
	return md
}

func (c *Client) openOrClone(ctx context.Context, repoURL string) (*gogit.Repository, error) {
	dir := c.repoDir(repoURL)
	if repo, err := gogit.PlainOpen(dir); err == nil {
		return repo, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	auth, insecure, err := c.auth(ctx, repoURL)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, c.fetchTimeout)
	defer cancel()
	c.logger.Info("cloning repository", "repo", repoURL)
	repo, err := gogit.PlainCloneContext(cctx, dir, false, &gogit.CloneOptions{
		URL: repoURL, Auth: auth, InsecureSkipTLS: insecure, Tags: gogit.AllTags, NoCheckout: true,
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("clone %s: %w", repoURL, err)
	}
	return repo, nil
}

func (c *Client) fetch(ctx context.Context, repo *gogit.Repository, repoURL string) error {
	auth, insecure, err := c.auth(ctx, repoURL)
	if err != nil {
		return err
	}
	fctx, cancel := context.WithTimeout(ctx, c.fetchTimeout)
	defer cancel()
	err = repo.FetchContext(fctx, &gogit.FetchOptions{
		RemoteName: "origin",
		Auth:       auth,
		Force:      true,
		Tags:       gogit.AllTags,
		RefSpecs: []config.RefSpec{
			"+refs/heads/*:refs/remotes/origin/*",
			"+refs/tags/*:refs/tags/*",
		},
		InsecureSkipTLS: insecure,
	})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return fmt.Errorf("fetch %s: %w", repoURL, err)
	}
	return nil
}

// TestConnection checks that the repository is reachable with creds.
func (c *Client) TestConnection(ctx context.Context, repoURL string, creds *Credentials) error {
	auth, insecure, err := authMethod(repoURL, creds, c.knownHostsFile)
	if err != nil {
		return err
	}
	remote := gogit.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: "origin", URLs: []string{repoURL}})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = remote.ListContext(ctx, &gogit.ListOptions{Auth: auth, InsecureSkipTLS: insecure})
	return err
}
