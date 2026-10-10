package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"

	"fuseronline/internal/config"
	"fuseronline/internal/store"
)

const exemptUsage = `usage:
  fuserd exempt -config config.json -username <name> -key <key>

Adds an exempt Epic login: a player whose ticket fails Epic's signature check (an emulator) may still log in when
its display name is exactly <name> and its account ID (the emulator's epicid) is exactly <key>. Running it again
with the same key changes the name. A key that is already verified or blocked is refused. Exempt logins are only
accepted while auth.allow_epic_failed_signatures is true (README, "Authentication").`

var epicIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// exemptCommand is "fuserd exempt ...": provision an allowed epic_auth_policy row.
func exemptCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("exempt", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "config.json", "path to the configuration file")
	username := fs.String("username", "", "the player's display name (the JWT dn claim), matched exactly")
	key := fs.String("key", "", "the player's account ID (the JWT sub claim; the emulator's epicid)")
	if err := fs.Parse(args); err != nil {
		return errors.New(exemptUsage)
	}
	if fs.NArg() > 0 || *username == "" || *key == "" {
		return errors.New(exemptUsage)
	}
	if strings.TrimSpace(*key) != *key || strings.TrimSpace(*username) != *username {
		return errors.New("-username and -key must not start or end with whitespace (they are matched exactly)")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer st.Close()
	updated, err := st.AllowEpicExemption(context.Background(), *key, *username)
	if err != nil {
		return err
	}
	if updated {
		fmt.Fprintf(out, "updated exempt login %q\n", *username)
	} else {
		fmt.Fprintf(out, "added exempt login %q\n", *username)
	}
	if !epicIDPattern.MatchString(*key) {
		fmt.Fprintln(out, "note: the key is not 32 lowercase hex characters, unlike an Epic account ID; the login only works if the emulator sends it exactly")
	}
	if !cfg.Auth.AllowEpicFailedSignatures {
		fmt.Fprintln(out, "note: auth.allow_epic_failed_signatures is false, so exempt logins are refused until it is enabled")
	}
	return nil
}
