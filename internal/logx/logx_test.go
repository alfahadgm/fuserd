package logx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func newLogs(t *testing.T, enabled ...string) (*Logs, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	l, err := New(slog.NewTextHandler(&buf, nil), enabled, []byte("test key"))
	if err != nil {
		t.Fatal(err)
	}
	return l, &buf
}

func TestCategoryFilter(t *testing.T) {
	l, buf := newLogs(t, "auth", " STORE ")
	l.For(Auth).Info("auth line")
	l.For(Store).Info("store line")
	l.For(Poll).Info("poll line")
	l.For(Poll).Error("poll error")
	l.For(Server).Info("server line")
	out := buf.String()
	for _, want := range []string{"auth line", "cat=AUTH", "store line", "poll error", "server line"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "poll line") {
		t.Errorf("disabled category was output:\n%s", out)
	}
	if got := l.Enabled(); got != "SERVER,CONTROL,AUTH,STORE" {
		t.Errorf("Enabled() = %q", got)
	}
}

func TestAllCategories(t *testing.T) {
	for _, enabled := range [][]string{nil, {"*"}} {
		l, buf := newLogs(t, enabled...)
		l.For(Poll).Info("poll line")
		if !strings.Contains(buf.String(), "poll line") {
			t.Errorf("%v: poll line missing", enabled)
		}
	}
}

func TestDisabledCategories(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewFiltered(slog.NewTextHandler(&buf, nil), nil, []string{" poll ", "BI"}, []byte("test key"))
	if err != nil {
		t.Fatal(err)
	}
	l.For(Poll).Info("poll line")
	l.For(BI).Info("bi line")
	l.For(Poll).Error("poll error")
	l.For(Auth).Info("auth line")
	out := buf.String()
	if strings.Contains(out, "poll line") || strings.Contains(out, "bi line") {
		t.Errorf("disabled category was output:\n%s", out)
	}
	if !strings.Contains(out, "poll error") || !strings.Contains(out, "auth line") {
		t.Errorf("errors and other categories must still be output:\n%s", out)
	}
	if strings.Contains(l.Enabled(), "POLL") || !strings.Contains(l.Enabled(), "AUTH") {
		t.Errorf("Enabled() = %q", l.Enabled())
	}
	for _, bad := range [][]string{{"PLOL"}, {"SERVER"}, {"CONTROL"}} {
		if _, err := NewFiltered(slog.NewTextHandler(&bytes.Buffer{}, nil), nil, bad, nil); err == nil {
			t.Errorf("disabled %v accepted", bad)
		}
	}
}

func TestUnknownCategory(t *testing.T) {
	if _, err := New(slog.NewTextHandler(&bytes.Buffer{}, nil), []string{"AUHT"}, nil); err == nil {
		t.Fatal("a misspelt category was accepted")
	}
}

func TestTag(t *testing.T) {
	l, _ := newLogs(t)
	a := l.Tag("76561198000000001")
	if len(a) != 12 || a != l.Tag("76561198000000001") || a == l.Tag("76561198000000002") {
		t.Fatalf("tag %q is not a stable 12-character pseudonym", a)
	}
	other, _ := New(slog.NewTextHandler(&bytes.Buffer{}, nil), nil, []byte("other key"))
	if other.Tag("76561198000000001") == a {
		t.Fatal("tag does not depend on the key")
	}
	if l.Tag("") != "" {
		t.Fatal("empty value should stay empty")
	}
}

func TestTagJSON(t *testing.T) {
	l, _ := newLogs(t)
	body := `{"platform":"stm","platform_user_id":"76561198000000001","Token":"abc123","user_id":5,
		"hype_profile":{"xp":100},"friends_list":["111","222"],"page":1}`
	out := l.TagJSON([]byte(body))
	for _, secret := range []string{"76561198000000001", "abc123", `"111"`, `"222"`, `"user_id":5`} {
		if strings.Contains(out, secret) {
			t.Errorf("%s survived: %s", secret, out)
		}
	}
	for _, want := range []string{`"platform":"stm"`, `"xp":100`, `"page":1`, l.Tag("76561198000000001"),
		`"user_id":"` + l.TagID(5) + `"`, l.Tag("111")} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %s: %s", want, out)
		}
	}
	if got := l.TagJSON([]byte("ticket=76561198000000001")); strings.Contains(got, "7656") {
		t.Errorf("non-JSON body logged verbatim: %s", got)
	}
}

func TestTagJSONMatchmakerData(t *testing.T) {
	l, _ := newLogs(t)
	md := `{"teams":[{"name":"djs","players":[{"playerId":"4242","attributes":{"hype_user_id":{"valueAttribute":"4242"},"venue_select":{"valueAttribute":"VenueA"}}}]}]}`
	body, _ := json.Marshal(map[string]any{"game_session_id": "gsess-1", "ip_address": "203.0.113.9",
		"matchmaker_data": md, "matched_player_sessions": []map[string]string{{"player_id": "4242"}}})
	out := l.TagJSON(body)
	for _, secret := range []string{"4242", "203.0.113.9"} {
		if strings.Contains(out, secret) {
			t.Errorf("%s survived: %s", secret, out)
		}
	}
	if !strings.Contains(out, "VenueA") || !strings.Contains(out, "gsess-1") {
		t.Errorf("non-identifying values lost: %s", out)
	}
}
