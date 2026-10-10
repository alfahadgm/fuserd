package api

import (
	"slices"

	"fuseronline/internal/config"
)

// Handlers for the UOnlineModeFlow stages and the main-menu calls made on the way in.

// ping is BackendPingJob, sent every PingSeconds. It must succeed before the player has authed
// (AttemptToGoOnline precedes CheckAuthentication), so it never requires a token.
func (s *Server) ping(q *request) (any, error) {
	if id := q.playerID(); id != 0 {
		s.mm.Seen(id) // keeps a session this player hosts alive
		if err := s.store.TouchPlayer(q.Context(), id); err != nil {
			return nil, err
		}
	}
	return map[string]string{"status": "ok"}, nil
}

// crossplay stores FUploadCrossplaySettingRequest; the response struct is empty.
func (s *Server) crossplay(q *request) (any, error) {
	var req struct {
		EnableCrossplay bool `json:"enable_crossplay"`
	}
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	return nil, s.store.SetCrossplay(q.Context(), q.playerID(), req.EnableCrossplay)
}

// activeBattleSeason is FGetActiveBattleSeasonResponse. season_name is looked up in the client's
// BattleSeasonDataAssets map, so it must be one of its keys, or "noseason" when battles are off (the client then
// ignores end_date).
func (s *Server) activeBattleSeason(*request) (any, error) {
	if !s.cfg.BattleSeason.Enabled {
		return map[string]string{"season_name": config.NoBattleSeason, "end_date": ""}, nil
	}
	return map[string]string{
		"season_name": s.cfg.BattleSeason.Name,
		"end_date":    s.cfg.BattleSeason.EndDate,
	}, nil
}

// vivoxConfig is FVivoxConfigResponse. There is no Vivox service behind this server; the CheckVivoxConfig stage
// needs an answer, voice chat itself is out of scope.
func (s *Server) vivoxConfig(*request) (any, error) {
	return map[string]string{
		"domain": s.cfg.Vivox.Domain,
		"server": s.cfg.Vivox.Server,
		"issuer": s.cfg.Vivox.Issuer,
	}, nil
}

// vivoxToken is FVivoxGenerateTokenResponse. Minting a real Vivox access token needs the Vivox issuer secret,
// which we do not have, so the token is empty and voice login fails cleanly on the client.
func (s *Server) vivoxToken(*request) (any, error) {
	return map[string]string{"token": ""}, nil
}

// songManifest is FSongManifestInfoResponse: the DLC song shortnames the game treats as valid.
func (s *Server) songManifest(*request) (any, error) {
	m := s.cfg.SongManifest
	songs := append([]string{}, m.Songs...)
	// Catalogue songs join the list only on request (store.add_to_song_manifest): listed, the game's DLC machinery
	// would expect manifest metadata for them that does not exist.
	if c := s.cat(); c != nil && s.cfg.Store.AddToSongManifest {
		for _, it := range c.Items() {
			if !slices.Contains(songs, it.ShortName) {
				songs = append(songs, it.ShortName)
			}
		}
	}
	asset := s.cfg.PublicURL + "/static/dlcmanifest/"
	return map[string]any{
		"release_id":                m.ReleaseID,
		"release_date":              m.ReleaseDate,
		"ps_asset_download_url":     asset + "ps.bin",
		"xbox_asset_download_url":   asset + "xbox.bin",
		"pc_asset_download_url":     asset + "pc.bin",
		"switch_asset_download_url": asset + "switch.bin",
		"version":                   m.Version,
		"songs":                     songs,
	}, nil
}

// telemetry swallows bi/events/*. Nothing is stored.
func (s *Server) telemetry(*request) (any, error) { return nil, nil }

// contentFlags is FNewContentFlags.
func (s *Server) contentFlags(*request) (any, error) {
	return map[string]any{"new_content_flags": []any{}}, nil
}

func (s *Server) nop(*request) (any, error) { return map[string]string{"status": "ok"}, nil }
