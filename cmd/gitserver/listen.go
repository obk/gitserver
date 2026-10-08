package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strconv"
	"strings"
)

// listen opens the address given to "serve -listen":
//
//	host:port        TCP, e.g. 127.0.0.1:8080
//	unix:/path       a Unix socket, created with mode 0660
//	systemd          the socket systemd passes in (socket activation, see
//	                 deploy/gitserver.socket)
//
// A Unix socket is reachable only by the users its file mode allows, so a
// local user can't connect and set their own X-Real-IP, as they could on a
// localhost TCP port.
func listen(addr string) (net.Listener, error) {
	if addr == "systemd" {
		return systemdListener(os.Getenv("LISTEN_PID"), os.Getenv("LISTEN_FDS"), 3)
	}
	if path, ok := strings.CutPrefix(addr, "unix:"); ok {
		return unixListener(path)
	}
	return net.Listen("tcp", addr)
}

// unixListener listens on a new socket at path, replacing a stale socket
// left by an earlier run, but never any other kind of file.
func unixListener(path string) (net.Listener, error) {
	if path == "" {
		return nil, errors.New("-listen unix: needs a path, e.g. unix:/run/gitserver/http.sock")
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// The umask may have removed group access; the owner's group (and only
	// it) must be able to connect.
	if err := os.Chmod(path, 0o660); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// systemdListener returns the listening socket systemd passed as file
// descriptor fd (the first one, 3) when it started this process.
func systemdListener(pid, fds string, fd int) (net.Listener, error) {
	if pid == "" || fds == "" {
		return nil, errors.New("-listen systemd: no socket from systemd (start the service through gitserver.socket)")
	}
	if p, err := strconv.Atoi(pid); err != nil || p != os.Getpid() {
		return nil, errors.New("-listen systemd: the socket from systemd was meant for another process")
	}
	if n, err := strconv.Atoi(fds); err != nil || n != 1 {
		return nil, fmt.Errorf("-listen systemd: expected 1 socket from systemd, got %q", fds)
	}
	// Child processes (git) must not inherit these.
	os.Unsetenv("LISTEN_PID")
	os.Unsetenv("LISTEN_FDS")
	os.Unsetenv("LISTEN_FDNAMES")
	f := os.NewFile(uintptr(fd), "systemd-socket")
	defer f.Close() // net.FileListener works on a duplicate
	return net.FileListener(f)
}
