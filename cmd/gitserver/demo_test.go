package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:8080": true, "localhost:80": true, "[::1]:8080": true,
		":8080": false, "0.0.0.0:8080": false, "192.0.2.1:8080": false, "example.com:80": false, "bogus": false} {
		if loopbackAddr(addr) != want {
			t.Errorf("loopbackAddr(%q) = %v", addr, !want)
		}
	}
}

func TestUpdateHandsOff(t *testing.T) {
	dir := t.TempDir()
	old := gitserverctl
	defer func() { gitserverctl = old }()
	gitserverctl = filepath.Join(dir, "missing")
	if err := cmdUpdate(nil); err == nil || !strings.Contains(err.Error(), "install.sh") {
		t.Fatalf("no helper: %v", err)
	}
}
