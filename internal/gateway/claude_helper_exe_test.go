package gateway

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaudeHelperExeCopiedOnce checks that the MCP helper is run from a
// stable copy under the cache: the copy matches the source, is executable,
// and a second call returns the same path without rewriting the file, so an
// endpoint-security agent is not handed a new file to judge on each launch.
func TestClaudeHelperExeCopiedOnce(t *testing.T) {
	src := filepath.Join(t.TempDir(), "magpie-reclaude")
	content := []byte("#!/bin/sh\necho magpie\n")
	if err := os.WriteFile(src, content, 0o755); err != nil {
		t.Fatal(err)
	}
	cacheRoot := t.TempDir()

	dst, err := copyExeToCache(src, cacheRoot)
	if err != nil {
		t.Fatalf("first copy: %v", err)
	}
	if rel, err := filepath.Rel(cacheRoot, dst); err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("copy %q is not under cache %q", dst, cacheRoot)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("copy content = %q, want %q", got, content)
	}
	if fi, err := os.Stat(dst); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("copy mode = %v, want executable", fi.Mode())
	}

	before, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	dst2, err := copyExeToCache(src, cacheRoot)
	if err != nil {
		t.Fatalf("second copy: %v", err)
	}
	if dst2 != dst {
		t.Fatalf("second copy path = %q, want %q", dst2, dst)
	}
	after, err := os.Stat(dst2)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("second call rewrote the copy (modtime %v -> %v)", before.ModTime(), after.ModTime())
	}
}

// TestClaudeHelperExeNewContentNewCopy checks that a different executable, a
// new build, is copied to a new path, so a deploy is picked up and does not
// read back the old copy.
func TestClaudeHelperExeNewContentNewCopy(t *testing.T) {
	cacheRoot := t.TempDir()
	write := func(content string) string {
		src := filepath.Join(t.TempDir(), "magpie-reclaude")
		if err := os.WriteFile(src, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		dst, err := copyExeToCache(src, cacheRoot)
		if err != nil {
			t.Fatal(err)
		}
		return dst
	}
	if a, b := write("build one"), write("build two, different bytes"); a == b {
		t.Fatalf("different builds share a copy path %q", a)
	}
}
