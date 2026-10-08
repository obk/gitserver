package store

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"go-git-server/internal/account"
)

var testBinary string // gitserver, for the ssh-keys reader processes

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gitserver-test-bin-")
	if err != nil {
		panic(err)
	}
	testBinary = filepath.Join(dir, "gitserver")
	if out, err := exec.Command("go", "build", "-o", testBinary, "go-git-server/cmd/gitserver").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("go build: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestStoreImportsJSON(t *testing.T) {
	dir := t.TempDir()
	key, err := testKey(t, "old@laptop")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	old := map[string]any{
		"users": []*User{{Name: "obk", PasswordHash: "hash", TOTPSecret: "SECRET", TOTPLast: 42, Admin: true,
			Created: now, SSHKeys: []SSHKey{key}}},
		"invites": []*Invite{{ID: "inv1", Hash: account.HashToken("code"), CreatedBy: "obk", Created: now, Expires: now.Add(time.Hour)}},
	}
	b, _ := json.Marshal(old)
	os.WriteFile(filepath.Join(dir, "db.json"), b, 0o600)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.Get("obk")
	if err != nil || !u.Admin || u.TOTPLast != 42 || u.PasswordHash != "hash" || len(u.SSHKeys) != 1 ||
		u.SSHKeys[0].Fingerprint != key.Fingerprint || u.SSHKeys[0].Comment != "old@laptop" || !u.Created.Equal(now) {
		t.Fatalf("user not imported correctly: %+v, %v", u, err)
	}
	if _, err := s.LookupInvite("code"); err != nil {
		t.Fatalf("invite not imported: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "db.json")); !os.IsNotExist(err) {
		t.Fatal("db.json still in place (would be imported again)")
	}
	if _, err := os.Stat(filepath.Join(dir, "db.json.imported")); err != nil {
		t.Fatal("db.json backup missing")
	}
	s.Close()

	// Reopening keeps the data and does not import anything again.
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if users, _ := s2.List(); len(users) != 1 {
		t.Fatalf("got %d users after reopen", len(users))
	}
	// A backup is a complete database that gitserver can open directly.
	backupDir := t.TempDir()
	if err := s2.Backup(filepath.Join(backupDir, "gitserver.db")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(backupDir, "gitserver.db")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup file mode: %v %v", fi.Mode(), err)
	}
	if err := s2.Backup(filepath.Join(backupDir, "gitserver.db")); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
	bs, err := Open(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	if u, err := bs.Get("obk"); err != nil || len(u.SSHKeys) != 1 {
		t.Fatalf("backup incomplete: %v", err)
	}
	bs.Close()
	fi, _ := os.Stat(filepath.Join(dir, "gitserver.db"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("database file mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestStoreConstraints(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	key, _ := testKey(t, "")
	if err := s.Create(&User{Name: "a", SSHKeys: []SSHKey{key}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(&User{Name: "a"}); !errors.Is(err, ErrUserExists) {
		t.Fatalf("duplicate user: %v", err)
	}
	key2 := key
	key2.ID = "other"
	if err := s.Create(&User{Name: "b", SSHKeys: []SSHKey{key2}}); !errors.Is(err, ErrKeyInUse) {
		t.Fatalf("duplicate key on create: %v", err)
	}
	if _, err := s.Get("b"); err == nil {
		t.Fatal("failed create left a user behind")
	}
	s.Create(&User{Name: "b"})
	if err := s.Update("b", func(u *User) error { u.SSHKeys = append(u.SSHKeys, key2); return nil }); !errors.Is(err, ErrKeyInUse) {
		t.Fatalf("duplicate key via update: %v", err)
	}
	if err := s.DeleteSSHKey("a", key.ID); !errors.Is(err, errLastKey) {
		t.Fatalf("deleted last key: %v", err)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByKey(key.Fingerprint); err == nil {
		t.Fatal("key survived user deletion")
	}
	if err := s.Delete("a"); !errors.Is(err, errNoUser) {
		t.Fatalf("delete missing user: %v", err)
	}
	// A name can be taken only once, even after the account is deleted.
	if err := s.Create(&User{Name: "a"}); !errors.Is(err, ErrNameUsed) {
		t.Fatalf("deleted name reused: %v", err)
	}
	if used, err := s.NameUsed("a"); !used || err != nil {
		t.Fatalf("NameUsed(a) = %v, %v", used, err)
	}
	if used, _ := s.NameUsed("never"); used {
		t.Fatal("unused name reported as used")
	}
}

// Databases from before used_names get it on open, filled with the names
// known so far: users, invites and repository folders of deleted users.
func TestUsedNamesMigration(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Create(&User{Name: "alice"})
	code, _, _ := s.CreateInvite("alice", false, time.Hour)
	if err := s.RedeemInvite(account.HashToken(code), &User{Name: "bob"}); err != nil {
		t.Fatal(err)
	}
	s.Delete("bob")
	for _, stmt := range []string{`DROP TRIGGER users_name_used`, `DROP TABLE used_names`} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	os.MkdirAll(filepath.Join(dir, "repos", "carol"), 0o750) // carol was deleted before the upgrade

	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, name := range []string{"alice", "bob", "carol"} {
		if used, err := s.NameUsed(name); !used || err != nil {
			t.Errorf("%s not recorded as used: %v", name, err)
		}
	}
	if err := s.Create(&User{Name: "dave"}); err != nil {
		t.Fatal(err)
	}
	if used, _ := s.NameUsed("dave"); !used {
		t.Fatal("new user not recorded by the trigger")
	}
}

// Many writers (goroutines and separate Store instances, like the web
// server and the CLI) plus reader processes (like sshd's ssh-keys) must not
// fail with "database is locked" or lose updates.
func TestStoreConcurrency(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	key, _ := testKey(t, "")
	line := key.Key
	if err := s.Create(&User{Name: "a", SSHKeys: []SSHKey{key}}); err != nil {
		t.Fatal(err)
	}
	other, err := Open(dir) // a second "process"
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	const writers, perWriter = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter+100)
	for w := range writers {
		st := s
		if w%2 == 1 {
			st = other
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWriter {
				errs <- st.Update("a", func(u *User) error { u.TOTPLast++; return nil })
			}
		}()
	}
	f := strings.Fields(line)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := exec.Command(testBinary, "ssh-keys", "-data", dir, "git", f[0], f[1]).Output()
			if err == nil && !strings.Contains(string(out), "ssh-serve") {
				err = errors.New("ssh-keys printed no key line: " + string(out))
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if u, _ := s.Get("a"); u.TOTPLast != writers*perWriter {
		t.Fatalf("lost updates: totp_last = %d, want %d", u.TOTPLast, writers*perWriter)
	}
}

// testKey returns a new ed25519 SSHKey (sshgit.ParseKey can't be used here:
// sshgit imports this package).
func testKey(t *testing.T, comment string) (SSHKey, error) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sp, _ := ssh.NewPublicKey(pub)
	return SSHKey{ID: account.RandomToken(9), Key: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))),
		Fingerprint: ssh.FingerprintSHA256(sp), Comment: comment, Added: time.Now().UTC()}, nil
}

// Wrong 2FA codes are counted per user until taken, and go with the user.
func TestCodeAlerts(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Create(&User{Name: "alice"})
	s.Create(&User{Name: "bob"})
	before := time.Now().Add(-time.Second)
	for range 3 {
		if err := s.AddCodeFailure("alice"); err != nil {
			t.Fatal(err)
		}
	}
	if n, _, _ := s.TakeCodeFailures("bob"); n != 0 {
		t.Fatalf("bob has %d failures", n)
	}
	n, since, err := s.TakeCodeFailures("alice")
	if err != nil || n != 3 || since.Before(before) || since.After(time.Now()) {
		t.Fatalf("take: %d %v %v", n, since, err)
	}
	if n, _, _ := s.TakeCodeFailures("alice"); n != 0 {
		t.Fatalf("failures not cleared: %d", n)
	}
	// Failures belong to the account: deleting it deletes them.
	s.AddCodeFailure("bob")
	s.Delete("bob")
	var rows int
	s.db.QueryRow(`SELECT count(*) FROM code_alerts`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("%d code_alerts rows left after deleting the user", rows)
	}
	if err := s.AddCodeFailure("nobody"); err == nil {
		t.Fatal("failure recorded for a user that does not exist")
	}
}

// Databases from before code_alerts get it on open, and the schema version
// stays the same, so older gitserver versions can still open them.
func TestCodeAlertsMigration(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Create(&User{Name: "alice"})
	if _, err := s.db.Exec(`DROP TABLE code_alerts`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.AddCodeFailure("alice"); err != nil {
		t.Fatalf("table not added on open: %v", err)
	}
	var version int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != schemaVersion || schemaVersion != 1 {
		t.Fatalf("user_version %d (schemaVersion %d): older versions would refuse the database", version, schemaVersion)
	}
}

func TestRecoveryCodesStore(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Create(&User{Name: "alice"})
	s.Create(&User{Name: "bob"})
	if err := s.SetRecoveryCodes("alice", []string{"h1", "h2"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.RecoveryCodesLeft("alice"); n != 2 {
		t.Fatalf("%d codes left, want 2", n)
	}
	if ok, err := s.UseRecoveryCode("bob", "h1"); ok || err != nil {
		t.Fatalf("bob used alice's code: %v %v", ok, err)
	}
	if ok, err := s.UseRecoveryCode("alice", "h1"); !ok || err != nil {
		t.Fatalf("code not accepted: %v %v", ok, err)
	}
	if ok, _ := s.UseRecoveryCode("alice", "h1"); ok {
		t.Fatal("code accepted twice")
	}
	// A new set replaces the old one.
	s.SetRecoveryCodes("alice", []string{"h3"})
	if ok, _ := s.UseRecoveryCode("alice", "h2"); ok {
		t.Fatal("old code still works after new codes were made")
	}
	if n, _ := s.RecoveryCodesLeft("alice"); n != 1 {
		t.Fatalf("%d codes left, want 1", n)
	}
	// Codes go with the account.
	s.SetRecoveryCodes("bob", []string{"b1"})
	s.Delete("bob")
	var rows int
	s.db.QueryRow(`SELECT count(*) FROM recovery_codes WHERE user = 'bob'`).Scan(&rows)
	if rows != 0 {
		t.Fatal("codes left after deleting the user")
	}
	// Databases from before recovery_codes get it on open, same version.
	if _, err := s.db.Exec(`DROP TABLE recovery_codes`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetRecoveryCodes("alice", []string{"h4"}); err != nil {
		t.Fatalf("table not added on open: %v", err)
	}
	var version int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&version)
	if version != 1 {
		t.Fatalf("user_version %d", version)
	}
}

func TestLogins(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Create(&User{Name: "alice"})
	s.Create(&User{Name: "bob"})
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i := range 25 {
		if err := s.RecordLogin("alice", Login{At: start.Add(time.Duration(i) * time.Minute), IP: fmt.Sprintf("192.0.2.%d", i), Agent: "UA", Method: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	s.RecordLogin("bob", Login{At: start, IP: "198.51.100.1", Agent: strings.Repeat("x", 500), Method: "m"})
	list, err := s.RecentLogins("alice", 100)
	if err != nil || len(list) != maxLogins {
		t.Fatalf("%d logins kept, want %d: %v", len(list), maxLogins, err)
	}
	if list[0].IP != "192.0.2.24" || list[maxLogins-1].IP != "192.0.2.5" || !list[0].At.Equal(start.Add(24*time.Minute)) {
		t.Fatalf("not the newest first: %+v ... %+v", list[0], list[maxLogins-1])
	}
	if b, _ := s.RecentLogins("bob", 1); len(b) != 1 || len(b[0].Agent) != 200 {
		t.Fatal("bob's login missing or agent not shortened")
	}
	s.Delete("bob")
	var rows int
	s.db.QueryRow(`SELECT count(*) FROM logins WHERE user = 'bob'`).Scan(&rows)
	if rows != 0 {
		t.Fatal("logins left after deleting the user")
	}
	// Databases from before the logins table get it on open.
	s.db.Exec(`DROP TABLE logins`)
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecordLogin("alice", Login{At: start, IP: "x", Agent: "y", Method: "z"}); err != nil {
		t.Fatalf("table not added on open: %v", err)
	}
}
