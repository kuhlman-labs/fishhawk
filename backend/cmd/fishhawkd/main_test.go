package main

import "testing"

// TestSmoke gives `go test ./...` something to run during the scaffold
// phase. Real behaviour tests land alongside the HTTP server in E3.2 (#42).
func TestSmoke(t *testing.T) {
	t.Log("fishhawkd skeleton compiles and links")
}
