package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fuseronline/internal/community"
	"fuseronline/internal/config"
	"fuseronline/internal/store"
)

func controlCommunitySongFor(x string) community.Song {
	return community.Song{X: x, Kind: community.KindArchive, Archive: "zip",
		Files:    []community.File{{ID: "1wJNLylzC6twJujxImriBGD5cRSE4ogW0", Name: x + ".zip", Size: 9_000_000}},
		PakEntry: "dir/" + x + "_P.pak", SigEntry: "dir/" + x + "_P.sig",
		Link:   "https://drive.google.com/file/d/1wJNLylzC6twJujxImriBGD5cRSE4ogW0/view",
		Artist: "Artist " + x, Title: "Title " + x, Creator: "SheepQueen"}
}

func TestControlCommunity(t *testing.T) {
	_, s := newTestServerAndAPI(t, nil)
	s.cfg.Control = config.ControlConfig{Enabled: true, Origin: "https://control.example.org"}
	h, err := s.ControlHandler()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	token, _, _ := s.store.ControlToken(ctx, "operator", time.Minute, store.ControlCLI)
	if err := s.store.ControlEnroll(ctx, store.ControlHash(token), store.ControlAdmin{ID: "op", Name: "operator", CredentialID: "c", Credential: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.ControlSession(ctx, store.ControlHash("session"), "op", time.Now().Add(time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	do := func(method, path string, body []byte, signedIn bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Origin", s.cfg.Control.Origin)
		if signedIn {
			r.AddCookie(&http.Cookie{Name: "__Host-fuser_control", Value: "session"})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	ok := func(w *httptest.ResponseRecorder, what string) map[string]any {
		t.Helper()
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", what, w.Code, w.Body.String())
		}
		var v map[string]any
		json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}
	encode := func(s community.Song) []byte { b, _ := json.Marshal(s); return b }

	if w := do("GET", "/control/api/community", nil, false); w.Code != 401 {
		t.Fatalf("signed out list: %d", w.Code)
	}
	r := httptest.NewRequest("POST", "/control/api/community", bytes.NewReader(encode(controlCommunitySongFor("No_Origin"))))
	r.AddCookie(&http.Cookie{Name: "__Host-fuser_control", Value: "session"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("add without origin: %d", w.Code)
	}

	// Add, refuse duplicates and official shortnames.
	ok(do("POST", "/control/api/community", encode(controlCommunitySongFor("Song_A")), true), "add")
	if w := do("POST", "/control/api/community", encode(controlCommunitySongFor("song_a")), true); w.Code != 409 {
		t.Fatalf("duplicate add: %d %s", w.Code, w.Body.String())
	}
	if w := do("POST", "/control/api/community", encode(controlCommunitySongFor("countingstars")), true); w.Code != 400 || !strings.Contains(w.Body.String(), "official") {
		t.Fatalf("official add: %d %s", w.Code, w.Body.String())
	}
	if w := do("POST", "/control/api/community", []byte(`{"x":"A","bogus":1}`), true); w.Code != 400 {
		t.Fatalf("unknown field: %d", w.Code)
	}

	// Import: a dry run changes nothing; applying adds and updates in one go.
	changedA := controlCommunitySongFor("Song_A")
	changedA.Note = "re-checked"
	var batch strings.Builder
	batch.WriteString("# re-sync\n")
	batch.Write(encode(changedA))
	batch.WriteString("\n")
	for i := 0; i < 60; i++ {
		batch.Write(encode(controlCommunitySongFor(fmt.Sprintf("Batch_%02d", i))))
		batch.WriteString("\n")
	}
	batch.WriteString(`{"x": "Broken", "kind": "fol` + "\n")
	v := ok(do("POST", "/control/api/community-import?mode=add&dry_run=1", []byte(batch.String()), true), "dry run")
	counts := v["counts"].(map[string]any)
	if counts["added"] != 60.0 || counts["kept"] != 1.0 || counts["refused"] != 1.0 || len(v["rows"].([]any)) != 62 {
		t.Fatalf("dry run: %v", counts)
	}
	if n, _ := s.store.CommunityCount(ctx); n != 1 {
		t.Fatalf("dry run stored entries: %d", n)
	}
	v = ok(do("POST", "/control/api/community-import?mode=update&dry_run=0&name=sync.jsonl", []byte(batch.String()), true), "import")
	if counts := v["counts"].(map[string]any); counts["added"] != 60.0 || counts["updated"] != 1.0 || v["dry_run"] != false {
		t.Fatalf("import: %v", v["counts"])
	}
	if e, _ := s.store.CommunitySong(ctx, "Song_A"); e == nil || e.Note != "re-checked" {
		t.Fatalf("updated entry: %+v", e)
	}
	audit, _ := s.store.ControlAudit(ctx, 1)
	if len(audit) != 1 || audit[0].Action != "community.import" || audit[0].Target != "sync.jsonl" ||
		audit[0].Detail != "mode=update added=60 updated=1 unchanged=0 kept=0 refused=1" {
		t.Fatalf("import audit: %+v", audit)
	}
	if w := do("POST", "/control/api/community-import?mode=add", bytes.Repeat([]byte(" "), CommunityImportMaxBytes+1), true); w.Code != 413 {
		t.Fatalf("oversized import: %d", w.Code)
	}
	if w := do("POST", "/control/api/community-import?mode=sometimes", []byte(batch.String()), true); w.Code != 400 {
		t.Fatalf("bad mode: %d", w.Code)
	}

	// List: stats, search, filters and paging.
	v = ok(do("GET", "/control/api/community?size=25&page=3&sort=x", nil, true), "list")
	stats := v["stats"].(map[string]any)
	if stats["songs"] != 61.0 || v["matched"] != 61.0 || v["pages"] != 3.0 || v["page"] != 3.0 || len(v["items"].([]any)) != 11 {
		t.Fatalf("list: stats %v matched %v pages %v page %v", stats, v["matched"], v["pages"], v["page"])
	}
	v = ok(do("GET", "/control/api/community?q=batch_0&state=enabled&kind=archive", nil, true), "search")
	if v["matched"] != 10.0 {
		t.Fatalf("search matched %v", v["matched"])
	}
	for _, q := range []string{"size=251", "page=0", "state=odd", "kind=zip", "sort=random"} {
		if w := do("GET", "/control/api/community?"+q, nil, true); w.Code != 400 {
			t.Errorf("%s: %d", q, w.Code)
		}
	}

	// Edit, disable, enable, detail.
	changedA.Note = "edited in the panel"
	ok(do("PUT", "/control/api/community/Song_A", encode(changedA), true), "update")
	if w := do("PUT", "/control/api/community/Batch_01", encode(changedA), true); w.Code != 400 {
		t.Fatalf("renaming update: %d", w.Code)
	}
	if w := do("PUT", "/control/api/community/Nobody", encode(controlCommunitySongFor("Nobody")), true); w.Code != 404 {
		t.Fatalf("update of a missing entry: %d", w.Code)
	}
	ok(do("POST", "/control/api/community/Song_A/disable", []byte(`{"reason":"dead link"}`), true), "disable")
	v = ok(do("GET", "/control/api/community?state=disabled", nil, true), "disabled list")
	item := v["items"].([]any)[0].(map[string]any)
	if v["matched"] != 1.0 || item["x"] != "Song_A" || item["disabled_reason"] != "dead link" {
		t.Fatalf("disabled list: %v", v)
	}
	ok(do("POST", "/control/api/community/Song_A/enable", nil, true), "enable")
	v = ok(do("GET", "/control/api/community/Song_A", nil, true), "detail")
	if song := v["song"].(map[string]any); song["note"] != "edited in the panel" || song["disabled"] != false {
		t.Fatalf("detail: %v", v)
	}
	audit, _ = s.store.ControlAudit(ctx, 3)
	if audit[0].Action != "community.enable" || audit[1].Action != "community.disable" || audit[1].Detail != "dead link" ||
		audit[2].Action != "community.update" || audit[2].Detail != "changes note" {
		t.Fatalf("edit audit: %+v", audit)
	}

	// Export is the import format.
	w = do("GET", "/control/api/community-export", nil, true)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), ".jsonl") {
		t.Fatalf("export: %d %v", w.Code, w.Header())
	}
	exported := community.ParseBatch(w.Body.Bytes(), community.FormatJSONL)
	if len(exported) != 61 || exported[0].Err != nil {
		t.Fatalf("export: %d entries", len(exported))
	}

	// Remove.
	ok(do("DELETE", "/control/api/community/Song_A", nil, true), "remove")
	if w := do("GET", "/control/api/community/Song_A", nil, true); w.Code != 404 {
		t.Fatalf("removed detail: %d", w.Code)
	}
	if w := do("DELETE", "/control/api/community/Song_A", nil, true); w.Code != 404 {
		t.Fatalf("second remove: %d", w.Code)
	}
	v = ok(do("GET", "/control/api/state", nil, true), "state")
	if v["community_songs"] != 60.0 {
		t.Fatalf("state count: %v", v["community_songs"])
	}
}
