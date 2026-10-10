package api

import "testing"

func TestDisplayName(t *testing.T) {
	for in, want := range map[string]string{
		"TesterOne#0042": "TesterOne",
		"DJ#Name#1234":   "DJ#Name",
		"offline":        "offline",
		"Name#12":        "Name#12",   // not a 4-digit suffix
		"Name#12a4":      "Name#12a4", // not digits
		"#1234":          "#1234",     // nothing would be left
	} {
		if got := displayName(in); got != want {
			t.Errorf("displayName(%q) = %q, want %q", in, got, want)
		}
	}
}
