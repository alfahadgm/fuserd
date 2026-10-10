package api

// The control panel's Community songs tab: a paged, filtered list of the community song table, each entry's details
// and reports, edits, and bulk imports and exports in the "fuserd community import" format. The rules are the CLI's:
// official shortnames and rows credited to Harmonix are refused.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"fuseronline/internal/community"
	"fuseronline/internal/control"
	"fuseronline/internal/store"
)

const (
	communityPageSize    = 50
	communityPageSizeMax = 250
	// CommunityImportMaxBytes caps a bulk import upload. A full re-sync of the >12,000-entry sheet is ~6 MB.
	CommunityImportMaxBytes = 32 << 20
	// communityTransferTime is how long an import upload or export download may take: the server's own read and write
	// timeouts (30 s) are too short for a 32 MB upload on a slow link.
	communityTransferTime = 5 * time.Minute
	communityReportsShown = 20
	communityReasonMax    = 200
)

// controlCommunitySong is an entry as the panel lists it. Times are wall times in the session's timezone.
type controlCommunitySong struct {
	community.Song
	Disabled       bool   `json:"disabled"`
	DisabledReason string `json:"disabled_reason"`
	Added          string `json:"added"`
	Updated        string `json:"updated"`
	ReportsOK      int    `json:"reports_ok"`
	ReportsFailed  int    `json:"reports_failed"`
}

type controlCommunityReport struct {
	At     string `json:"at"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func communityListed(e *store.CommunityEntry, ok, failed int, loc *time.Location) controlCommunitySong {
	return controlCommunitySong{Song: e.Song, Disabled: e.Disabled, DisabledReason: e.DisabledReason,
		Added: store.ControlWall(e.AddedAt, loc), Updated: store.ControlWall(e.UpdatedAt, loc), ReportsOK: ok, ReportsFailed: failed}
}

func (s *Server) communityChecker() community.Checker {
	return community.NewChecker(s.cfg.SongManifest.Songs, s.cfg.Store.Reserved)
}

// inCatalogue reports whether the catalogue has x: it answers resolve first, so the community entry is only a
// fallback for it.
func (s *Server) inCatalogue(x string) bool {
	c := s.cat()
	if c == nil {
		return false
	}
	_, ok := c.ByShortName(x)
	return ok
}

// communityQuery reads the list parameters: q, state (all|enabled|disabled|failing), kind (all|archive|folder),
// sort (updated|artist|creator|x), page (from 1) and size (1..250, default 50).
func communityQuery(r *http.Request) (store.CommunityQuery, error) {
	v := r.URL.Query()
	q := store.CommunityQuery{Search: strings.TrimSpace(v.Get("q")), State: v.Get("state"), Kind: v.Get("kind"),
		Sort: v.Get("sort"), Page: 1, Size: communityPageSize}
	if len(q.Search) > 200 {
		return q, errors.New("search is too long")
	}
	switch q.State {
	case "all":
		q.State = ""
	case "", "enabled", "disabled", "failing":
	default:
		return q, errors.New("state must be all, enabled, disabled or failing")
	}
	switch q.Kind {
	case "all":
		q.Kind = ""
	case "", community.KindArchive, community.KindFolder:
	default:
		return q, errors.New("kind must be all, archive or folder")
	}
	switch q.Sort {
	case "", "updated", "artist", "creator", "x":
	default:
		return q, errors.New("sort must be updated, artist, creator or x")
	}
	for name, dst := range map[string]*int{"page": &q.Page, "size": &q.Size} {
		if raw := v.Get(name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				return q, fmt.Errorf("%s must be a positive number", name)
			}
			*dst = n
		}
	}
	if q.Size > communityPageSizeMax {
		return q, fmt.Errorf("size must be at most %d", communityPageSizeMax)
	}
	return q, nil
}

// controlCommunityRoutes registers the tab's endpoints. Edits hold edits, as the panel's other changes do.
func (s *Server) controlCommunityRoutes(protect func(string, http.HandlerFunc), auth *control.Auth, edits *sync.Mutex) {
	protect("GET /control/api/community", func(w http.ResponseWriter, r *http.Request) {
		q, err := communityQuery(r)
		if err != nil {
			control.Fail(w, 400, err.Error())
			return
		}
		res, err := s.store.CommunityPage(r.Context(), q)
		if err != nil {
			control.Fail(w, 500, "could not list community songs")
			return
		}
		loc := control.Location(r.Context())
		items := make([]controlCommunitySong, len(res.Items))
		for i, it := range res.Items {
			items[i] = communityListed(it.CommunityEntry, it.ReportsOK, it.ReportsFailed, loc)
		}
		control.JSON(w, 200, map[string]any{"stats": res.Stats, "matched": res.Matched, "page": res.Page, "pages": res.Pages,
			"size": q.Size, "items": items})
	})
	protect("GET /control/api/community/{x}", func(w http.ResponseWriter, r *http.Request) {
		ctx, x := r.Context(), r.PathValue("x")
		e, err := s.store.CommunitySong(ctx, x)
		if err != nil {
			communityFail(w, err)
			return
		}
		reports, err := s.store.CommunityReports(ctx, e.X)
		if err != nil {
			control.Fail(w, 500, "could not load reports")
			return
		}
		loc := control.Location(ctx)
		ok, failed := 0, 0
		shown := []controlCommunityReport{}
		for _, rep := range reports {
			if rep.OK {
				ok++
			} else {
				failed++
			}
			if len(shown) < communityReportsShown {
				shown = append(shown, controlCommunityReport{store.ControlWall(rep.At, loc), rep.OK, rep.Reason, rep.SHA256, rep.Size})
			}
		}
		control.JSON(w, 200, map[string]any{"song": communityListed(e, ok, failed, loc), "reports": shown,
			"seen_sha256": append([]string{}, e.SeenSHA256...), "in_catalogue": s.inCatalogue(e.X)})
	})
	protect("POST /control/api/community", func(w http.ResponseWriter, r *http.Request) {
		song, ok := decodeCommunitySong(w, r)
		if !ok {
			return
		}
		if err := s.communityChecker().Check(&song); err != nil {
			control.Fail(w, 400, err.Error())
			return
		}
		edits.Lock()
		defer edits.Unlock()
		res, err := s.store.PutCommunitySong(r.Context(), song, false)
		if errors.Is(err, store.ErrCommunityExists) {
			control.Fail(w, 409, song.X+" is already listed; edit it instead")
			return
		}
		if err != nil {
			control.Fail(w, 400, err.Error())
			return
		}
		auth.Audit(r, "community.add", song.X, strings.TrimSpace(song.Kind+" "+song.Archive))
		control.JSON(w, 200, map[string]any{"result": res, "in_catalogue": s.inCatalogue(song.X)})
	})
	protect("PUT /control/api/community/{x}", func(w http.ResponseWriter, r *http.Request) {
		song, ok := decodeCommunitySong(w, r)
		if !ok {
			return
		}
		if !strings.EqualFold(song.X, r.PathValue("x")) {
			control.Fail(w, 400, "the shortname can't change; remove the entry and add it again")
			return
		}
		if err := s.communityChecker().Check(&song); err != nil {
			control.Fail(w, 400, err.Error())
			return
		}
		edits.Lock()
		defer edits.Unlock()
		ctx := r.Context()
		old, err := s.store.CommunitySong(ctx, song.X)
		if err != nil {
			communityFail(w, err)
			return
		}
		res, err := s.store.PutCommunitySong(ctx, song, true)
		if err != nil {
			control.Fail(w, 400, err.Error())
			return
		}
		if res != store.CommunityUnchanged {
			before, _ := json.Marshal(old.Song)
			after, _ := json.Marshal(song)
			auth.Audit(r, "community.update", song.X, "changes "+strings.Join(community.ChangedFields(string(before), string(after)), ", "))
		}
		control.JSON(w, 200, map[string]any{"result": res, "in_catalogue": s.inCatalogue(song.X)})
	})
	protect("DELETE /control/api/community/{x}", func(w http.ResponseWriter, r *http.Request) {
		edits.Lock()
		defer edits.Unlock()
		x := r.PathValue("x")
		if err := s.store.RemoveCommunitySong(r.Context(), x); err != nil {
			communityFail(w, err)
			return
		}
		auth.Audit(r, "community.remove", x, "")
		control.JSON(w, 200, map[string]bool{"ok": true})
	})
	protect("POST /control/api/community/{x}/{action}", func(w http.ResponseWriter, r *http.Request) {
		x, action := r.PathValue("x"), r.PathValue("action")
		if action != "disable" && action != "enable" {
			control.Fail(w, 404, "not found")
			return
		}
		var req struct {
			Reason string `json:"reason"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := decodeControl(r, &req); err != nil && !errors.Is(err, io.EOF) {
			control.Fail(w, 400, "invalid request")
			return
		}
		reason := strings.TrimSpace(req.Reason)
		if reason == "" {
			reason = "disabled by an operator"
		}
		if len(reason) > communityReasonMax {
			control.Fail(w, 400, fmt.Sprintf("the reason must be at most %d characters", communityReasonMax))
			return
		}
		edits.Lock()
		defer edits.Unlock()
		if err := s.store.SetCommunityDisabled(r.Context(), x, action == "disable", reason); err != nil {
			communityFail(w, err)
			return
		}
		detail := ""
		if action == "disable" {
			detail = reason
		}
		auth.Audit(r, "community."+action, x, detail)
		control.JSON(w, 200, map[string]bool{"ok": true})
	})
	protect("POST /control/api/community-import", func(w http.ResponseWriter, r *http.Request) {
		s.controlCommunityImport(w, r, auth, edits)
	})
	protect("GET /control/api/community-export", func(w http.ResponseWriter, r *http.Request) {
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(communityTransferTime))
		entries, err := s.store.ListCommunitySongs(r.Context(), "")
		if err != nil {
			control.Fail(w, 500, "could not export community songs")
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="community_songs_`+time.Now().UTC().Format("20060102")+`.jsonl"`)
		w.Header().Set("Cache-Control", "no-store")
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		for _, e := range entries {
			if err := enc.Encode(e.Song); err != nil {
				return
			}
		}
	})
}

// controlCommunityImport plans a batch (query: mode=add|update, dry_run=1|0, format=jsonl|json, empty to tell from the
// body, and name for the audit trail) and, unless it's a dry run, applies its added and updated entries in one
// transaction. The response lists every entry's result.
func (s *Server) controlCommunityImport(w http.ResponseWriter, r *http.Request, auth *control.Auth, edits *sync.Mutex) {
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(communityTransferTime))
	rc.SetWriteDeadline(time.Now().Add(communityTransferTime))
	v := r.URL.Query()
	mode, format, dryRun := v.Get("mode"), v.Get("format"), v.Get("dry_run") != "0"
	if mode != "add" && mode != "update" {
		control.Fail(w, 400, "mode must be add or update")
		return
	}
	if format != "" && format != community.FormatJSONL && format != community.FormatJSON {
		control.Fail(w, 400, "format must be jsonl or json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, CommunityImportMaxBytes))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		control.Fail(w, 413, fmt.Sprintf("the file is larger than %d MB", CommunityImportMaxBytes>>20))
		return
	}
	if err != nil {
		control.Fail(w, 400, "could not read the upload")
		return
	}
	items := community.ParseBatch(body, format)
	if len(items) == 0 {
		control.Fail(w, 400, "the file has no entries")
		return
	}
	edits.Lock()
	defer edits.Unlock()
	ctx := r.Context()
	existing, err := s.store.CommunityEntriesJSON(ctx)
	if err != nil {
		control.Fail(w, 500, "could not load community songs")
		return
	}
	check := s.communityChecker()
	rows := community.Plan(items, existing, mode == "update", check.Check)
	counts := map[string]int{community.ResultAdded: 0, community.ResultUpdated: 0, community.ResultUnchanged: 0,
		community.ResultKept: 0, community.ResultRefused: 0}
	var songs []community.Song
	for _, row := range rows {
		counts[row.Result]++
		if row.Song != nil {
			songs = append(songs, *row.Song)
		}
	}
	if !dryRun && len(songs) > 0 {
		if _, err := s.store.PutCommunitySongs(ctx, songs, mode == "update"); err != nil {
			control.Fail(w, 409, "nothing was imported: "+err.Error())
			return
		}
		name := strings.TrimSpace(v.Get("name"))
		if len(name) > 100 {
			name = name[:100]
		}
		auth.Audit(r, "community.import", name, fmt.Sprintf("mode=%s added=%d updated=%d unchanged=%d kept=%d refused=%d",
			mode, counts[community.ResultAdded], counts[community.ResultUpdated], counts[community.ResultUnchanged],
			counts[community.ResultKept], counts[community.ResultRefused]))
	}
	control.JSON(w, 200, map[string]any{"dry_run": dryRun, "mode": mode, "counts": counts, "rows": rows})
}

func decodeCommunitySong(w http.ResponseWriter, r *http.Request) (community.Song, bool) {
	var song community.Song
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := decodeControl(r, &song); err != nil {
		control.Fail(w, 400, "invalid entry: "+err.Error())
		return song, false
	}
	return song, true
}

func communityFail(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		control.Fail(w, 404, "no community song has that shortname")
		return
	}
	control.Fail(w, 500, "could not update community songs")
}
