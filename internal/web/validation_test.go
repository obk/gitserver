package web

import "testing"

func TestValidation(t *testing.T) {
	for _, p := range []string{"..", "a/../b", "./a", "a//b"} {
		if _, ok := cleanTreePath(p); ok {
			t.Errorf("cleanTreePath(%q) accepted", p)
		}
	}
	for _, n := range []string{"//evil.com", "https://evil.com", `/\evil.com`, "/\t/evil.com", "/\n/evil.com", "/x\x7f", ""} {
		if safeNext(n) != "/" {
			t.Errorf("safeNext(%q) = %q", n, safeNext(n))
		}
	}
	if safeNext("/~alice/pub/?h=main") != "/~alice/pub/?h=main" {
		t.Error("safeNext rejected a local path")
	}
	if ipKey("2001:db8:1:2:3:4:5:6") != ipKey("2001:db8:1:2:ffff::1") || ipKey("2001:db8:1:2::1") == ipKey("2001:db8:1:3::1") {
		t.Error("IPv6 clients are not grouped per /64")
	}
	if ipKey("192.0.2.1") == ipKey("192.0.2.2") {
		t.Error("IPv4 addresses grouped")
	}
}
