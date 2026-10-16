// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gitaginanjar/cystemd/internal/testutil"
)

func TestKVThroughTokenSink(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	agent, token := testutil.StartVault(t)
	sink := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(sink, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{Address: agent.URL, TokenFile: sink})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.Read(ctx, "cystemd/repositories/x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing secret: %v", err)
	}
	if err := c.Write(ctx, "cystemd/repositories/x", map[string]any{"sshPrivateKey": "KEY"}); err != nil {
		t.Fatal(err)
	}
	data, err := c.Read(ctx, "cystemd/repositories/x")
	if err != nil || data["sshPrivateKey"] != "KEY" {
		t.Fatalf("read back: %v %v", data, err)
	}
	if err := c.Delete(ctx, "cystemd/repositories/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(ctx, "cystemd/repositories/x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	st, err := c.Health(ctx)
	if err != nil || !st.Initialized || st.Sealed {
		t.Fatalf("health: %+v %v", st, err)
	}

	// A rotated sink (Vault Agent re-auth) is picked up; a bad token fails.
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(sink, []byte("bogus"), 0o600)
	future := time.Now().Add(time.Second)
	os.Chtimes(sink, future, future)
	if _, err := c.Read(ctx, "cystemd/x"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("rotated (bad) token not used: %v", err)
	}
}
