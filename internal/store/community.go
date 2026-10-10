package store

// Community songs: entries that point at a song's files on its creator's Google Drive, added by
// "fuserd community", and the loaders' reports on fetching them.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"fuseronline/internal/community"
)

// CommunityEntry is a stored community song.
type CommunityEntry struct {
	community.Song
	Disabled       bool
	DisabledReason string
	AddedAt        time.Time
	UpdatedAt      time.Time
	SeenSHA256     []string // the pak hashes loaders reported after a good install, oldest first
}

// Results of PutCommunitySong.
const (
	CommunityAdded     = "added"
	CommunityUpdated   = "updated"
	CommunityUnchanged = "unchanged"
)

// ErrCommunityExists refuses an add for a shortname that already has an entry (use update).
var ErrCommunityExists = errors.New("already has an entry (use update to replace it)")

// Report reasons that count towards disabling an entry: the files themselves are wrong, not just unreachable.
var communityBadFileReasons = []string{"layout", "sig"}

// CommunityDisableAfter is how many different players must report a bad file before the entry is disabled.
const CommunityDisableAfter = 2

// PutCommunitySong adds an entry, or with update replaces one. An update that changes the entry clears its reports
// and re-enables it, since the files are new; an identical one changes nothing.
func (s *Store) PutCommunitySong(ctx context.Context, song community.Song, update bool) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	res, err := putCommunity(ctx, tx, song, update, time.Now().Unix())
	if err != nil || res == CommunityUnchanged {
		return res, err
	}
	return res, tx.Commit()
}

// PutCommunitySongs applies PutCommunitySong to each song in one transaction: all of them or, on an error, none.
func (s *Store) PutCommunitySongs(ctx context.Context, songs []community.Song, update bool) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	out := make([]string, len(songs))
	for i, song := range songs {
		if out[i], err = putCommunity(ctx, tx, song, update, now); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

func putCommunity(ctx context.Context, tx *sql.Tx, song community.Song, update bool, now int64) (string, error) {
	if err := song.Validate(); err != nil {
		return "", err
	}
	entry, err := json.Marshal(song)
	if err != nil {
		return "", err
	}
	var old string
	err = tx.QueryRowContext(ctx, `SELECT entry FROM community_songs WHERE x = ?`, song.X).Scan(&old)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `INSERT INTO community_songs (x, entry, creator, added_at, updated_at)
			VALUES (?, ?, ?, ?, ?)`, song.X, string(entry), song.Creator, now, now); err != nil {
			return "", err
		}
		return CommunityAdded, nil
	case err != nil:
		return "", err
	case !update:
		return "", fmt.Errorf("%s %w", song.X, ErrCommunityExists)
	case old == string(entry):
		return CommunityUnchanged, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE community_songs SET x = ?, entry = ?, creator = ?, disabled = 0,
		disabled_reason = '', updated_at = ? WHERE x = ?`, song.X, string(entry), song.Creator, now, song.X); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM community_reports WHERE x = ?`, song.X); err != nil {
		return "", err
	}
	return CommunityUpdated, nil
}

// CommunityEntriesJSON returns every stored entry as its JSON, keyed by lower-cased shortname (for planning a
// batch against).
func (s *Store) CommunityEntriesJSON(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT x, entry FROM community_songs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var x, entry string
		if err := rows.Scan(&x, &entry); err != nil {
			return nil, err
		}
		out[strings.ToLower(x)] = entry
	}
	return out, rows.Err()
}

// CommunityQuery selects a page of entries. Search matches the shortname, artist, title or creator (a substring;
// case is ignored for ASCII letters). State is "", "enabled", "disabled" or "failing" (an entry with a failed
// report); Kind "", community.KindArchive or community.KindFolder; Sort "updated" (newest first, the default),
// "artist", "creator" or "x". Page counts from 1.
type CommunityQuery struct {
	Search, State, Kind, Sort string
	Page, Size                int
}

// CommunityStats counts every entry, whatever the query.
type CommunityStats struct {
	Songs    int `json:"songs"`
	Enabled  int `json:"enabled"`
	Disabled int `json:"disabled"`
	Archives int `json:"archives"`
	Folders  int `json:"folders"`
	Failing  int `json:"failing"` // entries with at least one failed report
}

// CommunityListed is an entry with its report counts (each player's latest report).
type CommunityListed struct {
	*CommunityEntry
	ReportsOK, ReportsFailed int
}

// CommunityResult is one page of a CommunityQuery.
type CommunityResult struct {
	Stats   CommunityStats
	Matched int // entries matching the query
	Page    int // the page returned: the query's, clamped to 1..Pages
	Pages   int
	Items   []CommunityListed
}

var communitySorts = map[string]string{
	"updated": `c.updated_at DESC, c.x`,
	"artist":  `json_extract(c.entry, '$.artist') COLLATE NOCASE, json_extract(c.entry, '$.title') COLLATE NOCASE, c.x`,
	"creator": `c.creator COLLATE NOCASE, c.x`,
	"x":       `c.x`,
}

// CommunityPage runs q. An unknown state, kind or sort is an error.
func (s *Store) CommunityPage(ctx context.Context, q CommunityQuery) (*CommunityResult, error) {
	if q.Sort == "" {
		q.Sort = "updated"
	}
	order, ok := communitySorts[q.Sort]
	if !ok {
		return nil, fmt.Errorf("unknown sort %q", q.Sort)
	}
	if q.Size < 1 {
		return nil, errors.New("page size must be positive")
	}
	where, args := []string{"1"}, []any{}
	if q.Search != "" {
		where = append(where, `instr(lower(c.x || char(10) || coalesce(json_extract(c.entry, '$.artist'), '') || char(10) ||
			coalesce(json_extract(c.entry, '$.title'), '') || char(10) || c.creator), lower(?)) > 0`)
		args = append(args, q.Search)
	}
	switch q.State {
	case "":
	case "enabled":
		where = append(where, `c.disabled = 0`)
	case "disabled":
		where = append(where, `c.disabled = 1`)
	case "failing":
		where = append(where, `coalesce(r.bad, 0) > 0`)
	default:
		return nil, fmt.Errorf("unknown state %q", q.State)
	}
	switch q.Kind {
	case "":
	case community.KindArchive, community.KindFolder:
		where = append(where, `json_extract(c.entry, '$.kind') = ?`)
		args = append(args, q.Kind)
	default:
		return nil, fmt.Errorf("unknown kind %q", q.Kind)
	}
	from := ` FROM community_songs c LEFT JOIN (SELECT x, sum(ok) AS ok, sum(1 - ok) AS bad FROM community_reports
		GROUP BY x) r ON r.x = c.x WHERE ` + strings.Join(where, " AND ")
	res := &CommunityResult{}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(disabled), 0),
		coalesce(sum(json_extract(entry, '$.kind') = 'archive'), 0), coalesce(sum(json_extract(entry, '$.kind') = 'folder'), 0),
		(SELECT count(*) FROM community_songs WHERE x IN (SELECT x FROM community_reports WHERE ok = 0))
		FROM community_songs`).Scan(&res.Stats.Songs, &res.Stats.Disabled, &res.Stats.Archives, &res.Stats.Folders,
		&res.Stats.Failing); err != nil {
		return nil, err
	}
	res.Stats.Enabled = res.Stats.Songs - res.Stats.Disabled
	if err := s.db.QueryRowContext(ctx, `SELECT count(*)`+from, args...).Scan(&res.Matched); err != nil {
		return nil, err
	}
	res.Pages = max(1, (res.Matched+q.Size-1)/q.Size)
	res.Page = min(max(q.Page, 1), res.Pages)
	rows, err := s.db.QueryContext(ctx, `SELECT c.x, c.entry, c.disabled, c.disabled_reason, c.added_at, c.updated_at,
		coalesce(r.ok, 0), coalesce(r.bad, 0)`+from+` ORDER BY `+order+` LIMIT ? OFFSET ?`,
		append(args, q.Size, (res.Page-1)*q.Size)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l CommunityListed
		var x, entry string
		var added, updated int64
		e := &CommunityEntry{}
		if err := rows.Scan(&x, &entry, &e.Disabled, &e.DisabledReason, &added, &updated, &l.ReportsOK,
			&l.ReportsFailed); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(entry), &e.Song); err != nil {
			return nil, fmt.Errorf("community entry %s: %w", x, err)
		}
		e.AddedAt, e.UpdatedAt = time.Unix(added, 0), time.Unix(updated, 0)
		l.CommunityEntry = e
		res.Items = append(res.Items, l)
	}
	return res, rows.Err()
}

// CommunityCount is the number of entries.
func (s *Store) CommunityCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM community_songs`).Scan(&n)
	return n, err
}

// RemoveCommunitySong deletes an entry and its reports.
func (s *Store) RemoveCommunitySong(ctx context.Context, x string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM community_songs WHERE x = ?`, x)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM community_reports WHERE x = ?`, x)
	return err
}

// SetCommunityDisabled disables an entry (resolve stops answering with it) or enables it again. Enabling clears
// the reports that disabled it.
func (s *Store) SetCommunityDisabled(ctx context.Context, x string, disabled bool, reason string) error {
	if !disabled {
		reason = ""
	}
	res, err := s.db.ExecContext(ctx, `UPDATE community_songs SET disabled = ?, disabled_reason = ? WHERE x = ?`,
		disabled, reason, x)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if !disabled {
		_, err = s.db.ExecContext(ctx, `DELETE FROM community_reports WHERE x = ? AND ok = 0`, x)
	}
	return err
}

const communityCols = `x, entry, disabled, disabled_reason, added_at, updated_at`

func scanCommunity(row interface{ Scan(...any) error }) (*CommunityEntry, error) {
	var e CommunityEntry
	var x, entry string
	var added, updated int64
	if err := row.Scan(&x, &entry, &e.Disabled, &e.DisabledReason, &added, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(entry), &e.Song); err != nil {
		return nil, fmt.Errorf("community entry %s: %w", x, err)
	}
	e.AddedAt, e.UpdatedAt = time.Unix(added, 0), time.Unix(updated, 0)
	return &e, nil
}

// CommunitySong is one entry, with the hashes seen for it.
func (s *Store) CommunitySong(ctx context.Context, x string) (*CommunityEntry, error) {
	e, err := scanCommunity(s.db.QueryRowContext(ctx, `SELECT `+communityCols+` FROM community_songs WHERE x = ?`, x))
	if err != nil {
		return nil, err
	}
	m := map[string]*CommunityEntry{strings.ToLower(e.X): e}
	return e, s.addSeenHashes(ctx, m)
}

// CommunitySongs looks up several shortnames at once; the map is keyed by the lower-cased shortname.
func (s *Store) CommunitySongs(ctx context.Context, xs []string) (map[string]*CommunityEntry, error) {
	out := map[string]*CommunityEntry{}
	if len(xs) == 0 {
		return out, nil
	}
	args := make([]any, len(xs))
	for i, x := range xs {
		args[i] = x
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+communityCols+` FROM community_songs WHERE x IN (?`+
		strings.Repeat(", ?", len(xs)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanCommunity(rows)
		if err != nil {
			return nil, err
		}
		out[strings.ToLower(e.X)] = e
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.addSeenHashes(ctx, out)
}

// ListCommunitySongs lists every entry, or those whose creator contains creator (any case), by shortname.
func (s *Store) ListCommunitySongs(ctx context.Context, creator string) ([]*CommunityEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+communityCols+` FROM community_songs
		WHERE instr(lower(creator), lower(?)) > 0 ORDER BY x`, creator)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*CommunityEntry
	for rows.Next() {
		e, err := scanCommunity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) addSeenHashes(ctx context.Context, m map[string]*CommunityEntry) error {
	for key, e := range m {
		rows, err := s.db.QueryContext(ctx, `SELECT sha256 FROM community_reports WHERE x = ? AND ok = 1
			GROUP BY sha256 ORDER BY min(created_at)`, e.X)
		if err != nil {
			return err
		}
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				rows.Close()
				return err
			}
			m[key].SeenSHA256 = append(m[key].SeenSHA256, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// CommunityReport is a loader's outcome for fetching an entry.
type CommunityReport struct {
	PlayerID  int64
	OK        bool
	Reason    string
	SHA256    string
	SigSHA256 string
	Size      int64
	At        time.Time
}

// AddCommunityReport records a player's latest outcome for x. Once CommunityDisableAfter different players have
// reported a bad file (layout or sig), the entry is disabled; disabled says whether this report did that.
func (s *Store) AddCommunityReport(ctx context.Context, x string, r CommunityReport) (disabled bool, err error) {
	var cur string
	if err := s.db.QueryRowContext(ctx, `SELECT x FROM community_songs WHERE x = ?`, x).Scan(&cur); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO community_reports
		(x, player_id, ok, reason, sha256, sig_sha256, size, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(x, player_id) DO UPDATE SET ok = excluded.ok, reason = excluded.reason, sha256 = excluded.sha256,
		sig_sha256 = excluded.sig_sha256, size = excluded.size, created_at = excluded.created_at`,
		cur, r.PlayerID, r.OK, r.Reason, r.SHA256, r.SigSHA256, r.Size, time.Now().Unix()); err != nil {
		return false, err
	}
	if r.OK || !isBadFileReason(r.Reason) {
		return false, nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM community_reports WHERE x = ? AND ok = 0
		AND reason IN ('layout', 'sig')`, cur).Scan(&n); err != nil {
		return false, err
	}
	if n < CommunityDisableAfter {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, `UPDATE community_songs SET disabled = 1, disabled_reason = ?
		WHERE x = ? AND disabled = 0`, fmt.Sprintf("%d players reported a bad file (%s)", n, r.Reason), cur)
	if err != nil {
		return false, err
	}
	changed, _ := res.RowsAffected()
	return changed > 0, nil
}

func isBadFileReason(reason string) bool {
	for _, r := range communityBadFileReasons {
		if r == reason {
			return true
		}
	}
	return false
}

// CommunityReports lists the latest report of each player for x, newest first.
func (s *Store) CommunityReports(ctx context.Context, x string) ([]CommunityReport, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT player_id, ok, reason, sha256, sig_sha256, size, created_at
		FROM community_reports WHERE x = ? ORDER BY created_at DESC`, x)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommunityReport
	for rows.Next() {
		var r CommunityReport
		var at int64
		if err := rows.Scan(&r.PlayerID, &r.OK, &r.Reason, &r.SHA256, &r.SigSHA256, &r.Size, &at); err != nil {
			return nil, err
		}
		r.At = time.Unix(at, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}
