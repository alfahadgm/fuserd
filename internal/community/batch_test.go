package community

import (
	"encoding/json"
	"strings"
	"testing"
)

func line(s Song) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestParseBatch(t *testing.T) {
	a, b := archiveSong(), archiveSong()
	b.X, b.PakEntry, b.SigEntry = "Other_X", "Other_X_P.pak", "Other_X_P.sig"
	jsonl := "\xef\xbb\xbf# comment\n\n" + line(a) + "\r\n" + `{"x": "T", "titel": "typo"}` + "\n" + line(b) + "\n"
	items := ParseBatch([]byte(jsonl), "")
	if len(items) != 3 || items[0].Line != "line 3" || items[0].Err != nil || items[1].Err == nil || items[2].Song.X != "Other_X" {
		t.Fatalf("jsonl: %+v", items)
	}
	arr := "[" + line(a) + ",\n " + line(b) + "]"
	items = ParseBatch([]byte(arr), "")
	if len(items) != 2 || items[1].Line != "item 2" || items[1].Err != nil {
		t.Fatalf("json array: %+v", items)
	}
	pretty, _ := json.MarshalIndent(a, "", "  ")
	items = ParseBatch(pretty, "")
	if len(items) != 1 || items[0].Line != "file" || items[0].Err != nil || items[0].Song.X != a.X {
		t.Fatalf("single pretty-printed entry: %+v", items)
	}
	// One JSON Lines entry is also one JSON value; either reading gives the same entry.
	if items := ParseBatch([]byte(line(a)), FormatJSONL); len(items) != 1 || items[0].Err != nil {
		t.Fatalf("single line: %+v", items)
	}
	if items := ParseBatch([]byte("[1, "), FormatJSON); len(items) != 1 || items[0].Err == nil {
		t.Fatalf("broken array: %+v", items)
	}
	if items := ParseBatch([]byte("# only comments\n\n"), ""); len(items) != 0 {
		t.Fatalf("comments only: %+v", items)
	}
}

func TestPlan(t *testing.T) {
	stored := archiveSong()
	changed := stored
	changed.Note = "permission confirmed"
	fresh := archiveSong()
	fresh.X, fresh.PakEntry, fresh.SigEntry = "Fresh_X", "Fresh_X_P.pak", "Fresh_X_P.sig"
	official := fresh
	official.X, official.PakEntry, official.SigEntry = "BadGuy", "BadGuy_P.pak", "BadGuy_P.sig"
	existing := map[string]string{strings.ToLower(stored.X): line(stored)}
	check := NewChecker([]string{"badguy"}).Check
	body := strings.Join([]string{line(stored), line(changed), line(fresh), line(fresh), line(official), "{"}, "\n")
	results := func(update bool) string {
		var out []string
		for _, r := range Plan(ParseBatch([]byte(body), ""), existing, update, check) {
			out = append(out, r.Result)
		}
		return strings.Join(out, " ")
	}
	if got := results(false); got != "unchanged kept added unchanged refused refused" {
		t.Errorf("add only: %s", got)
	}
	// With update the changed line replaces the stored one; a repeated line is then compared with it.
	if got := results(true); got != "unchanged updated added unchanged refused refused" {
		t.Errorf("update: %s", got)
	}
	rows := Plan(ParseBatch([]byte(line(changed)), ""), existing, true, check)
	if rows[0].Detail != "changes note" || rows[0].Song == nil || rows[0].Song.Note != "permission confirmed" {
		t.Errorf("update row: %+v", rows[0])
	}
	rows = Plan(ParseBatch([]byte(line(official)), ""), existing, true, check)
	if !strings.Contains(rows[0].Detail, "official") || rows[0].Song != nil {
		t.Errorf("official row: %+v", rows[0])
	}
}
