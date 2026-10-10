package api

// auth is how a route treats the Authorization header.
type auth int

const (
	authOptional auth = iota // claims attached when a valid token is present
	authRequired             // 401 without a valid token
	authNone                 // header ignored
)

type route struct {
	path    string // relative to ClientURL, exactly as the client builds it
	auth    auth
	handler handlerFunc // nil = stub: empty results, logged as a stub hit
}

// routes lists every endpoint path literal recovered from the shipping exe, grouped by module, plus our own nop/
// polling target.
func (s *Server) routes() []route {
	return []route{
		{"auth/jwt/", authNone, s.authJWT},
		{"auth/steam/owned_dlc/", authRequired, s.steamOwnedDLC},

		{"bi/events/record_event/", authOptional, s.telemetry},
		{"bi/events/start_session/", authOptional, s.telemetry},

		{"events/claim_rewards/", authOptional, nil},
		{"events/get/", authOptional, nil},
		{"events/get_active_events/", authOptional, nil},
		{"events/get_battles_season_rewards/", authOptional, nil},
		{"events/get_most_recently_completed_evaluation_event/", authOptional, nil},
		{"events/get_past_events/", authOptional, nil},
		{"events/get_unclaimed_rewards/", authOptional, nil},
		{"events/shows/get_leaderboard/", authOptional, nil},
		{"events/shows/submit_score/", authOptional, nil},

		{"feed/mark_notifications_as_read/", authOptional, nil},
		{"feed/pull_activity_feed/", authOptional, nil},
		{"feed/pull_notifications/", authOptional, s.pullNotifications},
		{"feed/pull_user_feed/", authOptional, nil},

		{"followship/follow/", authRequired, s.follow},
		{"followship/followers/", authRequired, s.followList(true)},
		{"followship/following/", authRequired, s.followList(false)},
		{"followship/unfollow/", authRequired, s.unfollow},

		{"main_menu/get_content_flags/", authOptional, s.contentFlags},
		{"main_menu/mark_content_flag_as_read/", authOptional, nil},
		{"main_menu/promotions/get_active/", authOptional, s.activePromotions},

		{"main_stage/purchase_slot/", authRequired, s.msPurchaseSlot},
		{"main_stage/refresh_urls/", authOptional, s.mainStageURLs},
		{"main_stage/servers/", authRequired, s.msServers},

		{"multiplayer/game_sessions/cancel_invite/", authRequired, s.cancelInvite},
		{"multiplayer/game_sessions/get/", authRequired, s.getGameSession},
		{"multiplayer/game_sessions/get_game_session_id/", authOptional, nil},
		{"multiplayer/game_sessions/get_list/", authOptional, s.listGameSessions},
		{"multiplayer/game_sessions/get_platform_shadow_session_id/", authOptional, nil},
		{"multiplayer/game_sessions/invite/", authRequired, s.invite},
		{"multiplayer/game_sessions/regions_http/", authOptional, s.regionsHTTP},
		{"multiplayer/player_sessions/", authRequired, s.createPlayerSession},
		{"multiplayer/tickets/", authRequired, s.createTicket},
		{"multiplayer/tickets/accept_match/", authRequired, s.acceptMatch},
		{"multiplayer/tickets/cancel/", authRequired, s.cancelTicket},
		{"multiplayer/tickets/get/", authRequired, s.getTicket},
		{"multiplayer/vivox/config/", authOptional, s.vivoxConfig},
		{"multiplayer/vivox/gen_token/", authOptional, s.vivoxToken},

		{"players/active_battle_season/", authOptional, s.activeBattleSeason},
		{"players/avatar/", authOptional, nil},
		{"players/battle_stats/", authOptional, nil},
		{"players/block/", authRequired, s.block},
		{"players/blocked_users/", authRequired, s.blockedUsers},
		{"players/credits/", authOptional, nil},
		{"players/crossplay/", authRequired, s.crossplay},
		{"players/featured_mix/", authOptional, nil},
		{"players/followers_without_platform_friends/", authRequired, s.followersWithoutPlatformFriends},
		{"players/get/", authOptional, s.getPlayer},
		{"players/ping/", authOptional, s.ping},
		{"players/platform_blocked_users_and_friends/", authRequired, s.uploadFriends},
		{"players/platform_friends/", authRequired, s.platformFriends},
		{"players/profile_pic/", authOptional, s.profilePicUploadURLs},
		{"players/unblock/", authRequired, s.unblock},
		{"players/xp/", authOptional, s.updateXP},

		{"recordings/add_praise/", authOptional, nil},
		{"recordings/add_snapshot/", authOptional, nil},
		{"recordings/delete/", authOptional, nil},
		{"recordings/evaluations/get/", authOptional, nil},
		{"recordings/evaluations/get_top_recording/", authOptional, nil},
		{"recordings/evaluations/get_winning/", authOptional, nil},
		{"recordings/evaluations/participation_progress/", authOptional, nil},
		{"recordings/evaluations/submit/", authOptional, nil},
		{"recordings/get/", authOptional, nil},
		{"recordings/get_list/", authOptional, nil},
		{"recordings/get_next_mix/", authOptional, nil},
		{"recordings/get_popular/", authOptional, nil},
		{"recordings/get_random/", authOptional, nil},
		{"recordings/get_recommended/", authOptional, nil},
		{"recordings/get_recommended_categories/", authOptional, nil},
		{"recordings/get_recordings_for_category/", authOptional, nil},
		{"recordings/get_similar_mixes/", authOptional, nil},
		{"recordings/increment_view_count/", authOptional, nil},
		{"recordings/new/", authOptional, nil},
		{"recordings/new_remix/", authOptional, nil},
		{"recordings/remove_praise/", authOptional, nil},
		{"recordings/set_active_event_submission/", authOptional, nil},

		{"reporting/recording_report_reasons/", authOptional, nil},
		{"reporting/report_recording/", authOptional, nil},
		{"reporting/report_user/", authOptional, nil},
		{"reporting/user_report_reasons/", authOptional, nil},

		{"search/username/", authRequired, s.searchUsername},

		{"store/elder_credit_store/get_claimed_assets/", authOptional, s.claimedAssets},
		{"store/elder_credit_store/get_list/", authOptional, s.elderCreditStore},
		{"store/elder_credit_store/purchase/", authOptional, s.elderCreditPurchase},
		{"store/get_list/", authOptional, s.listStoreItems},
		{"store/song_manifest/get/", authOptional, s.songManifest},
		{"store/songs/get_song/", authOptional, s.getSong},

		{"video/youtube/store/", authOptional, nil},

		// Not a game endpoint: the target of the main_stage/refresh_urls/ polling URLs.
		{"nop/", authNone, s.nop},
	}
}
