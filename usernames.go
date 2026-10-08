package main

import (
	"embed"
	"io/fs"
	"regexp"
	"strings"
	"sync"
)

var userNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// blockedUserNames can't be registered, so nobody can pose as staff or the system.
var blockedUserNames = map[string]bool{
	"admin": true, "administrator": true, "root": true, "sysadmin": true, "system": true,
	"superuser": true, "su": true, "sudo": true, "owner": true, "staff": true,
	"mod": true, "moderator": true, "official": true, "team": true, "support": true,
	"help": true, "security": true, "abuse": true, "postmaster": true, "hostmaster": true,
	"webmaster": true, "noreply": true, "no-reply": true, "info": true, "contact": true,
	"git": true, "anonymous": true, "anon": true, "nobody": true, "null": true,
	"guest": true, "user": true, "test": true, "api": true, "www": true, "mail": true,
	"login": true, "logout": true, "signup": true, "settings": true, "static": true, "create": true,
}

//go:embed blocklists/*.txt
var blocklistFiles embed.FS

// blocklist holds every name from blocklists/*.txt (see blocklists/README.md).
var blocklist = sync.OnceValue(func() map[string]bool {
	names := make(map[string]bool)
	files, _ := fs.Glob(blocklistFiles, "blocklists/*.txt")
	for _, f := range files {
		b, err := blocklistFiles.ReadFile(f)
		if err != nil {
			panic(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			line, _, _ = strings.Cut(line, "#")
			if line = strings.ToLower(strings.TrimSpace(line)); line != "" {
				names[line] = true
			}
		}
	}
	return names
})

func blockedUserName(name string) bool { return blockedUserNames[name] || blocklist()[name] }

func validUserName(name string) bool { return userNameRe.MatchString(name) && !blockedUserName(name) }
