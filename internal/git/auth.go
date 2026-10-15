// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package git

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// sshDialTimeout bounds the SSH dial and handshake, which go-git's clone
// context does not cover.
const sshDialTimeout = 30 * time.Second

// isSSH reports whether url uses SSH (ssh:// or scp-like user@host:path).
func isSSH(url string) bool {
	if strings.HasPrefix(url, "ssh://") {
		return true
	}
	if strings.Contains(url, "://") {
		return false
	}
	at := strings.Index(url, "@")
	colon := strings.Index(url, ":")
	return at >= 0 && colon > at
}

// authMethod builds the transport auth for url. It returns insecure=true
// when TLS verification must be skipped (HTTPS with Insecure).
func authMethod(url string, creds *Credentials, knownHostsFile string) (transport.AuthMethod, bool, error) {
	if creds == nil {
		creds = &Credentials{}
	}
	if !isSSH(url) {
		if creds.Username == "" && creds.Password == "" {
			return nil, creds.Insecure, nil
		}
		user := creds.Username
		if user == "" {
			user = "git" // token-only HTTPS auth (GitHub, Bitbucket app passwords)
		}
		return &http.BasicAuth{Username: user, Password: creds.Password}, creds.Insecure, nil
	}
	if len(creds.SSHPrivateKey) == 0 {
		return nil, false, errors.New("an SSH repository needs an SSH private key")
	}
	user := "git"
	if u, _, ok := strings.Cut(strings.TrimPrefix(url, "ssh://"), "@"); ok && !strings.Contains(u, "/") {
		user = u
	}
	keys, err := gitssh.NewPublicKeys(user, creds.SSHPrivateKey, "")
	if err != nil {
		return nil, false, fmt.Errorf("invalid SSH private key: %w", err)
	}
	switch {
	case creds.Insecure:
		keys.HostKeyCallback = cryptossh.InsecureIgnoreHostKey()
	case knownHostsFile != "":
		cb, err := acceptNewHostKeyCallback(knownHostsFile)
		if err != nil {
			return nil, false, err
		}
		keys.HostKeyCallback = cb
	default:
		return nil, false, errors.New("SSH host-key verification needs a known_hosts file (server git.known_hosts_file) or an insecure repository")
	}
	return &timeoutAuth{PublicKeys: keys, timeout: sshDialTimeout}, false, nil
}

// timeoutAuth wraps PublicKeys so its ClientConfig carries a dial timeout.
// Never swap it for gitssh.NewClient with a ClientConfig{Timeout}: go-git's
// config override would wipe User, Auth and HostKeyCallback.
type timeoutAuth struct {
	*gitssh.PublicKeys
	timeout time.Duration
}

func (a *timeoutAuth) ClientConfig() (*cryptossh.ClientConfig, error) {
	cfg, err := a.PublicKeys.ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.Timeout = a.timeout
	return cfg, nil
}

// knownHostsMu makes the accept-new check → append → rebuild atomic across
// concurrent clones.
var knownHostsMu sync.Mutex

// acceptNewHostKeyCallback implements StrictHostKeyChecking=accept-new: a
// known host must present its recorded key (a changed key is rejected,
// never overwritten); an unknown host's key is appended and accepted.
func acceptNewHostKeyCallback(path string) (cryptossh.HostKeyCallback, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_ = f.Close()
	} else {
		return nil, err
	}
	check, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts %s: %w", path, err)
	}
	return func(hostname string, remote net.Addr, key cryptossh.PublicKey) error {
		knownHostsMu.Lock()
		defer knownHostsMu.Unlock()
		verr := check(hostname, remote, key)
		if verr == nil {
			return nil
		}
		var kerr *knownhosts.KeyError
		if !errors.As(verr, &kerr) || len(kerr.Want) != 0 {
			return verr
		}
		line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("record host key for %s: %w", hostname, err)
		}
		_, werr := fmt.Fprintln(f, line)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			return fmt.Errorf("record host key for %s: %v", hostname, errors.Join(werr, cerr))
		}
		if fresh, err := knownhosts.New(path); err == nil {
			check = fresh
		}
		return nil
	}, nil
}
