package api

// The social endpoints: following, username search, other players' profiles, uploaded platform friends, and
// multiplayer invites (delivered with the notification feed). Together they decide who can find and join whose
// private session: the session browser labels each session with the viewer's relation to its host (see
// sessionSource).

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"fuseronline/internal/logx"
	"fuseronline/internal/matchmaking"
	"fuseronline/internal/platform"
	"fuseronline/internal/store"
)

// Browser entry sources, as the client maps the "source" string to ESessionBrowserEntrySource (FUN_14070f4c0).
const (
	sourcePlatformFriend = "platform_friend"
	sourceFollowship     = "followship"
	sourceInvite         = "invite"
	sourcePublic         = "public"
)

// onlineWindow is how recently a player must have pinged to show as online.
const onlineWindow = 2 * time.Minute

// platformCode is EHypeOnlinePlatform (PC 0, XboxLive 1, PSN 2, Switch 3, Steam 4, Epic 5).
func platformCode(p string) int {
	switch p {
	case platform.Steam:
		return 4
	case platform.Epic:
		return 5
	case platform.Nintendo:
		return 3
	}
	return 0
}

func onlineStatus(p *store.Player) string {
	if time.Since(p.LastSeen) <= onlineWindow {
		return "online"
	}
	return "offline"
}

// displayName is the hype username without its "#NNNN" uniqueness suffix (store.createPlayer): the name players see.
func displayName(hypeUsername string) string {
	if i := strings.LastIndexByte(hypeUsername, '#'); i > 0 && len(hypeUsername)-i == 5 {
		if _, err := strconv.Atoi(hypeUsername[i+1:]); err == nil {
			return hypeUsername[:i]
		}
	}
	return hypeUsername
}

// hypeUser renders an FHypeUser as the viewer sees it. It is a superset of FMiniHypeUser, so it serves both.
// The platform account ID stays empty and the platform display name is replaced by the hype username without its
// "#NNNN" suffix (displayName): the invite screen shows platform friends by their platform name (blank before), the
// game takes every player's in-game name from it (the performer, player cards, the interstitial; see ownProfile),
// and the real platform name identifies the person behind the account. hype_username keeps the suffix: it is the
// unique name username search matches.
func (s *Server) hypeUser(ctx context.Context, viewer int64, p *store.Player) (map[string]any, error) {
	followed, followsYou, isBlocked, blockedYou := false, false, false, false
	if viewer != 0 && viewer != p.ID {
		var err error
		if followed, followsYou, err = s.store.Relation(ctx, viewer, p.ID); err != nil {
			return nil, err
		}
		if isBlocked, blockedYou, err = s.store.BlockRelation(ctx, viewer, p.ID); err != nil {
			return nil, err
		}
	}
	pic, err := s.profilePicMeta(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	u := map[string]any{
		"user_id":           p.ID,
		"hype_username":     p.HypeUsername,
		"is_followed":       followed,
		"follows_you":       followsYou,
		"is_blocked":        isBlocked,  // the viewer blocked this player (blocks.go)
		"blocked_you":       blockedYou, // this player blocked the viewer
		"platform_profile":  platformProfile(p),
		"crossplay_enabled": p.CrossplayEnabled,
		"is_banned":         false,
		"online_status":     onlineStatus(p),
	}
	if pic != nil {
		u["profile_pic"] = pic // profilepic.go; none: the default image
	}
	return u, nil
}

// platformProfile is FPlatformProfile as hypeUser describes it: no platform account ID, the display name instead
// of the platform name.
func platformProfile(p *store.Player) map[string]any {
	return map[string]any{
		"user_id":            strconv.FormatInt(p.ID, 10),
		"platform_enum_code": platformCode(p.Platform),
		"platform_user_id":   "",
		"platform_username":  displayName(p.HypeUsername),
	}
}

func (s *Server) hypeUsers(ctx context.Context, viewer int64, list []*store.Player) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		u, err := s.hypeUser(ctx, viewer, p)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

// pageRequest is PaginatedListRequest / FollowshipRequest / UsernameSearchRequest. Pages are 1-based.
type pageRequest struct {
	UserID   int64  `json:"user_id"`
	Page     int    `json:"page"`
	PageSize int    `json:"pageSize"`
	Search   string `json:"search"`
}

func (r pageRequest) window() (page, size, offset int) {
	page, size = r.Page, r.PageSize
	if page < 1 {
		page = 1
	}
	if size <= 0 || size > 100 {
		size = 50
	}
	return page, size, (page - 1) * size
}

func pageInfo(page, size, total int) (current, pages int) {
	pages = (total + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	return page, pages
}

// ---- following -------------------------------------------------------------------------------------------------

func (s *Server) follow(q *request) (any, error) {
	var req pageRequest
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	if err := s.store.Follow(q.Context(), q.playerID(), req.UserID); err != nil {
		return nil, err
	}
	s.logs.For(logx.Followship).Info("follow", "player", s.logs.TagID(q.playerID()), "target", s.logs.TagID(req.UserID))
	return map[string]string{"status": "ok"}, nil
}

func (s *Server) unfollow(q *request) (any, error) {
	var req pageRequest
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	if err := s.store.Unfollow(q.Context(), q.playerID(), req.UserID); err != nil {
		return nil, err
	}
	s.logs.For(logx.Followship).Info("unfollow", "player", s.logs.TagID(q.playerID()), "target", s.logs.TagID(req.UserID))
	return map[string]string{"status": "ok"}, nil
}

// followList serves followship/following/ and followers/. The request is PaginatedListRequest (own list) or
// FollowshipRequest (another player's, user_id). The response struct is not named in the job code, so the
// reply carries both candidate shapes: FFollowshipResponse (count, mini_users) and the paged list fields with
// "followers" (FHypeFollowingUserListResponse).
func (s *Server) followList(followers bool) handlerFunc {
	return func(q *request) (any, error) {
		var req pageRequest
		if err := q.decode(&req); err != nil {
			return nil, err
		}
		who := req.UserID
		if who == 0 {
			who = q.playerID()
		}
		page, size, offset := req.window()
		list, total, err := s.store.Following(q.Context(), who, offset, size)
		if followers {
			list, total, err = s.store.Followers(q.Context(), who, offset, size)
		}
		if err != nil {
			return nil, err
		}
		users, err := s.hypeUsers(q.Context(), q.playerID(), list)
		if err != nil {
			return nil, err
		}
		current, pages := pageInfo(page, size, total)
		return map[string]any{"count": total, "mini_users": users, "followers": users, "current_page": current,
			"num_pages": pages, "total_entries": total}, nil
	}
}

// followersWithoutPlatformFriends is FHypeFollowingUserListResponse: my followers who are not platform friends.
func (s *Server) followersWithoutPlatformFriends(q *request) (any, error) {
	var req pageRequest
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	page, size, offset := req.window()
	list, _, err := s.store.Followers(q.Context(), q.playerID(), 0, 1000)
	if err != nil {
		return nil, err
	}
	var kept []*store.Player
	for _, p := range list {
		friend, err := s.store.ArePlatformFriends(q.Context(), q.playerID(), p.ID)
		if err != nil {
			return nil, err
		}
		if !friend {
			kept = append(kept, p)
		}
	}
	total := len(kept)
	kept = kept[min(offset, total):min(offset+size, total)]
	users, err := s.hypeUsers(q.Context(), q.playerID(), kept)
	if err != nil {
		return nil, err
	}
	current, pages := pageInfo(page, size, total)
	return map[string]any{"followers": users, "current_page": current, "num_pages": pages, "total_entries": total}, nil
}

// searchUsername is UsernameSearchRequest to UsernameSearchResponse.
func (s *Server) searchUsername(q *request) (any, error) {
	var req pageRequest
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	page, size, offset := req.window()
	list, total := []*store.Player{}, 0
	if req.Search != "" {
		var err error
		if list, total, err = s.store.SearchPlayers(q.Context(), req.Search, q.playerID(), offset, size); err != nil {
			return nil, err
		}
	}
	users, err := s.hypeUsers(q.Context(), q.playerID(), list)
	if err != nil {
		return nil, err
	}
	current, pages := pageInfo(page, size, total)
	return map[string]any{"search_results": users, "current_page": current, "num_pages": pages,
		"total_entries": total}, nil
}

// getPlayer is players/get/ (HypeUserRequest): another player's profile, or the player's own (ownProfile).
func (s *Server) getPlayer(q *request) (any, error) {
	var req pageRequest
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	if req.UserID == 0 {
		return nil, nil
	}
	if req.UserID == q.playerID() {
		return s.ownProfile(q)
	}
	p, err := s.store.Player(q.Context(), req.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errBadRequest("unknown user")
	}
	if err != nil {
		return nil, err
	}
	return s.hypeUser(q.Context(), q.playerID(), p)
}

// ownStatsWait is how long ownProfile waits for the login burst's players/xp/ (sent just before players/get/, but
// answered concurrently) to have stored the player's stats.
var ownStatsWait = 2 * time.Second

// ownProfile is players/get/ for the player's own id, which the client asks once after login. Its answer is where
// the client gets its own name: the profile callback (FUN_140745570) copies platform_profile.platform_username into
// the local profile, and each player state takes _localUserName from there (FUN_14069eb40). Without a name a player
// is never promoted into the game state's _hypePlayerStates on its own machine: no performer, no player card, no
// names on the interstitial, no song HUD.
// The same callback also writes hype_profile.num_discs, matchmaking_rating, total_battle_wins and
// total_battle_losses into the client's local profile data, and elder_credits.amount into its balance, so these
// come from what the client last reported (store.PlayerStats). With the Diamond store on, the balance is the store's
// fixed one, as in players/xp/. Nothing else the client keeps (xp, credits, streaks, tier, rank) is read back.
func (s *Server) ownProfile(q *request) (any, error) {
	id := q.playerID()
	p, err := s.store.Player(q.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errBadRequest("unknown user")
	}
	if err != nil {
		return nil, err
	}
	stats, err := s.store.PlayerStats(q.Context(), id)
	for deadline := time.Now().Add(ownStatsWait); errors.Is(err, store.ErrNotFound) && time.Now().Before(deadline); {
		select {
		case <-q.Context().Done():
			return nil, q.Context().Err()
		case <-time.After(100 * time.Millisecond):
		}
		stats, err = s.store.PlayerStats(q.Context(), id)
	}
	stored := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	u, err := s.hypeUser(q.Context(), id, p)
	if err != nil {
		return nil, err
	}
	u["hype_profile"] = map[string]any{
		"num_discs":            stats.NumDiscs,
		"matchmaking_rating":   stats.MatchmakingRating,
		"total_battle_wins":    stats.TotalBattleWins,
		"total_battle_losses":  stats.TotalBattleLosses,
		"cached_elder_credits": stats.CachedElderCredits,
	}
	balance := stats.CachedElderCredits
	if s.cfg.Store.ElderCreditBalance > 0 {
		balance = int32(s.cfg.Store.ElderCreditBalance)
	}
	u["elder_credits"] = map[string]any{"amount": balance, "progress_to_next_credit": 0}
	lg := s.logs.For(logx.Players)
	if stored {
		lg.Info("own profile", "player", s.logs.TagID(id))
	} else {
		lg.Warn("own profile without stored stats: no players/xp/ yet; num_discs, matchmaking_rating, battle wins/losses and the balance are 0",
			"player", s.logs.TagID(id))
	}
	return u, nil
}

// storeOwnStats keeps the players/xp/ fields that ownProfile gives back. A missing or negative value (the login
// burst's second players/xp/ sends -1 for most stats) keeps the stored one. A hype_profile that doesn't parse is
// logged and skipped: the XP update itself goes on.
func (s *Server) storeOwnStats(q *request, id int64, raw json.RawMessage) error {
	var hp struct {
		NumDiscs           *int32 `json:"num_discs"`
		MatchmakingRating  *int32 `json:"matchmaking_rating"`
		TotalBattleWins    *int32 `json:"total_battle_wins"`
		TotalBattleLosses  *int32 `json:"total_battle_losses"`
		CachedElderCredits *int32 `json:"cached_elder_credits"`
	}
	if err := json.Unmarshal(raw, &hp); err != nil {
		s.logs.For(logx.Players).Warn("players/xp/ hype_profile not stored", "player", s.logs.TagID(id), "err", err)
		return nil
	}
	known := func(v *int32) *int32 {
		if v == nil || *v < 0 {
			return nil
		}
		return v
	}
	return s.store.UpdatePlayerStats(q.Context(), id, store.PlayerStatsUpdate{
		NumDiscs: known(hp.NumDiscs), MatchmakingRating: known(hp.MatchmakingRating), TotalBattleWins: known(hp.TotalBattleWins),
		TotalBattleLosses: known(hp.TotalBattleLosses), CachedElderCredits: known(hp.CachedElderCredits)})
}

// ---- platform friends --------------------------------------------------------------------------------------------

// uploadFriends is UploadFriendsRequest { friends_list, blocked_list }: the platform user IDs of the player's
// platform friends. Only the friends list is kept.
func (s *Server) uploadFriends(q *request) (any, error) {
	var req struct {
		FriendsList []string `json:"friends_list"`
		BlockedList []string `json:"blocked_list"`
	}
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	if err := s.store.SetPlatformFriends(q.Context(), q.playerID(), req.FriendsList); err != nil {
		return nil, err
	}
	s.logs.For(logx.Players).Info("platform friends uploaded", "player", s.logs.TagID(q.playerID()),
		"friends", len(req.FriendsList))
	return map[string]string{"status": "ok"}, nil
}

// platformFriends is FHypeFriendsUserListResponse: the uploaded platform friends who play on this server.
func (s *Server) platformFriends(q *request) (any, error) {
	var req pageRequest
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	list, err := s.store.PlatformFriends(q.Context(), q.playerID())
	if err != nil {
		return nil, err
	}
	page, size, offset := req.window()
	total := len(list)
	list = list[min(offset, total):min(offset+size, total)]
	users, err := s.hypeUsers(q.Context(), q.playerID(), list)
	if err != nil {
		return nil, err
	}
	current, pages := pageInfo(page, size, total)
	return map[string]any{"platform_friends": users, "current_page": current, "num_pages": pages,
		"total_entries": total}, nil
}

// ---- invites -------------------------------------------------------------------------------------------------

// invite is FMultiplayerInviteRequest { game_session_id, target_user_id }, sent by the invite screen for each
// selected player.
func (s *Server) invite(q *request) (any, error) {
	var req struct {
		GameSessionID string `json:"game_session_id"`
		TargetUserID  int64  `json:"target_user_id"`
	}
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	if _, err := s.store.Player(q.Context(), req.TargetUserID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errBadRequest("unknown user")
		}
		return nil, err
	}
	// The invitee blocked the inviter: the invite is not recorded, and the inviter gets the usual answer.
	if blocked, err := s.store.HasBlocked(q.Context(), req.TargetUserID, q.playerID()); err != nil {
		return nil, err
	} else if blocked {
		s.mmLog().Info("invite dropped: the target blocked the inviter", "player", s.logs.TagID(q.playerID()),
			"target", s.logs.TagID(req.TargetUserID), "session", s.logs.Tag(req.GameSessionID))
		return map[string]string{"game_session_id": req.GameSessionID}, nil
	}
	if err := s.mm.Invite(req.GameSessionID, q.playerID(), req.TargetUserID); err != nil {
		s.mmLog().Info("invite refused", "player", s.logs.TagID(q.playerID()), "target", s.logs.TagID(req.TargetUserID),
			"session", s.logs.Tag(req.GameSessionID), "reason", err)
		return nil, errBadRequest(err.Error())
	}
	s.mmLog().Info("invite sent", "player", s.logs.TagID(q.playerID()), "target", s.logs.TagID(req.TargetUserID),
		"session", s.logs.Tag(req.GameSessionID))
	return map[string]string{"game_session_id": req.GameSessionID}, nil
}

func (s *Server) cancelInvite(q *request) (any, error) {
	var req struct {
		GameSessionID string `json:"game_session_id"`
		TargetUserID  int64  `json:"target_user_id"`
	}
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	s.mm.CancelInvite(req.GameSessionID, req.TargetUserID)
	s.mmLog().Info("invite cancelled", "player", s.logs.TagID(q.playerID()), "target", s.logs.TagID(req.TargetUserID),
		"session", s.logs.Tag(req.GameSessionID))
	return map[string]string{"game_session_id": req.GameSessionID}, nil
}

// pullNotifications is FNotificationFeedResponse. There are no feed notifications yet; invites[] holds the open
// multiplayer invites (FMultiplayerInviteResponse), which the feed lists with a Join action.
func (s *Server) pullNotifications(q *request) (any, error) {
	invites := []map[string]any{}
	if id := q.playerID(); id != 0 {
		all, fresh := s.mm.Invites(id)
		// Invites from players this one blocked are never handed out (blocks.go; blocking also withdraws them, and
		// invite() doesn't record new ones: this covers an invite that raced a block).
		blocked := map[int64]bool{}
		for _, inv := range all {
			if _, seen := blocked[inv.InviterID]; !seen {
				b, err := s.store.HasBlocked(q.Context(), id, inv.InviterID)
				if err != nil {
					return nil, err
				}
				blocked[inv.InviterID] = b
			}
		}
		for _, inv := range all {
			if blocked[inv.InviterID] {
				continue
			}
			inviter, err := s.store.Player(q.Context(), inv.InviterID)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			profile, err := s.hypeUser(q.Context(), id, inviter)
			if err != nil {
				return nil, err
			}
			kind, err := s.relation(q.Context(), id, inv.InviterID)
			if err != nil {
				return nil, err
			}
			if kind == "" {
				kind = sourceInvite
			}
			invites = append(invites, map[string]any{
				"profile":         profile,
				"inviter_type":    kind,
				"timestamp":       inv.Created.Unix(),
				"game_session_id": inv.SessionID,
			})
		}
		for _, inv := range fresh {
			if blocked[inv.InviterID] {
				continue
			}
			s.mmLog().Info("invite delivered", "player", s.logs.TagID(id), "from", s.logs.TagID(inv.InviterID),
				"session", s.logs.Tag(inv.SessionID))
		}
	}
	// unread_count drives the feed's unread badge; with no other notifications, it is the number of open invites.
	return map[string]any{"unread_count": len(invites), "notifications": []any{}, "invites": invites,
		"current_page": 1, "num_pages": 1, "total_entries": len(invites)}, nil
}

// relation is the strongest social tie between viewer and other: platform friends, then a follow either way.
// Empty when there is none.
func (s *Server) relation(ctx context.Context, viewer, other int64) (string, error) {
	friends, err := s.store.ArePlatformFriends(ctx, viewer, other)
	if err != nil || friends {
		return sourcePlatformFriend, err
	}
	a, b, err := s.store.Relation(ctx, viewer, other)
	if err != nil {
		return "", err
	}
	if a || b {
		return sourceFollowship, nil
	}
	return "", nil
}

// sessionSource is the browser "source" of a session for the viewer, or "" when the viewer may not see it.
// Public sessions are seen by everyone; private ones by the host, invitees, platform friends and follows (either
// direction), or by everyone with multiplayer.open_private_sessions.
func (s *Server) sessionSource(ctx context.Context, viewer int64, sess matchmaking.Session) (string, error) {
	if viewer != 0 && s.mm.Invited(viewer, sess.ID) {
		return sourceInvite, nil
	}
	if viewer != 0 && viewer != sess.HostID {
		kind, err := s.relation(ctx, viewer, sess.HostID)
		if err != nil || kind != "" {
			return kind, err
		}
	}
	switch {
	case !sess.Private:
		return sourcePublic, nil
	case viewer == sess.HostID, s.cfg.Multiplayer.OpenPrivateSessions:
		return sourceInvite, nil
	}
	return "", nil
}
