package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTouchStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync-last-ok")
	touchStateFile(path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	if len(b) < len("2026-") {
		t.Fatalf("unexpected state content: %s", b)
	}
}
