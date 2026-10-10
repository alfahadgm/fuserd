package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"fuseronline/internal/config"
	"fuseronline/internal/store"
)

const profilePicUsage = `usage:
  fuserd profile-pic remove -config config.json -player <player id>

Removes a player's profile picture (moderation): the stored images and the record, so every game shows the default
image again. The player's game uploads a new picture the next time it renders one (an avatar change). The player id
is fuserd's (the user_id in the log's profile lines, or the players table).`

// profilePicCommand is "fuserd profile-pic ...".
func profilePicCommand(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "remove" {
		return errors.New(profilePicUsage)
	}
	fs := flag.NewFlagSet("profile-pic remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "config.json", "path to the configuration file")
	player := fs.Int64("player", 0, "the player's fuserd id")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 || *player <= 0 {
		return errors.New(profilePicUsage)
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
	picID, err := st.RemoveProfilePic(context.Background(), *player)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Fprintf(out, "player %d has no profile picture\n", *player)
		return nil
	}
	if err != nil {
		return err
	}
	// Every file of the player goes, not only the current picture's (a stale upload may have left one).
	dir := filepath.Join(cfg.ProfilePics.Dir, strconv.FormatInt(*player, 10))
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("record removed, but the images in %s were not: %w", dir, err)
	}
	fmt.Fprintf(out, "removed profile picture %d of player %d\n", picID, *player)
	return nil
}
