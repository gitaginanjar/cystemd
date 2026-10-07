package service

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/golang-jwt/jwt/v5"
)

// AuthConfig holds the config.yml auth settings (env overrides applied by
// applyAuthEnvOverrides).
type AuthConfig struct {
	Enabled       bool   `yaml:"enabled"`
	PublicKeyFile string `yaml:"public_key_file"`
}

var (
	publicKey   ed25519.PublicKey
	publicKeyMu sync.RWMutex // guards publicKey between InitAuth (writer) and RequireJWT (readers)

	// authReloadMu serialises the copy → InitAuth → write-back span that
	// StartConfigReload and StartEnvReload run on cfg.Auth (copy under
	// cfg.mu.RLock, InitAuth unlocked, write under cfg.mu.Lock). Both hold it for
	// the whole span and copy cfg.Auth only after acquiring it; otherwise a
	// slower InitAuth writes a stale copy back over a fresher one, silently
	// reverting a key rotation (lost update).
	authReloadMu sync.Mutex
)

// applyAuthEnvOverrides applies the env precedence to cfg in place:
// AUTH_ENABLED overrides auth.enabled (invalid → Warning, config value kept)
// and AUTH_PUBLIC_KEY_FILE overrides auth.public_key_file. It loads no key
// material, so applyReloaded re-applies it after a config.yml reload: a reload
// must never regress an env-forced auth state to the config.yml value.
func applyAuthEnvOverrides(cfg *AuthConfig) {
	if v := strings.TrimSpace(os.Getenv("AUTH_ENABLED")); v != "" {
		if enabled, err := parseBoolEnv(v); err != nil {
			Warningf("auth: invalid AUTH_ENABLED=%q (accepted: true/false, yes/no, 1/0) — using config.yml value (%t)", v, cfg.Enabled)
		} else {
			cfg.Enabled = enabled
			Debugf("auth: AUTH_ENABLED=%q overrides config.yml auth.enabled", v)
		}
	}
	if v := strings.TrimSpace(os.Getenv("AUTH_PUBLIC_KEY_FILE")); v != "" {
		cfg.PublicKeyFile = v
		Debugf("auth: AUTH_PUBLIC_KEY_FILE=%q overrides config.yml auth.public_key_file", v)
	}
}

// inlinePublicKeyEnv names the env var carrying the JWT key INLINE (OpenSSH
// text). Never merge it with AUTH_PUBLIC_KEY_FILE (a path, the next rung of the
// precedence): the file reader would os.ReadFile the key text itself.
const inlinePublicKeyEnv = "AUTH_PUBLIC_KEY"

// legacyInlinePublicKeyEnv is the deprecated unprefixed spelling (renamed
// 2026-09-07), still honoured: every host's .env sets it and CD never rewrites
// .env, so a hard cutover would break auth fleet-wide on rollout.
// Why: DOCS/MEMORY.md § `PUBLIC_KEY` renamed to `AUTH_PUBLIC_KEY`
const legacyInlinePublicKeyEnv = "PUBLIC_KEY"

// resolveInlinePublicKey returns the inline JWT key and the NAME of the variable
// it came from: AUTH_PUBLIC_KEY, else the deprecated PUBLIC_KEY ("" when
// neither). Return the real source, never assume the new name: the load line
// must name what the operator set. Both set → AUTH_PUBLIC_KEY wins with a
// Warning (the operator may have edited the wrong one); legacy alone → a
// deprecation Warning.
func resolveInlinePublicKey() (key, source string) {
	current := strings.TrimSpace(os.Getenv(inlinePublicKeyEnv))
	legacy := strings.TrimSpace(os.Getenv(legacyInlinePublicKeyEnv))

	switch {
	case current != "" && legacy != "":
		Warningf("auth: both %s and %s are set — using %s and ignoring the deprecated %s; remove %s from .env",
			inlinePublicKeyEnv, legacyInlinePublicKeyEnv, inlinePublicKeyEnv,
			legacyInlinePublicKeyEnv, legacyInlinePublicKeyEnv)
		return current, inlinePublicKeyEnv
	case current != "":
		return current, inlinePublicKeyEnv
	case legacy != "":
		Warningf("auth: %s is deprecated — rename it to %s in .env; it still works for now",
			legacyInlinePublicKeyEnv, inlinePublicKeyEnv)
		return legacy, legacyInlinePublicKeyEnv
	}
	return "", ""
}

// InitAuth applies the env overrides and, when auth is enabled, loads the
// Ed25519 verification key. Key source, first wins: AUTH_PUBLIC_KEY (inline;
// PUBLIC_KEY is its deprecated alias) → AUTH_PUBLIC_KEY_FILE →
// auth.public_key_file. AUTH_ENABLED (true/false, yes/no, 1/0, any case)
// overrides auth.enabled. Returns an error when the key cannot be read or parsed.
func InitAuth(cfg *AuthConfig) error {
	applyAuthEnvOverrides(cfg)

	if !cfg.Enabled {
		Infof("auth: disabled — all endpoints are open")
		return nil
	}

	envKey, envKeySource := resolveInlinePublicKey()
	if envKey != "" {
		pk, err := parseSSHED25519PublicKey(envKey)
		if err != nil {
			return fmt.Errorf("failed to parse inline public key env (%s): %w", envKeySource, err)
		}
		publicKeyMu.Lock()
		publicKey = pk
		publicKeyMu.Unlock()
		Infof("auth: enabled — public key loaded from %s env var", envKeySource)
		return nil
	}

	pubBytes, err := os.ReadFile(cfg.PublicKeyFile)
	if err != nil {
		return fmt.Errorf("failed to read public key file: %w", err)
	}
	pk, err := parseSSHED25519PublicKey(strings.TrimSpace(string(pubBytes)))
	if err != nil {
		return fmt.Errorf("failed to parse public key file %s: %w", cfg.PublicKeyFile, err)
	}
	publicKeyMu.Lock()
	publicKey = pk
	publicKeyMu.Unlock()
	Infof("auth: enabled — public key loaded from %s", cfg.PublicKeyFile)
	return nil
}

// RequireJWT wraps next with JWT verification when cfg.Enabled. cfg is read
// once, at wrap time, so enabling auth later needs a restart. Tokens must be
// Ed25519-signed and carry exp. See ADR-0006.
func RequireJWT(cfg *AuthConfig, next http.HandlerFunc) http.HandlerFunc {
	if !cfg.Enabled {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		tokenStr := r.Header.Get("Authorization")
		if tokenStr == "" {
			Warningf("auth: missing Authorization header from %s on %s", r.RemoteAddr, r.URL.Path)
			recordAuth("missing")
			http.Error(w, "missing Authorization header", http.StatusUnauthorized)
			return
		}
		// A bare token (no "Bearer " scheme) is accepted as-is.
		tokenStr = strings.TrimPrefix(tokenStr, "Bearer ")

		publicKeyMu.RLock()
		key := publicKey
		publicKeyMu.RUnlock()
		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
				return nil, fmt.Errorf("unexpected signing method")
			}
			return key, nil
		}, jwt.WithExpirationRequired())

		if err != nil || !token.Valid {
			Warningf("auth: invalid token from %s on %s: %v", r.RemoteAddr, r.URL.Path, err)
			recordAuth("invalid")
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		Debugf("auth: token accepted from %s on %s", r.RemoteAddr, r.URL.Path)
		recordAuth("accepted")
		next(w, r)
	}
}

// parseSSHED25519PublicKey parses an OpenSSH "ssh-ed25519 <base64> [comment]"
// line into the raw 32-byte key. The base64 blob is the OpenSSH wire format:
//
//	[uint32 key-type-len]["ssh-ed25519"][uint32 key-len][32-byte key]
func parseSSHED25519PublicKey(sshKey string) (ed25519.PublicKey, error) {
	parts := strings.Fields(sshKey)
	if len(parts) < 2 {
		return nil, errors.New("invalid ssh-ed25519 key format")
	}
	if parts[0] != "ssh-ed25519" {
		return nil, errors.New("not an ssh-ed25519 key")
	}
	decoded, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("failed to base64 decode key: %w", err)
	}

	if len(decoded) < 4 {
		return nil, errors.New("decoded key blob too short")
	}
	keyTypeLen := binary.BigEndian.Uint32(decoded[0:4])
	// uint64 bounds: a near-2^32 length must not overflow past this check and
	// panic the slice; a bad hot-reloaded key would crash the running server.
	if 4+uint64(keyTypeLen)+4 > uint64(len(decoded)) {
		return nil, errors.New("decoded key blob malformed: key type truncated")
	}
	keyType := string(decoded[4 : 4+keyTypeLen])
	if keyType != "ssh-ed25519" {
		return nil, fmt.Errorf("unexpected key type in blob: %s", keyType)
	}
	keyDataOffset := 4 + keyTypeLen
	keyDataLen := binary.BigEndian.Uint32(decoded[keyDataOffset : keyDataOffset+4])
	keyDataOffset += 4
	if uint64(keyDataOffset)+uint64(keyDataLen) > uint64(len(decoded)) {
		return nil, errors.New("decoded key blob malformed: key data truncated")
	}
	keyBytes := decoded[keyDataOffset : keyDataOffset+keyDataLen]
	if len(keyBytes) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("unexpected ed25519 key size: got %d, want %d", len(keyBytes), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(keyBytes), nil
}
