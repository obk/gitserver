package main

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
	"strings"
	"time"

	_ "modernc.org/sqlite"
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
	errKeyInUse      = errors.New("this SSH key is already registered to an account")
	errLastKey       = errors.New("you cannot delete your only SSH key; add another one first")
	errTooManyKeys   = errors.New("too many SSH keys; delete some first")
	errNoUser        = errors.New("no such user")
	errUserExists    = errors.New("user already exists")
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

func openStore(dataDir string) (*Store, error) {
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

// migrate creates the schema and, on first start, imports the old db.json.
func (s *Store) migrate(dataDir string) error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == schemaVersion {
		return nil
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
		var err error
		if imported, err = importJSON(tx, jsonPath); err != nil {
			return fmt.Errorf("importing %s: %w", jsonPath, err)
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

// tx runs fn in a write transaction (BEGIN IMMEDIATE, see openStore).
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
		return errUserExists
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
		return errKeyInUse
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

func (s *Store) Delete(name string) error {
	res, err := s.db.Exec(`DELETE FROM users WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNoUser
	}
	return nil
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
	code = randomToken(24)
	now := time.Now().UTC()
	inv = &Invite{ID: randomToken(6), Hash: hashToken(code), CreatedBy: createdBy, Created: now, Expires: now.Add(ttl), Admin: admin}
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

func (s *Store) Invites() ([]*Invite, error) {
	rows, err := s.db.Query(`SELECT ` + inviteColumns + ` FROM invites ORDER BY created DESC, id`)
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
	return findInvite(s.db, hashToken(code))
}

func (s *Store) RevokeInvite(id string) error {
	res, err := s.db.Exec(`DELETE FROM invites WHERE id = ? AND used_by = ''`, id)
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
