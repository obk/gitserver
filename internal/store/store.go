// Package store keeps users, SSH keys and invites in SQLite, and loads the
// key that encrypts TOTP secrets.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"go-git-server/internal/account"
)

// SSHKey is a public key that authenticates git over SSH.
type SSHKey struct {
	ID          string    `json:"id"`
	Key         string    `json:"key"`         // authorized_keys format without comment
	Fingerprint string    `json:"fingerprint"` // SHA256:...
	Comment     string    `json:"comment"`
	Added       time.Time `json:"added"`
}

type User struct {
	Name         string    `json:"name"`
	PasswordHash string    `json:"password_hash"`
	TOTPSecret   string    `json:"totp_secret"`
	TOTPLast     int64     `json:"totp_last"` // last accepted time step, for replay protection
	Admin        bool      `json:"admin"`
	Created      time.Time `json:"created,omitzero"`
	InvitedBy    string    `json:"invited_by,omitempty"`
	SSHKeys      []SSHKey  `json:"ssh_keys"`
}

// Invite is a single-use signup code. Only its SHA-256 is stored.
type Invite struct {
	ID        string    `json:"id"`
	Hash      string    `json:"hash"`
	CreatedBy string    `json:"created_by"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires"`
	Admin     bool      `json:"admin"` // the new account becomes an admin
	UsedBy    string    `json:"used_by,omitempty"`
	Used      time.Time `json:"used,omitzero"`
}

func (i *Invite) Usable(now time.Time) bool { return i.UsedBy == "" && now.Before(i.Expires) }

const maxSSHKeys = 20

var (
	ErrKeyInUse      = errors.New("this SSH key is already registered to an account")
	errLastKey       = errors.New("you cannot delete your only SSH key; add another one first")
	errTooManyKeys   = errors.New("too many SSH keys; delete some first")
	errNoUser        = errors.New("no such user")
	ErrUserExists    = errors.New("user already exists")
	ErrNameUsed      = errors.New("this user name was used before and can't be taken again")
	errInviteInvalid = errors.New("invite is invalid, expired or already used")
)

// Store keeps users, SSH keys and invites in <data>/gitserver.db (SQLite).
// The web server, "gitserver ssh-keys" (run by sshd for every connection)
// and the CLI all open it at the same time; WAL mode and immediate write
// transactions make that safe.
type Store struct {
	db *sql.DB
}

const schemaVersion = 1

var schema = []string{
	`CREATE TABLE users (
		name          TEXT PRIMARY KEY,
		password_hash TEXT NOT NULL,
		totp_secret   TEXT NOT NULL,
		totp_last     INTEGER NOT NULL DEFAULT 0,
		admin         INTEGER NOT NULL DEFAULT 0,
		created       INTEGER NOT NULL,
		invited_by    TEXT NOT NULL DEFAULT ''
	) STRICT`,
	`CREATE TABLE ssh_keys (
		id          TEXT PRIMARY KEY,
		user        TEXT NOT NULL REFERENCES users(name) ON DELETE CASCADE,
		key         TEXT NOT NULL,
		fingerprint TEXT NOT NULL UNIQUE,
		comment     TEXT NOT NULL DEFAULT '',
		added       INTEGER NOT NULL
	) STRICT`,
	`CREATE INDEX ssh_keys_user ON ssh_keys(user)`,
	`CREATE TABLE invites (
		id         TEXT PRIMARY KEY,
		hash       TEXT NOT NULL UNIQUE,
		created_by TEXT NOT NULL,
		created    INTEGER NOT NULL,
		expires    INTEGER NOT NULL,
		admin      INTEGER NOT NULL DEFAULT 0,
		used_by    TEXT NOT NULL DEFAULT '',
		used       INTEGER NOT NULL DEFAULT 0
	) STRICT`,
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "gitserver.db")
	// Create the file ourselves so it (and the -wal/-shm files SQLite derives
	// from it) is private to the service user.
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?_txlock=immediate" +
		"&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)" +
		"&_pragma=secure_delete(1)" // overwrite deleted data instead of leaving it in free pages
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(dataDir); err != nil {
		db.Close()
		return nil, fmt.Errorf("database %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// anyEncryptedTOTP returns one encrypted secret, to check the key against.
func (s *Store) anyEncryptedTOTP() (name, sealed string, err error) {
	err = s.db.QueryRow(`SELECT name, totp_secret FROM users WHERE totp_secret LIKE 'v1:%' ORDER BY name LIMIT 1`).Scan(&name, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return name, sealed, err
}

func (s *Store) countEncryptedTOTP() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM users WHERE totp_secret LIKE 'v1:%'`).Scan(&n)
	return n, err
}

// purgeFreedPages rebuilds the database file and empties the WAL, so data
// that was overwritten (like plaintext secrets) is not left behind on disk.
func (s *Store) purgeFreedPages() error {
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return err
	}
	_, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// Backup writes a consistent snapshot of the database to path, which must
// not exist yet. It is safe while the server is running.
func (s *Store) Backup(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	if _, err := s.db.Exec(`VACUUM INTO ?`, path); err != nil {
		return err
	}
	// The snapshot holds password hashes and TOTP secrets.
	return os.Chmod(path, 0o600)
}

// usedNamesSchema records every user name ever created, so a name can be
// taken only once: repositories and invites refer to users by name, and a
// new account under an old name would inherit them. The trigger fills it on
// every insert, even by an older gitserver that does not know the table.
var usedNamesSchema = []string{
	`CREATE TABLE IF NOT EXISTS used_names (name TEXT PRIMARY KEY) STRICT`,
	`CREATE TRIGGER IF NOT EXISTS users_name_used AFTER INSERT ON users
		BEGIN INSERT OR IGNORE INTO used_names (name) VALUES (NEW.name); END`,
}

// addUsedNames creates used_names and fills it with the names known so far:
// current users, users named in invites, and owners of repository folders
// (which remain after an account is deleted).
func addUsedNames(tx *sql.Tx, dataDir string) error {
	for _, stmt := range usedNamesSchema {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	for _, stmt := range []string{
		`INSERT OR IGNORE INTO used_names (name) SELECT name FROM users`,
		`INSERT OR IGNORE INTO used_names (name) SELECT used_by FROM invites WHERE used_by != ''`,
		`INSERT OR IGNORE INTO used_names (name) SELECT created_by FROM invites WHERE created_by != 'cli'`,
		`INSERT OR IGNORE INTO used_names (name) SELECT invited_by FROM users WHERE invited_by NOT IN ('', 'cli')`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "repos"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && account.UserNameRe.MatchString(e.Name()) {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO used_names (name) VALUES (?)`, e.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureUsedNames adds used_names to a database created before it existed.
// It only writes when the table is missing, so opening the store (which
// sshd does for every connection) stays read-only otherwise.
func (s *Store) ensureUsedNames(dataDir string) error {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name = 'users_name_used'`).Scan(&n); err != nil || n > 0 {
		return err
	}
	return s.tx(func(tx *sql.Tx) error { return addUsedNames(tx, dataDir) })
}

// codeAlertsSchema counts wrong 2FA codes entered together with a user's
// correct password, which means someone else may know the password. The
// user is told at their next login (see TakeCodeFailures). Stored here, not
// in memory, so restarting the server can't hide an attack.
//
// Like used_names, the table is created when the store opens, without
// changing user_version: older versions of gitserver still open the
// database and simply don't use it.
var codeAlertsSchema = []string{
	`CREATE TABLE IF NOT EXISTS code_alerts (
		user     TEXT PRIMARY KEY REFERENCES users(name) ON DELETE CASCADE,
		failures INTEGER NOT NULL,
		since    INTEGER NOT NULL
	) STRICT`,
}

// recoveryCodesSchema holds the unused 2FA recovery codes of each user
// (account.HashRecoveryCode; the codes themselves are never stored). Added
// like code_alerts, so older versions still open the database.
var recoveryCodesSchema = []string{
	`CREATE TABLE IF NOT EXISTS recovery_codes (
		user TEXT NOT NULL REFERENCES users(name) ON DELETE CASCADE,
		hash TEXT NOT NULL,
		PRIMARY KEY (user, hash)
	) STRICT`,
}

// loginsSchema keeps each user's recent successful logins (when, from which
// IP and browser, how), so users can spot a login that wasn't them. Only
// the newest maxLogins per user are kept. Added like code_alerts.
var loginsSchema = []string{
	`CREATE TABLE IF NOT EXISTS logins (
		id     INTEGER PRIMARY KEY,
		user   TEXT NOT NULL REFERENCES users(name) ON DELETE CASCADE,
		at     INTEGER NOT NULL,
		ip     TEXT NOT NULL,
		agent  TEXT NOT NULL,
		method TEXT NOT NULL
	) STRICT`,
	`CREATE INDEX IF NOT EXISTS logins_user ON logins(user, at)`,
}

// maxLogins is how many logins are kept per user.
const maxLogins = 20

// auditSchema is the admin audit log: who changed accounts, invites and
// account security, from the web UI or the command line. It has no foreign
// keys, so the record of a deleted user stays. Only the newest maxAudit
// entries are kept. Added like code_alerts.
var auditSchema = []string{
	`CREATE TABLE IF NOT EXISTS audit_log (
		id     INTEGER PRIMARY KEY,
		at     INTEGER NOT NULL,
		actor  TEXT NOT NULL,
		action TEXT NOT NULL,
		target TEXT NOT NULL,
		detail TEXT NOT NULL,
		ip     TEXT NOT NULL
	) STRICT`,
}

// maxAudit is how many audit log entries are kept.
const maxAudit = 1000

// AuditEntry is one entry of the audit log.
type AuditEntry struct {
	At     time.Time
	Actor  string // the user who did it, or "command line ..." (never a user name)
	Action string // e.g. "invite created", "user deleted"
	Target string // the user or invite it was done to
	Detail string
	IP     string // empty for the command line
}

// Login is one successful login.
type Login struct {
	At     time.Time
	IP     string
	Agent  string // the browser's User-Agent header
	Method string // e.g. "password and code", "recovery code"
}

// ensureTable adds a table that was introduced after schema version 1 to a
// database created before it existed. Like ensureUsedNames, it only writes
// when the table is missing.
func (s *Store) ensureTable(name string, schema []string) error {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil || n > 0 {
		return err
	}
	return s.tx(func(tx *sql.Tx) error {
		for _, stmt := range schema {
			if _, err := tx.Exec(stmt); err != nil {
				return err
			}
		}
		return nil
	})
}

// migrate creates the schema and, on first start, imports the old db.json.
func (s *Store) migrate(dataDir string) error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == schemaVersion {
		if err := s.ensureUsedNames(dataDir); err != nil {
			return err
		}
		if err := s.ensureTable("code_alerts", codeAlertsSchema); err != nil {
			return err
		}
		if err := s.ensureTable("recovery_codes", recoveryCodesSchema); err != nil {
			return err
		}
		if err := s.ensureTable("logins", loginsSchema); err != nil {
			return err
		}
		return s.ensureTable("audit_log", auditSchema)
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema %d is newer than this gitserver (%d); upgrade gitserver", version, schemaVersion)
	}
	jsonPath := filepath.Join(dataDir, "db.json")
	imported := false
	err := s.tx(func(tx *sql.Tx) error {
		// Another process may have migrated while we waited for the lock.
		if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version == schemaVersion {
			return err
		}
		for _, stmt := range schema {
			if _, err := tx.Exec(stmt); err != nil {
				return err
			}
		}
		for _, stmt := range slices.Concat(usedNamesSchema, codeAlertsSchema, recoveryCodesSchema, loginsSchema, auditSchema) {
			if _, err := tx.Exec(stmt); err != nil {
				return err
			}
		}
		var err error
		if imported, err = importJSON(tx, jsonPath); err != nil {
			return fmt.Errorf("importing %s: %w", jsonPath, err)
		}
		if err := addUsedNames(tx, dataDir); err != nil {
			return err
		}
		_, err = tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
		return err
	})
	if err == nil && imported {
		// Keep the old file as a backup, but never import it twice.
		os.Rename(jsonPath, jsonPath+".imported")
		os.Remove(jsonPath + ".lock")
	}
	return err
}

func importJSON(tx *sql.Tx, path string) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var file struct {
		Users   []*User   `json:"users"`
		Invites []*Invite `json:"invites"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		return false, err
	}
	for _, u := range file.Users {
		if err := insertUser(tx, u); err != nil {
			return false, fmt.Errorf("user %s: %w", u.Name, err)
		}
	}
	for _, i := range file.Invites {
		if err := insertInvite(tx, i); err != nil {
			return false, err
		}
	}
	return true, nil
}

// tx runs fn in a write transaction (BEGIN IMMEDIATE, see Open).
func (s *Store) tx(fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

func loadUser(q querier, name string) (*User, error) {
	u := &User{}
	var created int64
	err := q.QueryRow(`SELECT name, password_hash, totp_secret, totp_last, admin, created, invited_by
		FROM users WHERE name = ?`, name).
		Scan(&u.Name, &u.PasswordHash, &u.TOTPSecret, &u.TOTPLast, &u.Admin, &created, &u.InvitedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoUser
	}
	if err != nil {
		return nil, err
	}
	u.Created = fromUnix(created)
	rows, err := q.Query(`SELECT id, key, fingerprint, comment, added FROM ssh_keys
		WHERE user = ? ORDER BY added, id`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k SSHKey
		var added int64
		if err := rows.Scan(&k.ID, &k.Key, &k.Fingerprint, &k.Comment, &added); err != nil {
			return nil, err
		}
		k.Added = fromUnix(added)
		u.SSHKeys = append(u.SSHKeys, k)
	}
	return u, rows.Err()
}

// insertUser adds u and its keys, refusing taken names and keys.
func insertUser(tx *sql.Tx, u *User) error {
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM users WHERE name = ?`, u.Name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrUserExists
	}
	if err := tx.QueryRow(`SELECT count(*) FROM used_names WHERE name = ?`, u.Name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrNameUsed
	}
	if u.Created.IsZero() {
		u.Created = time.Now().UTC()
	}
	_, err := tx.Exec(`INSERT INTO users (name, password_hash, totp_secret, totp_last, admin, created, invited_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.Name, u.PasswordHash, u.TOTPSecret, u.TOTPLast, u.Admin, unix(u.Created), u.InvitedBy)
	if err != nil {
		return err
	}
	for _, k := range u.SSHKeys {
		if err := insertKey(tx, u.Name, k); err != nil {
			return err
		}
	}
	return nil
}

func insertKey(tx *sql.Tx, user string, k SSHKey) error {
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM ssh_keys WHERE fingerprint = ?`, k.Fingerprint).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrKeyInUse
	}
	_, err := tx.Exec(`INSERT INTO ssh_keys (id, user, key, fingerprint, comment, added) VALUES (?, ?, ?, ?, ?, ?)`,
		k.ID, user, k.Key, k.Fingerprint, k.Comment, unix(k.Added))
	return err
}

func insertInvite(tx *sql.Tx, i *Invite) error {
	_, err := tx.Exec(`INSERT INTO invites (id, hash, created_by, created, expires, admin, used_by, used)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ID, i.Hash, i.CreatedBy, unix(i.Created), unix(i.Expires), i.Admin, i.UsedBy, unix(i.Used))
	return err
}

// AddCodeFailure records a wrong 2FA code entered with user's correct
// password (see codeAlertsSchema).
func (s *Store) AddCodeFailure(user string) error {
	_, err := s.db.Exec(`INSERT INTO code_alerts (user, failures, since) VALUES (?, 1, ?)
		ON CONFLICT (user) DO UPDATE SET failures = failures + 1`, user, time.Now().Unix())
	return err
}

// TakeCodeFailures returns and clears the wrong 2FA codes recorded for user:
// how many, and when the first one was entered. n is 0 if there were none.
func (s *Store) TakeCodeFailures(user string) (n int, since time.Time, err error) {
	err = s.tx(func(tx *sql.Tx) error {
		var first int64
		err := tx.QueryRow(`SELECT failures, since FROM code_alerts WHERE user = ?`, user).Scan(&n, &first)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		since = fromUnix(first)
		_, err = tx.Exec(`DELETE FROM code_alerts WHERE user = ?`, user)
		return err
	})
	if err != nil {
		return 0, time.Time{}, err
	}
	return n, since, nil
}

// SetRecoveryCodes replaces all recovery codes of user with these hashes
// (account.HashRecoveryCode), so older codes stop working.
func (s *Store) SetRecoveryCodes(user string, hashes []string) error {
	return s.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM recovery_codes WHERE user = ?`, user); err != nil {
			return err
		}
		for _, h := range hashes {
			if _, err := tx.Exec(`INSERT INTO recovery_codes (user, hash) VALUES (?, ?)`, user, h); err != nil {
				return err
			}
		}
		return nil
	})
}

// UseRecoveryCode deletes the recovery code with this hash and reports
// whether user had it. Each code works only once, even for parallel logins.
func (s *Store) UseRecoveryCode(user, hash string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM recovery_codes WHERE user = ? AND hash = ?`, user, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RecoveryCodesLeft returns how many unused recovery codes user has.
func (s *Store) RecoveryCodesLeft(user string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM recovery_codes WHERE user = ?`, user).Scan(&n)
	return n, err
}

// RecordLogin adds a successful login of user, keeping only the newest
// maxLogins. The browser string is cut to 200 bytes.
func (s *Store) RecordLogin(user string, l Login) error {
	if len(l.Agent) > 200 {
		l.Agent = l.Agent[:200]
	}
	return s.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO logins (user, at, ip, agent, method) VALUES (?, ?, ?, ?, ?)`,
			user, l.At.Unix(), l.IP, l.Agent, l.Method); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM logins WHERE user = ? AND id NOT IN
			(SELECT id FROM logins WHERE user = ? ORDER BY at DESC, id DESC LIMIT ?)`, user, user, maxLogins)
		return err
	})
}

// RecentLogins returns up to n of user's logins, newest first.
func (s *Store) RecentLogins(user string, n int) ([]Login, error) {
	rows, err := s.db.Query(`SELECT at, ip, agent, method FROM logins WHERE user = ?
		ORDER BY at DESC, id DESC LIMIT ?`, user, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Login
	for rows.Next() {
		var l Login
		var at int64
		if err := rows.Scan(&at, &l.IP, &l.Agent, &l.Method); err != nil {
			return nil, err
		}
		l.At = fromUnix(at)
		list = append(list, l)
	}
	return list, rows.Err()
}

// Audit adds e to the audit log (At defaults to now), keeping only the
// newest maxAudit entries. Long fields are cut to 200 bytes.
func (s *Store) Audit(e AuditEntry) error {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	cut := func(v string) string {
		if len(v) > 200 {
			return v[:200]
		}
		return v
	}
	return s.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO audit_log (at, actor, action, target, detail, ip) VALUES (?, ?, ?, ?, ?, ?)`,
			e.At.Unix(), cut(e.Actor), cut(e.Action), cut(e.Target), cut(e.Detail), cut(e.IP)); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM audit_log WHERE id NOT IN
			(SELECT id FROM audit_log ORDER BY at DESC, id DESC LIMIT ?)`, maxAudit)
		return err
	})
}

// AuditLog returns up to n audit log entries, newest first.
func (s *Store) AuditLog(n int) ([]AuditEntry, error) {
	rows, err := s.db.Query(`SELECT at, actor, action, target, detail, ip FROM audit_log
		ORDER BY at DESC, id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&at, &e.Actor, &e.Action, &e.Target, &e.Detail, &e.IP); err != nil {
			return nil, err
		}
		e.At = fromUnix(at)
		list = append(list, e)
	}
	return list, rows.Err()
}

// NameUsed reports whether name belongs, or once belonged, to an account.
func (s *Store) NameUsed(name string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM used_names WHERE name = ?`, name).Scan(&n)
	return n > 0, err
}

// Get returns a freshly loaded copy of the user.
func (s *Store) Get(name string) (*User, error) {
	return loadUser(s.db, name)
}

func (s *Store) List() ([]*User, error) {
	rows, err := s.db.Query(`SELECT name FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	users := make([]*User, 0, len(names))
	for _, n := range names {
		u, err := loadUser(s.db, n)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

func (s *Store) Create(u *User) error {
	return s.tx(func(tx *sql.Tx) error { return insertUser(tx, u) })
}

// Update applies fn to the stored user and saves the result unless fn fails.
// The name and creation time cannot be changed.
func (s *Store) Update(name string, fn func(u *User) error) error {
	return s.tx(func(tx *sql.Tx) error {
		u, err := loadUser(tx, name)
		if err != nil {
			return err
		}
		if err := fn(u); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE users SET password_hash = ?, totp_secret = ?, totp_last = ?, admin = ?, invited_by = ?
			WHERE name = ?`, u.PasswordHash, u.TOTPSecret, u.TOTPLast, u.Admin, u.InvitedBy, name)
		if err != nil {
			return err
		}
		// Replace the key list with whatever fn left in it.
		if _, err := tx.Exec(`DELETE FROM ssh_keys WHERE user = ?`, name); err != nil {
			return err
		}
		for _, k := range u.SSHKeys {
			if err := insertKey(tx, name, k); err != nil {
				return err
			}
		}
		return nil
	})
}

// Delete removes the user with their SSH keys, login history and recovery
// codes, and the unused invites they created: a deleted account can't keep
// inviting people. The name stays taken (see usedNamesSchema). Repositories
// are not touched.
func (s *Store) Delete(name string) error {
	return s.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM users WHERE name = ?`, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errNoUser
		}
		_, err = tx.Exec(`DELETE FROM invites WHERE created_by = ? AND used_by = ''`, name)
		return err
	})
}

// UserByKey returns the user owning the key with this fingerprint.
func (s *Store) UserByKey(fingerprint string) (*User, error) {
	var name string
	err := s.db.QueryRow(`SELECT user FROM ssh_keys WHERE fingerprint = ?`, fingerprint).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoUser
	}
	if err != nil {
		return nil, err
	}
	return loadUser(s.db, name)
}

// AddSSHKey adds k to user; a key can belong to only one account.
func (s *Store) AddSSHKey(user string, k SSHKey) error {
	return s.tx(func(tx *sql.Tx) error {
		var exists, keys int
		err := tx.QueryRow(`SELECT (SELECT count(*) FROM users WHERE name = ?), (SELECT count(*) FROM ssh_keys WHERE user = ?)`,
			user, user).Scan(&exists, &keys)
		if err != nil {
			return err
		}
		if exists == 0 {
			return errNoUser
		}
		if keys >= maxSSHKeys {
			return errTooManyKeys
		}
		return insertKey(tx, user, k)
	})
}

// DeleteSSHKey removes a key, but never the user's last one.
func (s *Store) DeleteSSHKey(user, id string) error {
	return s.tx(func(tx *sql.Tx) error {
		var mine, keys int
		err := tx.QueryRow(`SELECT (SELECT count(*) FROM ssh_keys WHERE user = ? AND id = ?), (SELECT count(*) FROM ssh_keys WHERE user = ?)`,
			user, id, user).Scan(&mine, &keys)
		if err != nil {
			return err
		}
		if mine == 0 {
			return errors.New("no such key")
		}
		if keys <= 1 {
			return errLastKey
		}
		_, err = tx.Exec(`DELETE FROM ssh_keys WHERE user = ? AND id = ?`, user, id)
		return err
	})
}

// CreateInvite stores a new invite and returns its plaintext code.
func (s *Store) CreateInvite(createdBy string, admin bool, ttl time.Duration) (code string, inv *Invite, err error) {
	code = account.RandomToken(24)
	now := time.Now().UTC()
	inv = &Invite{ID: account.RandomToken(6), Hash: account.HashToken(code), CreatedBy: createdBy, Created: now, Expires: now.Add(ttl), Admin: admin}
	err = s.tx(func(tx *sql.Tx) error {
		// Drop invites that expired unused more than 30 days ago.
		if _, err := tx.Exec(`DELETE FROM invites WHERE used_by = '' AND expires < ?`, now.Add(-30*24*time.Hour).Unix()); err != nil {
			return err
		}
		return insertInvite(tx, inv)
	})
	return code, inv, err
}

func scanInvites(rows *sql.Rows) ([]*Invite, error) {
	defer rows.Close()
	var list []*Invite
	for rows.Next() {
		i := &Invite{}
		var created, expires, used int64
		if err := rows.Scan(&i.ID, &i.Hash, &i.CreatedBy, &created, &expires, &i.Admin, &i.UsedBy, &used); err != nil {
			return nil, err
		}
		i.Created, i.Expires, i.Used = fromUnix(created), fromUnix(expires), fromUnix(used)
		list = append(list, i)
	}
	return list, rows.Err()
}

const inviteColumns = `id, hash, created_by, created, expires, admin, used_by, used`

// Invites returns the invites createdBy made, newest first; all of them if
// createdBy is "" (the command line).
func (s *Store) Invites(createdBy string) ([]*Invite, error) {
	rows, err := s.db.Query(`SELECT `+inviteColumns+` FROM invites WHERE ? = '' OR created_by = ? ORDER BY created DESC, id`,
		createdBy, createdBy)
	if err != nil {
		return nil, err
	}
	return scanInvites(rows)
}

func findInvite(q querier, hash string) (*Invite, error) {
	rows, err := q.Query(`SELECT `+inviteColumns+` FROM invites WHERE hash = ? AND used_by = '' AND expires > ?`,
		hash, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	list, err := scanInvites(rows)
	if err != nil {
		return nil, err
	}
	if len(list) != 1 || !strings.EqualFold(list[0].Hash, hash) {
		return nil, errInviteInvalid
	}
	return list[0], nil
}

// LookupInvite returns the usable invite for code.
func (s *Store) LookupInvite(code string) (*Invite, error) {
	return findInvite(s.db, account.HashToken(code))
}

// RevokeInvite deletes an unused invite that createdBy made; any unused
// invite if createdBy is "" (the command line).
func (s *Store) RevokeInvite(id, createdBy string) error {
	res, err := s.db.Exec(`DELETE FROM invites WHERE id = ? AND used_by = '' AND (? = '' OR created_by = ?)`,
		id, createdBy, createdBy)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errInviteInvalid
	}
	return nil
}

// RedeemInvite atomically consumes the invite identified by codeHash and
// creates u. Admin rights come from the invite, never from the caller.
func (s *Store) RedeemInvite(codeHash string, u *User) error {
	return s.tx(func(tx *sql.Tx) error {
		inv, err := findInvite(tx, codeHash)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		u.Admin = inv.Admin
		u.InvitedBy = inv.CreatedBy
		u.Created = now
		if err := insertUser(tx, u); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE invites SET used_by = ?, used = ? WHERE id = ?`, u.Name, now.Unix(), inv.ID)
		return err
	})
}
