package api

// Block lists. A player blocks another from their profile or a player list (UBlockUserAsyncAction):
// players/block/ and players/unblock/ take FBlockUnblockRequest {user_id} and answer FBlockResponse {blocking} /
// FUnblockResponse {unblocked}; players/blocked_users/ (read at login, and by the blocked-users screen) answers
// FBlockedUsersResponse {current_page, num_pages, total_entries, blocked_mini_users}. The game itself mutes and
// hides blocked players it meets (UPlayerProfileMgr _blockedUserIds, OnBlockedUserAdded) and shows FHypeUser
// is_blocked / blocked_you.
//
// fuserd keeps blocked players apart in multiplayer, in both directions (A blocked B, or B blocked A):
//   - public matchmaking never places them in the same session (FDS slot choice, player-hosted sessions, viewers);
//   - the session browser hides a session holding someone the viewer blocked or was blocked by, and a join to one
//     is refused;
//   - an invite from a player the invitee blocked is not delivered (the inviter isn't told).
// An invite overrides the session checks: a player invited to a session sees it and may join it, whoever is in it.

import (
	"context"
	"errors"

	"fuseronline/internal/logx"
	"fuseronline/internal/store"
)

type blockRequest struct {
	UserID int64 `json:"user_id"`
}

// blockTarget decodes a block / unblock request and checks the target: another existing player.
func (s *Server) blockTarget(q *request) (int64, error) {
	var req blockRequest
	if err := q.decode(&req); err != nil {
		return 0, err
	}
	if req.UserID <= 0 {
		return 0, errBadRequest("user_id required")
	}
	if req.UserID == q.playerID() {
		return 0, errBadRequest("cannot block yourself")
	}
	if _, err := s.store.Player(q.Context(), req.UserID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, errBadRequest("unknown user")
		}
		return 0, err
	}
	return req.UserID, nil
}

// block is players/block/. Blocking someone already blocked succeeds again. Their open invites to the player go.
func (s *Server) block(q *request) (any, error) {
	target, err := s.blockTarget(q)
	if err != nil {
		return nil, err
	}
	added, err := s.store.Block(q.Context(), q.playerID(), target)
	if err != nil {
		return nil, err
	}
	s.mm.DropInvitesFrom(target, q.playerID())
	s.logs.For(logx.Players).Info("blocked", "player", s.logs.TagID(q.playerID()), "target", s.logs.TagID(target), "new", added)
	return map[string]int64{"blocking": target}, nil
}

// unblock is players/unblock/. Unblocking someone not blocked succeeds too.
func (s *Server) unblock(q *request) (any, error) {
	target, err := s.blockTarget(q)
	if err != nil {
		return nil, err
	}
	removed, err := s.store.Unblock(q.Context(), q.playerID(), target)
	if err != nil {
		return nil, err
	}
	s.logs.For(logx.Players).Info("unblocked", "player", s.logs.TagID(q.playerID()), "target", s.logs.TagID(target), "was_blocked", removed)
	return map[string]int64{"unblocked": target}, nil
}

// blockedUsers is players/blocked_users/: one page of the player's block list.
func (s *Server) blockedUsers(q *request) (any, error) {
	var req pageRequest
	if err := q.decode(&req); err != nil {
		return nil, err
	}
	page, size, offset := req.window()
	list, total, err := s.store.BlockedPlayers(q.Context(), q.playerID(), offset, size)
	if err != nil {
		return nil, err
	}
	users, err := s.hypeUsers(q.Context(), q.playerID(), list)
	if err != nil {
		return nil, err
	}
	current, pages := pageInfo(page, size, total)
	return map[string]any{"blocked_mini_users": users, "current_page": current, "num_pages": pages,
		"total_entries": total}, nil
}

// blockedEitherWay is the set of players the player blocked or was blocked by, as a predicate for matchmaking.
// Nil (nobody) for no player.
func (s *Server) blockedEitherWay(ctx context.Context, player int64) (func(int64) bool, error) {
	if player == 0 {
		return nil, nil
	}
	set, err := s.store.BlockedEitherWay(ctx, player)
	if err != nil || len(set) == 0 {
		return nil, err
	}
	return func(id int64) bool { return set[id] }, nil
}

// anyBlocked reports whether avoid is true for one of ids (false for a nil avoid).
func anyBlocked(avoid func(int64) bool, ids []int64) bool {
	if avoid == nil {
		return false
	}
	for _, id := range ids {
		if avoid(id) {
			return true
		}
	}
	return false
}
