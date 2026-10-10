package api

// The control panel's Status tab (GET /control/api/status): the front page's server status - players online, the
// public FDS lobbies with their venue, state, set length and fill - plus the private lobbies, and who is in each
// lobby (name, user id, performer or viewer, the creator). Members are those of the matchmaking session still seen
// online recently, as on the front page. Lobbies whose server is starting or not reporting are listed too.

import (
	"context"
	"fmt"
	"time"

	"fuseronline/internal/control"
	"fuseronline/internal/publichost"
)

type controlStatusMember struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Role    string `json:"role"` // "performer" or "viewer"
	Creator bool   `json:"creator"`
	Joined  string `json:"joined"` // wall time in the session's timezone
}

type controlStatusLobby struct {
	Title      string                `json:"title"` // "Public lobby 1", "Private lobby 1"
	Private    bool                  `json:"private"`
	Session    string                `json:"session"`
	Slot       string                `json:"slot"`
	Venue      string                `json:"venue"`
	TimeOfDay  string                `json:"time_of_day"`
	Image      string                `json:"image"`
	State      string                `json:"state"`
	StateClass string                `json:"state_class"`
	Rounds     int                   `json:"rounds"`
	Bars       int                   `json:"bars"`
	Capacity   int                   `json:"capacity"`
	Players    int                   `json:"players"`
	Performers int                   `json:"performers"`
	Viewers    int                   `json:"viewers"`
	Created    string                `json:"created"`
	Members    []controlStatusMember `json:"members"`
}

type controlStatus struct {
	Online     int                  `json:"online"`
	Performers int                  `json:"performers"`
	Viewers    int                  `json:"viewers"`
	InPublic   int                  `json:"in_public"`
	InPrivate  int                  `json:"in_private"`
	Elsewhere  int                  `json:"elsewhere"`
	Updated    string               `json:"updated"`
	Lobbies    []controlStatusLobby `json:"lobbies"`
}

func (s *Server) controlStatus(ctx context.Context) (controlStatus, error) {
	loc := control.Location(ctx)
	online, sessions, ended := s.mm.StatusSnapshot()
	s.logEnded(ended)
	out := controlStatus{Online: online, Updated: time.Now().In(loc).Format("15:04:05 MST"), Lobbies: []controlStatusLobby{}}
	var managed map[string]publichost.SiteStatus
	if s.public != nil {
		managed = s.public.SiteSnapshot()
	}
	public, private := map[int64]bool{}, map[int64]bool{}
	nPublic, nPrivate := 0, 0
	for _, session := range sessions {
		lobby := controlStatusLobby{Private: session.Private, Session: session.ID, Slot: session.SlotID, Capacity: session.MaxPlayers,
			Created: session.Created.In(loc).Format("2006-01-02 15:04:05"), Members: []controlStatusMember{}}
		venue, tod := session.Venue, session.TimeOfDay
		if meta, ok := managed[session.SlotID]; ok {
			lobby.State, lobby.StateClass = siteGameState(meta.Stage)
			lobby.Rounds, lobby.Bars = meta.Rounds, meta.Bars
			if meta.Venue != "" {
				venue, tod = meta.Venue, meta.TimeOfDay
			}
		} else {
			lobby.State, lobby.StateClass = "Server not ready", "unknown"
		}
		lobby.Venue, lobby.TimeOfDay, lobby.Image = siteVenue(venue, tod)
		if session.Private {
			nPrivate++
			lobby.Title = fmt.Sprintf("Private lobby %d", nPrivate)
		} else {
			nPublic++
			lobby.Title = fmt.Sprintf("Public lobby %d", nPublic)
		}
		for _, m := range session.Members {
			role := "viewer"
			if m.IsDJ {
				role = "performer"
				lobby.Performers++
			} else {
				lobby.Viewers++
			}
			name := fmt.Sprintf("Player %d", m.ID)
			if p, err := s.store.Player(ctx, m.ID); err == nil && p.HypeUsername != "" {
				name = p.HypeUsername
			}
			lobby.Members = append(lobby.Members, controlStatusMember{ID: m.ID, Name: name, Role: role,
				Creator: m.ID == session.HostID, Joined: m.Joined.In(loc).Format("15:04:05")})
			seen := public
			if session.Private {
				seen = private
			}
			if !public[m.ID] && !private[m.ID] {
				if m.IsDJ {
					out.Performers++
				} else {
					out.Viewers++
				}
			}
			seen[m.ID] = true
		}
		lobby.Players = lobby.Performers + lobby.Viewers
		out.Lobbies = append(out.Lobbies, lobby)
	}
	out.InPublic, out.InPrivate = len(public), len(private)
	inLobby := len(public)
	for id := range private {
		if !public[id] {
			inLobby++
		}
	}
	out.Elsewhere = max(0, out.Online-inLobby)
	return out, nil
}
