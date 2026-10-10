// Package logx is fuserd's logging: output filtered by category, and pseudonymous tags in place of identifying
// values. Log lines may be shared, so nothing that identifies a player may appear in them verbatim.
package logx

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
)

// Category groups log lines by the part of the server they come from. API routes log under their module (the first
// path segment, upper-cased), except the polling endpoints, which log as Poll.
type Category string

const (
	Server      Category = "SERVER"  // startup and shutdown; always output
	Control     Category = "CONTROL" // the administrator panel's audit trail; always output
	HTTP        Category = "HTTP"    // requests for paths no route matches (clients and internet scanners)
	Poll        Category = "POLL"    // endpoints the client calls every few seconds while online
	Auth        Category = "AUTH"
	BI          Category = "BI"
	Events      Category = "EVENTS"
	Feed        Category = "FEED"
	Followship  Category = "FOLLOWSHIP"
	MainMenu    Category = "MAIN_MENU"
	MainStage   Category = "MAIN_STAGE"
	Multiplayer Category = "MULTIPLAYER"
	Players     Category = "PLAYERS"
	Recordings  Category = "RECORDINGS"
	Reporting   Category = "REPORTING"
	Search      Category = "SEARCH"
	Store       Category = "STORE"
	Video       Category = "VIDEO"
	P2P         Category = "P2P" // the STUN service and P2P signaling
	Web         Category = "WEB" // the landing page and the mod pack download
)

// All is every category, in the order the startup line lists them.
var All = []Category{Server, Control, HTTP, Poll, Auth, BI, Events, Feed, Followship, MainMenu, MainStage, Multiplayer,
	Players, Recordings, Reporting, Search, Store, Video, P2P, Web}

// Logs hands out a logger per category and computes tags.
type Logs struct {
	byCat   map[Category]*slog.Logger
	enabled []Category
	tagKey  []byte
}

// New builds the loggers. enabled lists the categories to output ("*" = all; empty = all). SERVER is always output,
// and so is anything logged at Error level, whatever its category. tagKey keys the tag HMAC.
func New(h slog.Handler, enabled []string, tagKey []byte) (*Logs, error) {
	return NewFiltered(h, enabled, nil, tagKey)
}

// NewFiltered is New with categories switched off: disabled (log_disabled_categories) removes categories from what
// enabled selects, e.g. everything but POLL. SERVER can't be switched off, and errors are still output.
func NewFiltered(h slog.Handler, enabled, disabled []string, tagKey []byte) (*Logs, error) {
	on := map[Category]bool{Server: true, Control: true}
	if len(enabled) == 0 {
		enabled = []string{"*"}
	}
	for _, name := range enabled {
		if name == "*" {
			for _, c := range All {
				on[c] = true
			}
			continue
		}
		c, err := category("log_categories", name)
		if err != nil {
			return nil, err
		}
		on[c] = true
	}
	for _, name := range disabled {
		c, err := category("log_disabled_categories", name)
		if err != nil {
			return nil, err
		}
		if c == Server || c == Control {
			return nil, fmt.Errorf("log_disabled_categories: %s is always output", c)
		}
		on[c] = false
	}

	l := &Logs{byCat: map[Category]*slog.Logger{}, tagKey: tagKey}
	for _, c := range All {
		if on[c] {
			l.enabled = append(l.enabled, c)
			l.byCat[c] = slog.New(h).With("cat", string(c))
		} else {
			l.byCat[c] = slog.New(floor{h, slog.LevelError}).With("cat", string(c))
		}
	}
	return l, nil
}

// category parses a category name from the configuration (case-insensitive); a misspelt one is an error.
func category(setting, name string) (Category, error) {
	c := Category(strings.ToUpper(strings.TrimSpace(name)))
	if !slices.Contains(All, c) {
		return "", fmt.Errorf("%s: unknown category %q (known: %s)", setting, name, joinCats(All))
	}
	return c, nil
}

// For returns the logger for c.
func (l *Logs) For(c Category) *slog.Logger {
	if lg, ok := l.byCat[c]; ok {
		return lg
	}
	return l.byCat[Server].With("cat", string(c))
}

// Enabled lists the categories being output, for the startup line.
func (l *Logs) Enabled() string { return joinCats(l.enabled) }

func joinCats(cs []Category) string {
	s := make([]string, len(cs))
	for i, c := range cs {
		s[i] = string(c)
	}
	return strings.Join(s, ",")
}

// floor drops records below min: a category that is switched off still reports errors.
type floor struct {
	slog.Handler
	min slog.Level
}

func (f floor) Enabled(ctx context.Context, lv slog.Level) bool {
	return lv >= f.min && f.Handler.Enabled(ctx, lv)
}
func (f floor) WithAttrs(as []slog.Attr) slog.Handler { return floor{f.Handler.WithAttrs(as), f.min} }
func (f floor) WithGroup(name string) slog.Handler    { return floor{f.Handler.WithGroup(name), f.min} }

// DeriveTagKey derives the tag key from the token secret when no log_tag_secret is configured. The derivation keeps
// tags from ever being truncated signatures made with the token-signing key itself.
func DeriveTagKey(tokenSecret string) []byte {
	m := hmac.New(sha256.New, []byte(tokenSecret))
	m.Write([]byte("fuserd log tag key"))
	return m.Sum(nil)
}

// Tag is the log pseudonym for an identifying value: the first 12 hex characters of HMAC-SHA-256(key, v). It is
// stable for as long as the key is, so lines about one player can be correlated. Without the key it cannot be
// reversed by hashing guesses, which a plain SHA-256 of a SteamID or a small player ID could be.
func (l *Logs) Tag(v string) string {
	if v == "" {
		return ""
	}
	m := hmac.New(sha256.New, l.tagKey)
	m.Write([]byte(v))
	return hex.EncodeToString(m.Sum(nil))[:12]
}

// TagID tags a numeric ID. It matches the tag of the same ID in a JSON body (TagJSON).
func (l *Logs) TagID(id int64) string { return l.Tag(strconv.FormatInt(id, 10)) }

// sensitiveKeys are JSON keys whose values identify a player or are credentials. Keys are compared lower-cased
// with underscores removed, so "platform_user_id", "platformUserId" and "PlatformUserID" all match. Add to this as
// endpoints are implemented.
var sensitiveKeys = map[string]bool{
	"token":            true,
	"platformuserid":   true,
	"platformusername": true,
	"userid":           true,
	"hypeuserid":       true,
	"hypeusername":     true,
	"username":         true,
	"encodeduserid":    true,
	"ipaddress":        true,
	"ipadress":         true, // FGameSessionMetadata.IPAdress (sic)
	"playerid":         true,
	"targetuserid":     true,
	"fromuserid":       true,
	"search":           true, // username searches
	"playerids":        true,
	"leaderhypeuserid": true,
	"leaderusername":   true,
	"friendslist":      true,
	"blockedlist":      true,
}

// nestedJSONKeys hold a JSON document inside a string (GameLift's matchmaker_data). Their contents are tagged too.
var nestedJSONKeys = map[string]bool{"matchmakerdata": true}

func sensitive(key string) bool {
	return sensitiveKeys[strings.ReplaceAll(strings.ToLower(key), "_", "")]
}

// TagJSON renders a request or response body for the log, with the value of every sensitive key, at any depth,
// replaced by its tag. A body that is not JSON is not logged, only its size.
func (l *Logs) TagJSON(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // keep "user_id": 5 as "5", so its tag matches TagID(5)
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Sprintf("(non-JSON body, %d bytes)", len(body))
	}
	l.walk(v)
	b, _ := json.Marshal(v)
	return string(b)
}

func (l *Logs) walk(x any) {
	switch t := x.(type) {
	case map[string]any:
		for k, v := range t {
			if sensitive(k) {
				t[k] = l.tagAll(v)
			} else if str, ok := v.(string); ok && nestedJSONKeys[strings.ReplaceAll(strings.ToLower(k), "_", "")] {
				t[k] = l.TagJSON([]byte(str))
			} else {
				l.walk(v)
			}
		}
	case []any:
		for _, e := range t {
			l.walk(e)
		}
	}
}

// tagAll replaces every string and number under x with its tag.
func (l *Logs) tagAll(x any) any {
	switch t := x.(type) {
	case string:
		return l.Tag(t)
	case json.Number:
		return l.Tag(t.String())
	case []any:
		for i := range t {
			t[i] = l.tagAll(t[i])
		}
	case map[string]any:
		for k, v := range t {
			t[k] = l.tagAll(v)
		}
	}
	return x
}
