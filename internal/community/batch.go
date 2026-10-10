package community

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Checker refuses entries fuserd must not list: official shortnames, and anything Validate rejects.
type Checker struct{ reserved map[string]bool }

// NewChecker refuses the shortnames in each list (the song manifest's and store.reserved_shortnames), in any case.
func NewChecker(reserved ...[]string) Checker {
	c := Checker{reserved: map[string]bool{}}
	for _, list := range reserved {
		for _, sn := range list {
			c.reserved[strings.ToLower(sn)] = true
		}
	}
	return c
}

// Check validates s and refuses an official shortname.
func (c Checker) Check(s *Song) error {
	if c.reserved[strings.ToLower(s.X)] {
		return fmt.Errorf("%s is an official song's shortname", s.X)
	}
	if err := s.Validate(); err != nil {
		return fmt.Errorf("%s: %w", s.X, err)
	}
	return nil
}

// Batch formats: JSON Lines (one entry per line, as "fuserd community import" reads) or JSON (one entry, or an
// array of entries).
const (
	FormatJSONL = "jsonl"
	FormatJSON  = "json"
)

// BatchItem is one entry of a batch, or why it couldn't be read.
type BatchItem struct {
	Line string // "line 12" (JSON Lines), "item 3" (a JSON array) or "file" (a single JSON entry)
	Song Song
	Err  error
}

// ParseBatch reads a batch. format is FormatJSONL, FormatJSON or "" to tell from the content: an array, or a body
// holding exactly one JSON value, is JSON. In JSON Lines, blank lines and lines starting with # are skipped. Unknown
// fields are refused, as the CLI does.
func ParseBatch(body []byte, format string) []BatchItem {
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	trimmed := bytes.TrimSpace(body)
	if format == "" {
		format = FormatJSONL
		if bytes.HasPrefix(trimmed, []byte("[")) || (bytes.HasPrefix(trimmed, []byte("{")) && json.Valid(trimmed)) {
			format = FormatJSON
		}
	}
	var items []BatchItem
	if format == FormatJSON {
		if bytes.HasPrefix(trimmed, []byte("[")) {
			var raw []json.RawMessage
			if err := json.Unmarshal(trimmed, &raw); err != nil {
				return []BatchItem{{Line: "file", Err: fmt.Errorf("not a JSON array of entries: %w", err)}}
			}
			for i, r := range raw {
				items = append(items, decodeItem(fmt.Sprintf("item %d", i+1), r))
			}
			return items
		}
		return []BatchItem{decodeItem("file", trimmed)}
	}
	for i, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		items = append(items, decodeItem(fmt.Sprintf("line %d", i+1), line))
	}
	return items
}

func decodeItem(label string, raw []byte) BatchItem {
	it := BatchItem{Line: label}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&it.Song); err != nil {
		it.Err = err
	} else if dec.More() {
		it.Err = fmt.Errorf("more than one JSON value")
	}
	return it
}

// Results of planning a batch entry.
const (
	ResultAdded     = "added"
	ResultUpdated   = "updated"
	ResultUnchanged = "unchanged"
	ResultKept      = "kept" // exists and differs, but the batch doesn't update
	ResultRefused   = "refused"
)

// PlanRow is what applying a batch entry does.
type PlanRow struct {
	Line   string `json:"line"`
	X      string `json:"x"`
	Result string `json:"result"`
	Detail string `json:"detail"`
	Song   *Song  `json:"-"` // added and updated rows: the entry to store
}

// Plan works out each entry's result against existing, the stored entries as JSON keyed by lower-cased shortname.
// Entries apply in order, so a later line for the same shortname is compared with the earlier one. With update,
// changed entries replace the stored ones; without, they're kept.
func Plan(items []BatchItem, existing map[string]string, update bool, check func(*Song) error) []PlanRow {
	working := map[string]string{}
	current := func(x string) (string, bool) {
		if v, ok := working[x]; ok {
			return v, true
		}
		v, ok := existing[x]
		return v, ok
	}
	rows := make([]PlanRow, 0, len(items))
	for _, it := range items {
		row := PlanRow{Line: it.Line, X: it.Song.X}
		if it.Err != nil {
			row.Result, row.Detail = ResultRefused, "not a valid entry: "+it.Err.Error()
			rows = append(rows, row)
			continue
		}
		s := it.Song
		if err := check(&s); err != nil {
			row.Result, row.Detail = ResultRefused, err.Error()
			rows = append(rows, row)
			continue
		}
		raw, _ := json.Marshal(s)
		key := strings.ToLower(s.X)
		old, ok := current(key)
		switch {
		case !ok:
			row.Result, row.Song = ResultAdded, &s
			row.Detail = strings.TrimSpace(s.Kind+" "+s.Archive) + " · " + s.Artist + " – " + s.Title
			working[key] = string(raw)
		case old == string(raw):
			row.Result = ResultUnchanged
		case !update:
			row.Result, row.Detail = ResultKept, "differs in "+strings.Join(ChangedFields(old, string(raw)), ", ")
		default:
			row.Result, row.Song = ResultUpdated, &s
			row.Detail = "changes " + strings.Join(ChangedFields(old, string(raw)), ", ")
			working[key] = string(raw)
		}
		rows = append(rows, row)
	}
	return rows
}

// ChangedFields lists the JSON fields that differ between two encoded entries.
func ChangedFields(a, b string) []string {
	var ma, mb map[string]json.RawMessage
	json.Unmarshal([]byte(a), &ma)
	json.Unmarshal([]byte(b), &mb)
	var out []string
	for _, k := range []string{"x", "kind", "archive", "files", "pak_entry", "sig_entry", "link", "artist", "title",
		"creator", "submitted", "sheet", "note"} {
		if !bytes.Equal(ma[k], mb[k]) {
			out = append(out, k)
		}
	}
	return out
}
