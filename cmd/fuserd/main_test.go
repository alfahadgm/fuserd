package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenLogFileAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fuserd.log")
	for _, msg := range []string{"first", "second"} {
		w, closeLog, err := openLogFile(path)
		if err != nil {
			t.Fatal(err)
		}
		slog.New(slog.NewTextHandler(w, nil)).Info(msg, "cat", "SERVER")
		if err := closeLog(); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, "msg=first") || !strings.Contains(s, "msg=second") {
		t.Fatalf("log file does not hold both runs: %q", s)
	}
	if _, _, err := openLogFile(filepath.Join(t.TempDir(), "missing-dir", "x.log")); err == nil {
		t.Fatal("expected an error for an unopenable path")
	}
}
