package main

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestUnixListener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "http.sock")
	l, err := listen("unix:" + path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o660 || fi.Mode().Type() != os.ModeSocket {
		t.Fatalf("socket mode: %v %v", fi.Mode(), err)
	}
	go func() {
		if c, err := l.Accept(); err == nil {
			c.Write([]byte("ok"))
			c.Close()
		}
	}()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := c.Read(buf); err != nil || string(buf) != "ok" {
		t.Fatalf("read %q: %v", buf, err)
	}
	c.Close()
	l.Close()

	// A stale socket from a crashed run is replaced...
	stale, _ := net.Listen("unix", path)
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	if l, err := listen("unix:" + path); err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	} else {
		l.Close()
	}
	// ...but no other kind of file is ever removed.
	file := filepath.Join(t.TempDir(), "data")
	os.WriteFile(file, []byte("keep"), 0o644)
	if _, err := listen("unix:" + file); err == nil {
		t.Fatal("listened over a regular file")
	}
	if b, _ := os.ReadFile(file); string(b) != "keep" {
		t.Fatal("regular file was replaced")
	}
	if _, err := listen("unix:"); err == nil {
		t.Fatal("empty socket path accepted")
	}
}

func TestSystemdListener(t *testing.T) {
	pid := strconv.Itoa(os.Getpid())
	for _, tc := range []struct{ pid, fds string }{
		{"", ""}, {"1", "1"}, {pid, ""}, {pid, "2"}, {pid, "x"},
	} {
		if _, err := systemdListener(tc.pid, tc.fds, 99); err == nil {
			t.Errorf("LISTEN_PID=%q LISTEN_FDS=%q accepted", tc.pid, tc.fds)
		}
	}
	// A real listening socket passed by descriptor number, as systemd does.
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	f, err := tcp.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LISTEN_PID", pid)
	t.Setenv("LISTEN_FDS", "1")
	l, err := systemdListener(pid, "1", int(f.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if os.Getenv("LISTEN_FDS") != "" {
		t.Error("LISTEN_FDS left for child processes")
	}
	go func() {
		if c, err := l.Accept(); err == nil {
			c.Close()
		}
	}()
	c, err := net.Dial("tcp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}
