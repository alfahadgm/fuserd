package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"fuseronline/internal/config"
	"fuseronline/internal/control"
	"fuseronline/internal/store"
	"io"
	"text/tabwriter"
	"time"
)

const controlUsage = "usage: fuserd control enroll -name NAME [-expires 15m] | list | revoke -id ADMIN_ID | revoke-token -id TOKEN_ID [-config config.json]"

func controlCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(controlUsage)
	}
	action := args[0]
	fs := flag.NewFlagSet("control", flag.ContinueOnError)
	path := fs.String("config", "config.json", "configuration file")
	name := fs.String("name", "", "administrator name")
	ttl := fs.Duration("expires", 15*time.Minute, "enrollment expiry (maximum 24h)")
	id := fs.String("id", "", "administrator id (revoke) or enrollment token id (revoke-token), as list prints them")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if !cfg.Control.Enabled {
		return errors.New("control.enabled must be true")
	}
	if err := control.ValidateOrigin(cfg.Control.Origin); err != nil {
		return err
	}
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	switch action {
	case "enroll":
		token, tokenID, err := st.ControlToken(ctx, *name, *ttl, store.ControlCLI)
		if err != nil {
			return err
		}
		if err := cliAudit(ctx, st, "admins.token_create", tokenID, fmt.Sprintf("for %q, expires in %s", *name, *ttl)); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, token)
		return err
	case "list":
		return controlList(ctx, st, out)
	case "revoke":
		if *id == "" {
			return errors.New("-id required (see fuserd control list)")
		}
		admin, err := st.ControlAdmin(ctx, *id)
		if err != nil {
			return fmt.Errorf("administrator %s not found", *id)
		}
		if err := st.ControlRevoke(ctx, *id); err != nil {
			return err
		}
		return cliAudit(ctx, st, "admins.revoke", *id, admin.Name)
	case "revoke-token":
		if *id == "" {
			return errors.New("-id required (see fuserd control list)")
		}
		if err := st.ControlTokenRevoke(ctx, *id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("enrollment token %s not found; it may have been used or expired", *id)
			}
			return err
		}
		return cliAudit(ctx, st, "admins.token_cancel", *id, "")
	}
	return errors.New("unknown control command; " + controlUsage)
}

// cliAudit records a command-line action in the audit trail the panel shows.
func cliAudit(ctx context.Context, st *store.Store, action, target, detail string) error {
	err := st.ControlAuditAdd(ctx, store.ControlAuditEntry{At: time.Now(), AdminID: store.ControlCLI, AdminName: "Command line",
		Action: action, Target: target, Detail: detail})
	if err != nil {
		return fmt.Errorf("done, but the audit entry was not stored: %w", err)
	}
	return nil
}

func controlList(ctx context.Context, st *store.Store, out io.Writer) error {
	admins, err := st.ControlAdmins(ctx)
	if err != nil {
		return err
	}
	tokens, err := st.ControlTokens(ctx)
	if err != nil {
		return err
	}
	names := map[string]string{store.ControlCLI: "command line"}
	for _, a := range admins {
		names[a.ID] = a.Name
	}
	by := func(id string) string {
		if id == "" {
			return "-"
		}
		if n, ok := names[id]; ok {
			return n
		}
		return "removed administrator"
	}
	when := func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.Local().Format("2006-01-02 15:04 MST")
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ADMINISTRATORS")
	if len(admins) == 0 {
		fmt.Fprintln(tw, "(none)")
	} else {
		fmt.Fprintln(tw, "ID\tNAME\tENROLLED\tENROLLED BY\tLAST SIGN-IN")
		for _, a := range admins {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.Name, when(a.Created), by(a.CreatedBy), when(a.LastLogin))
		}
	}
	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "PENDING ENROLLMENT TOKENS")
	if len(tokens) == 0 {
		fmt.Fprintln(tw, "(none)")
	} else {
		fmt.Fprintln(tw, "ID\tNAME\tCREATED BY\tEXPIRES")
		for _, t := range tokens {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.ID, t.Name, by(t.CreatedBy), when(t.Expires))
		}
	}
	return tw.Flush()
}
