// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/gitaginanjar/cystemd/pkg/client"
)

// ssoTimeout bounds how long login -sso waits for the browser.
const ssoTimeout = 5 * time.Minute

type ssoAnswer struct {
	code, state, iss, err string
}

// ssoLogin logs in through single sign-on, as argocd login --sso does: the
// identity provider sends the browser back to a listener on localhost,
// whose answer the server completes into a session. It returns the session
// token and the user name.
func (c *loginCommand) ssoLogin(ctx context.Context, cl *client.Client, method string, port int) (string, string, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", "", fmt.Errorf("listen for the SSO callback: %w", err)
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://localhost:%d/auth/callback", ln.Addr().(*net.TCPAddr).Port)
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	nonce := hex.EncodeToString(b)
	method, authURL, err := cl.SSOAuthURL(ctx, method, redirect, nonce)
	if err != nil {
		return "", "", err
	}
	u, err := url.Parse(authURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return "", "", fmt.Errorf("the server returned an invalid auth URL %q", authURL)
	}
	state := u.Query().Get("state")

	answers := make(chan ssoAnswer, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		a := ssoAnswer{code: q.Get("code"), state: q.Get("state"), iss: q.Get("iss"), err: q.Get("error_description")}
		if a.err == "" {
			a.err = q.Get("error")
		}
		if a.err == "" && state != "" && a.state != state {
			http.Error(w, "This answer belongs to another sign-in.", http.StatusBadRequest)
			return
		}
		msg := "Signed in. You can close this window and go back to the terminal."
		if a.err != "" {
			msg = "The sign-in failed: " + a.err
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><title>CYSTEMD</title><p style=\"font:16px sans-serif;margin:40px\">%s</p>", html.EscapeString(msg))
		select {
		case answers <- a:
		default:
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	c.ui.Output(fmt.Sprintf("Log in via %s in your browser: %s", method, authURL))
	open := c.opts.OpenBrowser
	if open == nil {
		open = openBrowser
	}
	if err := open(authURL); err != nil {
		c.ui.Warn("Could not open a browser (" + err.Error() + "); open the URL above.")
	}

	var a ssoAnswer
	select {
	case a = <-answers:
	case <-ctx.Done():
		return "", "", ctx.Err()
	case <-time.After(ssoTimeout):
		return "", "", errors.New("timed out waiting for the browser to come back")
	}
	if a.err != "" {
		return "", "", fmt.Errorf("the identity provider refused the sign-in: %s", a.err)
	}
	token, err := cl.LoginSSO(ctx, client.SSOCallback{AuthMethod: method, ClientNonce: nonce, State: a.state, Code: a.code,
		Iss: a.iss, RedirectURI: redirect})
	if err != nil {
		return "", "", err
	}
	cl.Token = token
	user := "sso"
	if info, err := cl.UserInfo(ctx); err == nil {
		user = info.Username
	}
	return token, user, nil
}

// openBrowser opens an http(s) URL in the desktop's browser.
func openBrowser(u string) error {
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return errors.New("not a web URL")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
