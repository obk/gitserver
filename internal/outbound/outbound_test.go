package outbound

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

func TestIsPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "140.82.112.3": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false, "::": false,
		"fd00::1": false, "fe80::1": false, "::ffff:127.0.0.1": false, "::ffff:10.0.0.1": false,
		"64:ff9b::a00:1": false, "224.0.0.1": false, "255.255.255.255": false, "198.18.0.1": false,
	} {
		if got := IsPublic(netip.MustParseAddr(addr)); got != want {
			t.Errorf("IsPublic(%s) = %v", addr, got)
		}
	}
}

func TestPublicAddrs(t *testing.T) {
	ctx := context.Background()
	if _, err := PublicAddrs(ctx, "127.0.0.1"); !errors.Is(err, ErrNotPublic) {
		t.Errorf("loopback literal: %v", err)
	}
	if _, err := PublicAddrs(ctx, "localhost"); !errors.Is(err, ErrNotPublic) {
		t.Errorf("localhost: %v", err)
	}
	if a, err := PublicAddrs(ctx, "1.1.1.1"); err != nil || len(a) != 1 {
		t.Errorf("public literal: %v %v", a, err)
	}
}
