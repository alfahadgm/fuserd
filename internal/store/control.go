package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"fuseronline/internal/mainstage"
	"strings"
	"time"
)

const controlSchema = `
CREATE TABLE IF NOT EXISTS control_tokens(hash TEXT PRIMARY KEY, name TEXT NOT NULL, expires INTEGER NOT NULL, id TEXT, created INTEGER NOT NULL DEFAULT 0, created_by TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS control_admins(id TEXT PRIMARY KEY, name TEXT NOT NULL, credential_id TEXT NOT NULL UNIQUE, credential BLOB NOT NULL, created INTEGER NOT NULL DEFAULT 0, created_by TEXT NOT NULL DEFAULT '', last_login INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS control_audit(seq INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, admin_id TEXT NOT NULL, admin_name TEXT NOT NULL, ip TEXT NOT NULL, action TEXT NOT NULL, target TEXT NOT NULL, detail TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS control_sessions(hash TEXT PRIMARY KEY, admin_id TEXT NOT NULL REFERENCES control_admins(id) ON DELETE CASCADE, expires INTEGER NOT NULL, timezone TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS control_documents(key TEXT PRIMARY KEY, value BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS control_notes(kind TEXT NOT NULL,id TEXT NOT NULL,note TEXT NOT NULL,PRIMARY KEY(kind,id));
CREATE TABLE IF NOT EXISTS control_media(hash TEXT PRIMARY KEY, data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS mainstage_slot_blocks(event_uuid TEXT NOT NULL COLLATE NOCASE,event_slot INTEGER NOT NULL,PRIMARY KEY(event_uuid,event_slot));
CREATE TABLE IF NOT EXISTS mainstage_removed(uuid TEXT PRIMARY KEY COLLATE NOCASE);
`

// ControlCLI is the creator recorded for enrollment tokens and audit entries made with "fuserd control".
const ControlCLI = "cli"

// controlColumns are the columns added to control tables after their first release; controlMigrate adds them to
// databases created before.
var controlColumns = []struct{ table, column, ddl string }{
	{"control_tokens", "id", "id TEXT"},
	{"control_tokens", "created", "created INTEGER NOT NULL DEFAULT 0"},
	{"control_tokens", "created_by", "created_by TEXT NOT NULL DEFAULT ''"},
	{"control_admins", "created", "created INTEGER NOT NULL DEFAULT 0"},
	{"control_admins", "created_by", "created_by TEXT NOT NULL DEFAULT ''"},
	{"control_admins", "last_login", "last_login INTEGER NOT NULL DEFAULT 0"},
	{"control_sessions", "timezone", "timezone TEXT NOT NULL DEFAULT ''"},
	{"mainstage_events", "allow_stock", "allow_stock INTEGER NOT NULL DEFAULT 0"},
}

func controlMigrate(db *sql.DB) error {
	for _, c := range controlColumns {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, c.table, c.column).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE ` + c.table + ` ADD COLUMN ` + c.ddl); err != nil {
				return err
			}
		}
	}
	_, err := db.Exec(`UPDATE control_tokens SET id=lower(hex(randomblob(8))) WHERE id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS control_tokens_id ON control_tokens(id);
CREATE INDEX IF NOT EXISTS control_audit_at ON control_audit(at);`)
	return err
}

func ControlHash(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func ControlRandom(n int) (string, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}

// ControlPendingToken is an unused enrollment token. ID is a public handle for listing and cancelling; the token
// itself is stored only as its hash.
type ControlPendingToken struct {
	ID, Name, CreatedBy string
	Created, Expires    time.Time
}

// ControlToken creates a single-use enrollment token. createdBy is the creating administrator's ID, or ControlCLI.
func (s *Store) ControlToken(ctx context.Context, name string, ttl time.Duration, createdBy string) (token, id string, err error) {
	if strings.TrimSpace(name) == "" || len(name) > 80 || ttl <= 0 || ttl > 24*time.Hour {
		return "", "", errors.New("name required; expiry must be between 0 and 24 hours")
	}
	if token, err = ControlRandom(8); err != nil {
		return "", "", err
	}
	if id, err = ControlRandom(8); err != nil {
		return "", "", err
	}
	now := time.Now()
	_, err = s.db.ExecContext(ctx, `INSERT INTO control_tokens(hash,name,expires,id,created,created_by) VALUES(?,?,?,?,?,?)`,
		ControlHash(token), strings.TrimSpace(name), now.Add(ttl).Unix(), id, now.Unix(), createdBy)
	if err != nil {
		return "", "", err
	}
	return token, id, nil
}
func (s *Store) ControlTokenLookup(ctx context.Context, hash string) (ControlPendingToken, error) {
	var t ControlPendingToken
	var created, expires int64
	err := s.db.QueryRowContext(ctx, `SELECT id,name,created_by,created,expires FROM control_tokens WHERE hash=? AND expires>?`, hash, time.Now().Unix()).
		Scan(&t.ID, &t.Name, &t.CreatedBy, &created, &expires)
	t.Created, t.Expires = time.Unix(created, 0), time.Unix(expires, 0)
	return t, err
}

// ControlTokens lists the unexpired enrollment tokens, soonest expiry first, and deletes the expired ones.
func (s *Store) ControlTokens(ctx context.Context) ([]ControlPendingToken, error) {
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM control_tokens WHERE expires<=?`, now); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,created_by,created,expires FROM control_tokens WHERE expires>? ORDER BY expires,id`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ControlPendingToken{}
	for rows.Next() {
		var t ControlPendingToken
		var created, expires int64
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedBy, &created, &expires); err != nil {
			return nil, err
		}
		t.Created, t.Expires = time.Unix(created, 0), time.Unix(expires, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Store) ControlTokenRevoke(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM control_tokens WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type ControlAdmin struct {
	ID, Name, CredentialID string
	Credential             []byte
	// CreatedBy is the ID of the administrator whose token enrolled this one, or ControlCLI. Created, CreatedBy and
	// LastLogin are zero for administrators enrolled before they were recorded.
	CreatedBy          string
	Created, LastLogin time.Time
}

const controlAdminCols = `id,name,credential_id,credential,created_by,created,last_login`

func scanControlAdmin(row interface{ Scan(...any) error }) (ControlAdmin, error) {
	var a ControlAdmin
	var created, last int64
	err := row.Scan(&a.ID, &a.Name, &a.CredentialID, &a.Credential, &a.CreatedBy, &created, &last)
	if created != 0 {
		a.Created = time.Unix(created, 0)
	}
	if last != 0 {
		a.LastLogin = time.Unix(last, 0)
	}
	return a, err
}

// ControlEnroll consumes the enrollment token and stores the administrator, crediting the token's creator.
func (s *Store) ControlEnroll(ctx context.Context, hash string, a ControlAdmin) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	var createdBy string
	err = tx.QueryRowContext(ctx, `SELECT created_by FROM control_tokens WHERE hash=? AND expires>?`, hash, now).Scan(&createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("enrollment token expired or used")
	}
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM control_tokens WHERE hash=? AND expires>?`, hash, now)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("enrollment token expired or used")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO control_admins(`+controlAdminCols+`) VALUES(?,?,?,?,?,?,?)`, a.ID, a.Name, a.CredentialID, a.Credential, createdBy, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ControlAdmin(ctx context.Context, id string) (ControlAdmin, error) {
	return scanControlAdmin(s.db.QueryRowContext(ctx, `SELECT `+controlAdminCols+` FROM control_admins WHERE id=?`, id))
}

// ControlAdmins lists every administrator, oldest first.
func (s *Store) ControlAdmins(ctx context.Context) ([]ControlAdmin, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+controlAdminCols+` FROM control_admins ORDER BY created,name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ControlAdmin{}
	for rows.Next() {
		a, err := scanControlAdmin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ControlCredential stores the credential after a sign-in (its new sign count) and records the sign-in time.
func (s *Store) ControlCredential(ctx context.Context, id string, old, new []byte) error {
	res, err := s.db.ExecContext(ctx, `UPDATE control_admins SET credential=?,last_login=? WHERE id=? AND credential=?`, new, time.Now().Unix(), id, old)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("credential changed or revoked")
	}
	return nil
}

// ControlSession stores a session. timezone is the IANA zone the browser reported at sign-in, or empty for the
// server's default.
func (s *Store) ControlSession(ctx context.Context, hash, id string, expires time.Time, timezone string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM control_sessions WHERE expires<=?`, time.Now().Unix())
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO control_sessions(hash,admin_id,expires,timezone) VALUES(?,?,?,?)`, hash, id, expires.Unix(), timezone)
	return err
}
func (s *Store) ControlSessionAdmin(ctx context.Context, hash string) (ControlAdmin, error) {
	a, _, err := s.ControlSessionLookup(ctx, hash)
	return a, err
}

// ControlSessionLookup returns a live session's administrator and the timezone stored with the session.
func (s *Store) ControlSessionLookup(ctx context.Context, hash string) (ControlAdmin, string, error) {
	var id, timezone string
	err := s.db.QueryRowContext(ctx, `SELECT admin_id,timezone FROM control_sessions WHERE hash=? AND expires>?`, hash, time.Now().Unix()).Scan(&id, &timezone)
	if err != nil {
		return ControlAdmin{}, "", err
	}
	a, err := s.ControlAdmin(ctx, id)
	return a, timezone, err
}
func (s *Store) ControlLogout(ctx context.Context, hash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM control_sessions WHERE hash=?`, hash)
	return err
}

// ControlRevoke deletes the administrator, which ends their sessions (ON DELETE CASCADE), and cancels the enrollment
// tokens they created: a removed administrator must not keep a way back in.
func (s *Store) ControlRevoke(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM control_admins WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM control_tokens WHERE created_by=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ControlAuditEntry is one administrator action. AdminID is empty for unattributed entries; the command line
// records AdminID ControlCLI.
type ControlAuditEntry struct {
	At                                             time.Time
	AdminID, AdminName, IP, Action, Target, Detail string
}

func (s *Store) ControlAuditAdd(ctx context.Context, e ControlAuditEntry) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO control_audit(at,admin_id,admin_name,ip,action,target,detail) VALUES(?,?,?,?,?,?,?)`,
		e.At.Unix(), e.AdminID, e.AdminName, e.IP, e.Action, e.Target, e.Detail)
	return err
}

// ControlAudit returns the newest limit entries, newest first.
func (s *Store) ControlAudit(ctx context.Context, limit int) ([]ControlAuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at,admin_id,admin_name,ip,action,target,detail FROM control_audit ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ControlAuditEntry{}
	for rows.Next() {
		var e ControlAuditEntry
		var at int64
		if err := rows.Scan(&at, &e.AdminID, &e.AdminName, &e.IP, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) ControlDocument(ctx context.Context, key string, v any) (bool, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM control_documents WHERE key=?`, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

// Expected document bytes provide optimistic concurrency across tabs and processes.
func (s *Store) ControlSaveDocument(ctx context.Context, key string, v any, expected string) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old []byte
	err = tx.QueryRowContext(ctx, `SELECT value FROM control_documents WHERE key=?`, key).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if ControlHash(string(old)) != expected {
		return errors.New("data changed; reload before saving")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO control_documents VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, b)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ControlDocumentVersion(ctx context.Context, key string) (string, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM control_documents WHERE key=?`, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return ControlHash(string(b)), err
}
func (s *Store) ControlUsers(ctx context.Context) ([]Player, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+playerCols+` FROM players ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Player{}
	for rows.Next() {
		p, err := scanPlayer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}
func (s *Store) ControlNotes(ctx context.Context, kind string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,note FROM control_notes WHERE kind=?`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
func (s *Store) ControlUserSave(ctx context.Context, id int64, name string, banned bool, note string) error {
	if len(name) > 80 || strings.TrimSpace(name) == "" || len(note) > 4000 {
		return errors.New("invalid name or notes")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE players SET hype_username=?,banned=? WHERE id=?`, name, banned, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrNotFound
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO control_notes VALUES('user',?,?) ON CONFLICT(kind,id) DO UPDATE SET note=excluded.note`, fmt.Sprint(id), note)
	if err != nil {
		return err
	}
	return tx.Commit()
}

type ControlExemption struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Key      string `json:"key"`
	Notes    string `json:"notes"`
}

func (s *Store) ControlExemptions(ctx context.Context) ([]ControlExemption, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sub,dn,COALESCE(note,'') FROM epic_auth_policy LEFT JOIN control_notes ON kind='exemption' AND id=sub WHERE state='allowed' ORDER BY sub`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ControlExemption{}
	for rows.Next() {
		var e ControlExemption
		if err := rows.Scan(&e.Key, &e.Username, &e.Notes); err != nil {
			return nil, err
		}
		e.ID = e.Key
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) ControlExemptionSave(ctx context.Context, old string, e *ControlExemption) error {
	if len(e.Key) > 256 || len(e.Username) > 80 || len(e.Notes) > 4000 || strings.TrimSpace(e.Key) == "" || strings.TrimSpace(e.Username) == "" {
		return errors.New("invalid exemption")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if old != "" {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM epic_auth_policy WHERE sub=?`, old).Scan(&state); err != nil {
			return err
		}
		if state != "allowed" {
			return errors.New("verified and blocked identities cannot be edited as exemptions")
		}
	}
	if old != e.Key {
		var state string
		err := tx.QueryRowContext(ctx, `SELECT state FROM epic_auth_policy WHERE sub=?`, e.Key).Scan(&state)
		if !errors.Is(err, sql.ErrNoRows) {
			return errors.New("that key already has an authentication policy")
		}
	}
	if old != "" && old != e.Key {
		if _, err = tx.ExecContext(ctx, `DELETE FROM epic_auth_policy WHERE sub=? AND state='allowed'`, old); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM control_notes WHERE kind='exemption' AND id=?`, old); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO epic_auth_policy VALUES(?,?,'allowed') ON CONFLICT(sub) DO UPDATE SET dn=excluded.dn WHERE state='allowed'`, e.Key, e.Username)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO control_notes VALUES('exemption',?,?) ON CONFLICT(kind,id) DO UPDATE SET note=excluded.note`, e.Key, e.Notes)
	if err != nil {
		return err
	}
	e.ID = e.Key
	return tx.Commit()
}
func (s *Store) ControlExemptionRemove(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM epic_auth_policy WHERE sub=? AND state='allowed'`, key)
	return err
}

type ControlSlot struct {
	User    *int64 `json:"user"`
	Blocked bool   `json:"blocked"`
}

func (s *Store) MainStageBlocks(ctx context.Context) (map[string][]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event_uuid,event_slot FROM mainstage_slot_blocks ORDER BY event_slot`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[strings.ToLower(k)] = append(out[strings.ToLower(k)], n)
	}
	return out, rows.Err()
}
func (s *Store) MainStageRemoved(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT uuid FROM mainstage_removed`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[strings.ToLower(k)] = true
	}
	return out, rows.Err()
}
func (s *Store) ControlEventSave(ctx context.Context, e mainstage.Event, slots []ControlSlot, expected []ControlSlot) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if len(slots) != e.Slots {
		return errors.New("slot count does not match event")
	}
	e.UUID = strings.ToLower(e.UUID)
	if len(e.Name) > 100 {
		return errors.New("event name too long")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Refuse an edit that would discard a slot bought since the editor opened.
	rows, err := tx.QueryContext(ctx, `SELECT player_id,event_slot,source FROM mainstage_headliners WHERE event_uuid=?`, e.UUID)
	if err != nil {
		return err
	}
	actual := make([]ControlSlot, len(expected))
	sources := map[int64]string{}
	for rows.Next() {
		var id int64
		var slot int
		var source string
		if err := rows.Scan(&id, &slot, &source); err != nil {
			rows.Close()
			return err
		}
		if slot < 1 || slot > len(expected) || expected[slot-1].User == nil || *expected[slot-1].User != id {
			rows.Close()
			return errors.New("headliners changed; reload before saving")
		}

		actual[slot-1].User = &id
		sources[id] = source
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	blockRows, err := tx.QueryContext(ctx, `SELECT event_slot FROM mainstage_slot_blocks WHERE event_uuid=?`, e.UUID)
	if err != nil {
		return err
	}
	for blockRows.Next() {
		var n int
		if err := blockRows.Scan(&n); err != nil {
			blockRows.Close()
			return err
		}
		if n < 1 || n > len(actual) {
			blockRows.Close()
			return errors.New("slot blocks changed; reload before saving")
		}
		actual[n-1].Blocked = true
	}
	blockRows.Close()
	if err := blockRows.Err(); err != nil {
		return err
	}
	for i, want := range expected {
		got := actual[i]
		if got.Blocked != want.Blocked || (got.User == nil) != (want.User == nil) || got.User != nil && *got.User != *want.User {
			return errors.New("headliners or slot blocks changed; reload before saving")
		}
	}
	var other string
	err = tx.QueryRowContext(ctx, `SELECT uuid FROM mainstage_events WHERE uuid!=? AND start_at<? AND end_at>? LIMIT 1`, e.UUID, e.End.Unix(), e.Start.Unix()).Scan(&other)
	if err == nil {
		return ErrMainStageOverlap
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mainstage_events(uuid,name,type,start_at,end_at,special_name,special_type_name,special_description,image_url,slots,turn_minutes,created_at,allow_stock) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(uuid) DO UPDATE SET name=excluded.name,type=excluded.type,start_at=excluded.start_at,end_at=excluded.end_at,special_name=excluded.special_name,special_type_name=excluded.special_type_name,special_description=excluded.special_description,image_url=excluded.image_url,slots=excluded.slots,turn_minutes=excluded.turn_minutes,allow_stock=excluded.allow_stock`, e.UUID, e.Name, e.Type, e.Start.Unix(), e.End.Unix(), e.SpecialName, e.SpecialTypeName, e.SpecialDescription, e.ImageURL, e.Slots, e.TurnMinutes, time.Now().Unix(), e.AllowStock)
	if err != nil {
		return err
	}
	for _, q := range []string{`DELETE FROM mainstage_headliners WHERE event_uuid=?`, `DELETE FROM mainstage_slot_blocks WHERE event_uuid=?`, `DELETE FROM mainstage_removed WHERE uuid=?`} {
		if _, err = tx.ExecContext(ctx, q, e.UUID); err != nil {
			return err
		}
	}
	seen := map[int64]bool{}
	for i, slot := range slots {
		if slot.User != nil {
			if slot.Blocked || seen[*slot.User] {
				return errors.New("duplicate or blocked headliner")
			}
			seen[*slot.User] = true
			var banned bool
			if err := tx.QueryRowContext(ctx, `SELECT banned FROM players WHERE id=?`, *slot.User).Scan(&banned); err != nil {
				return err
			}
			if banned {
				return errors.New("banned user cannot headline")
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO mainstage_headliners VALUES(?,?,?,?,?)`, e.UUID, *slot.User, i+1, headlinerSource(sources, *slot.User, slot, i, expected), time.Now().Unix()); err != nil {
				return err
			}
		} else if slot.Blocked {
			if _, err = tx.ExecContext(ctx, `INSERT INTO mainstage_slot_blocks VALUES(?,?)`, e.UUID, i+1); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
func (s *Store) ControlEventRemove(ctx context.Context, uuid string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM mainstage_events WHERE uuid=?`, `DELETE FROM mainstage_headliners WHERE event_uuid=?`, `DELETE FROM mainstage_slot_blocks WHERE event_uuid=?`, `INSERT OR IGNORE INTO mainstage_removed VALUES(?)`} {
		if _, err = tx.ExecContext(ctx, q, uuid); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) ControlMediaSave(ctx context.Context, b []byte) (string, error) {
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO control_media VALUES(?,?)`, hash, b)
	return hash, err
}
func (s *Store) ControlMedia(ctx context.Context, hash string) ([]byte, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM control_media WHERE hash=?`, hash).Scan(&b)
	return b, err
}

func headlinerSource(sources map[int64]string, id int64, slot ControlSlot, i int, old []ControlSlot) string {
	if i < len(old) && old[i].User != nil && *old[i].User == id && sources[id] != "" {
		return sources[id]
	}
	return HeadlinerAdmin
}

// MainStageOverrideIDs also covers moved occurrences outside the current polling window.
func (s *Store) MainStageOverrideIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT uuid FROM mainstage_events`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[strings.ToLower(id)] = true
	}
	return out, rows.Err()
}
