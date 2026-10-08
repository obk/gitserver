package main

import (
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
