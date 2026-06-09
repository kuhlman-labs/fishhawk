package main

import (
	"io"
	"strings"
	"testing"
)

func TestParseFlags_DefaultsToStdio(t *testing.T) {
	tf, err := parseFlags([]string{"fishhawk-mcp"}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if tf.transport != transportStdio {
		t.Errorf("transport = %q, want %q", tf.transport, transportStdio)
	}
	if tf.addr != defaultHTTPAddr {
		t.Errorf("addr = %q, want %q", tf.addr, defaultHTTPAddr)
	}
}

func TestParseFlags_HTTPTransport(t *testing.T) {
	tf, err := parseFlags([]string{"fishhawk-mcp", "--transport", "http", "--addr", "127.0.0.1:9000"}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if tf.transport != transportHTTP {
		t.Errorf("transport = %q, want %q", tf.transport, transportHTTP)
	}
	if tf.addr != "127.0.0.1:9000" {
		t.Errorf("addr = %q, want 127.0.0.1:9000", tf.addr)
	}
}

func TestParseFlags_RejectsUnknownTransport(t *testing.T) {
	_, err := parseFlags([]string{"fishhawk-mcp", "--transport", "grpc"}, io.Discard)
	if err == nil {
		t.Fatal("expected an error for an unknown --transport value")
	}
	if !strings.Contains(err.Error(), "grpc") {
		t.Errorf("error should name the offending value; got %q", err.Error())
	}
}

func TestLoadConfig_HappyPath(t *testing.T) {
	env := map[string]string{
		"FISHHAWK_BACKEND_URL": "https://app.fishhawk.example.com",
		"FISHHAWK_API_TOKEN":   "tok_abc123",
	}
	cfg, err := loadConfig(envFunc(env))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.backendURL != "https://app.fishhawk.example.com" {
		t.Errorf("backendURL = %q", cfg.backendURL)
	}
	if cfg.apiToken != "tok_abc123" {
		t.Errorf("apiToken = %q", cfg.apiToken)
	}
}

func TestLoadConfig_BackendURLDefaultsToLocalhost(t *testing.T) {
	// Mirror the CLI's default so an operator who's only set the
	// token can flip between cli and mcp without re-configuring.
	env := map[string]string{
		"FISHHAWK_API_TOKEN": "tok",
	}
	cfg, err := loadConfig(envFunc(env))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.backendURL != "http://localhost:8080" {
		t.Errorf("backendURL = %q, want http://localhost:8080", cfg.backendURL)
	}
}

func TestLoadConfig_BackendURLTrailingSlashStripped(t *testing.T) {
	// URL concatenation at request time appends `/v0/…` — keeping
	// the trailing slash would produce `//v0/…` which works on
	// servers but reads ugly in logs and reverse proxies sometimes
	// reject it.
	env := map[string]string{
		"FISHHAWK_BACKEND_URL": "https://app.fishhawk.example.com/",
		"FISHHAWK_API_TOKEN":   "tok",
	}
	cfg, err := loadConfig(envFunc(env))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.backendURL != "https://app.fishhawk.example.com" {
		t.Errorf("backendURL = %q (trailing slash not stripped)", cfg.backendURL)
	}
}

func TestLoadConfig_RequiresAPIToken(t *testing.T) {
	// No "anonymous" mode for MCP — every tool round-trips the
	// API; running unauthenticated would be a silent permission
	// bug, not a developer convenience.
	env := map[string]string{
		"FISHHAWK_BACKEND_URL": "http://localhost:8080",
	}
	_, err := loadConfig(envFunc(env))
	if err == nil {
		t.Fatal("expected error when FISHHAWK_API_TOKEN is missing")
	}
	if !strings.Contains(err.Error(), "FISHHAWK_API_TOKEN") {
		t.Errorf("error should name the missing variable; got %q", err.Error())
	}
}

func TestLoadConfig_RequiresAPIToken_EmptyValueIsMissing(t *testing.T) {
	// An exported-but-empty FISHHAWK_API_TOKEN (e.g. a forgotten
	// secret in CI) must be rejected the same as an unset var —
	// otherwise the binary would happily fail every API call
	// downstream with a less obvious 401.
	env := map[string]string{
		"FISHHAWK_API_TOKEN": "",
	}
	_, err := loadConfig(envFunc(env))
	if err == nil {
		t.Fatal("empty FISHHAWK_API_TOKEN should error like a missing one")
	}
}

func TestBuildServer_HandshakeReady(t *testing.T) {
	// E19.2 ships handshake-only: the server should construct
	// cleanly with an empty tool registry. The protocol-level
	// handshake itself is tested by the SDK; this test just locks
	// in that buildServer doesn't panic and returns a non-nil
	// server we can hand to mcp.StdioTransport later.
	srv := buildServer(config{
		backendURL: "http://localhost:8080",
		apiToken:   "tok",
	})
	if srv == nil {
		t.Fatal("buildServer returned nil")
	}
}

// envFunc returns a func(string) string backed by a literal map. We
// don't use os.Setenv in tests because env state would leak across
// parallel runs; loadConfig takes the getter as a parameter for
// exactly this reason.
func envFunc(env map[string]string) func(string) string {
	return func(k string) string {
		return env[k]
	}
}
