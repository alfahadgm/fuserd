package api

import (
	"bytes"
	"context"
	"embed"
	"net/http"
	"strings"

	"fuseronline/internal/logx"
	"fuseronline/internal/mainstage"
)

//go:embed site/venues/*.jpg
var siteVenueImages embed.FS

// Texture letters differ from the game's VenueData names. These mappings come
// from BPFL_Venue and ST_StageNames in the retail game's cooked assets.
func siteVenue(venue, tod string) (name, timeOfDay, image string) {
	venues := map[string][2]string{
		"VenueA": {"Rhythmic Guardian", "C"},
		"VenueB": {"Cherry Dip'd", "A"},
		"VenueC": {"Sub Astral", "F"},
		"VenueD": {"Source Decay", "D"},
		"VenueE": {"Ravaged Reef", "E"},
		"VenueF": {"Starglacier", "B"},
	}
	v, ok := venues[venue]
	if !ok {
		return "Public freestyle", "", ""
	}
	switch tod {
	case "morning":
		timeOfDay = "Morning"
	case "day":
		timeOfDay = "Day"
	case "sunset":
		timeOfDay = "Sunset"
	case "night":
		timeOfDay = "Night"
	default:
		return v[0], "", ""
	}
	return v[0], timeOfDay, "/venues/" + v[1] + "-" + tod + ".jpg"
}

func (s *Server) serveVenueImage(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	data, err := siteVenueImages.ReadFile("site/venues/" + file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, file, siteStarted, bytes.NewReader(data))
}

type siteDiamond struct {
	Active, Unavailable, CountsAvailable bool
	Name, Image                          string
	Viewers, Headliners                  int
}

func (s *Server) frontpageDiamond(ctx context.Context) siteDiamond {
	if !s.cfg.MainStage.Enabled {
		return siteDiamond{}
	}
	sn, err := s.mainStageSnapshot(ctx)
	if err != nil {
		s.logs.For(logx.Web).Error("landing page event failed", "err", err)
		return siteDiamond{Unavailable: true}
	}
	e := sn.sched.Current
	if e == nil || e.Type == mainstage.TypeMaintenance {
		return siteDiamond{}
	}
	d := siteDiamond{Active: true, Name: e.Name, Image: e.ImageURL}
	if e.Type == mainstage.TypeSpecialEvent && e.SpecialName != "" {
		d.Name = e.SpecialName
	}
	key := strings.ToLower(e.UUID)
	if images := sn.images[key]; len(images) > 0 {
		d.Image, _ = images[0]["image_download_url"].(string)
	}
	// A fresh heartbeat must identify this event before its audience is shown.
	if live, ok := s.mainStageLive(); ok && strings.EqualFold(live.Event, e.UUID) {
		d.CountsAvailable = true
		seen := map[int64]bool{}
		for _, m := range live.Members {
			if seen[m.UserID] {
				continue
			}
			seen[m.UserID] = true
			if m.Team == "spectator" {
				d.Viewers++
			}
			if sn.headliner[key][m.UserID] {
				d.Headliners++
			}
		}
	}
	return d
}
