package api

import (
	"net/http"
	"strings"
	"testing"

	"fuseronline/internal/config"
)

const songPath = "/Game/DLC/Songs/JoWo_SeinThem_ShpQn/Meta_JoWo_SeinThem_ShpQn.Meta_JoWo_SeinThem_ShpQn"

type songViewJSON struct {
	TransferID string `json:"transfer_id"`
	Song       string `json:"song"`
	State      string `json:"state"`
	Role       string `json:"role"`
	Requester  int64  `json:"requester"`
	Owner      int64  `json:"owner"`
	Attempt    int    `json:"attempt"`
	PairID     uint32 `json:"pair_id"`
	Key        string `json:"key"`
	File       *struct {
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	} `json:"file"`
	Peer *struct {
		Candidates []struct {
			Port int `json:"port"`
		} `json:"candidates"`
	} `json:"peer"`
	Relay any `json:"relay"`
}

type songAnswer struct {
	Transfer  songViewJSON   `json:"transfer"`
	Transfers []songViewJSON `json:"transfers"`
	PollMs    int            `json:"poll_ms"`
	Sync      *struct {
		Round   int  `json:"round"`
		Active  bool `json:"active"`
		Done    int  `json:"done_round"`
		Players int  `json:"players"`
		Failed  int  `json:"failed"`
	} `json:"sync"`
}

// B lacks a song A and C hold: A declines, C offers, B accepts, the transfer finishes - over HTTP.
func TestSongTransferAPI(t *testing.T) {
	srv := newTestServerWith(t, func(c *config.Config) {
		c.P2P.Enabled = true
		c.Multiplayer.LoopbackHostAddress = "198.51.100.7"
	})
	aAuth, a := login(t, srv, "epc", "account-a")
	bAuth, b := login(t, srv, "epc", "account-b")
	cAuth, c := login(t, srv, "epc", "account-c")
	dAuth, _ := login(t, srv, "epc", "account-d")
	sid := hostPublic(t, srv, aAuth, a.UserID)
	for _, auth := range []string{bAuth, cAuth} {
		if status, env := call(t, srv, "POST", "multiplayer/player_sessions/", auth, map[string]string{"game_session_id": sid}); status != http.StatusOK {
			t.Fatalf("join: %d %+v", status, env.Error)
		}
	}
	post := func(path, auth string, body map[string]any) (int, songAnswer, string) {
		t.Helper()
		status, env := call(t, srv, "POST", path, auth, with(body, "game_session_id", sid))
		var ans songAnswer
		if env.Error != nil {
			return status, ans, env.Error.Message
		}
		decodeResults(t, env, &ans)
		return status, ans, ""
	}
	must := func(path, auth string, body map[string]any) songAnswer {
		t.Helper()
		status, ans, msg := post(path, auth, body)
		if status != http.StatusOK {
			t.Fatalf("%s: %d %s", path, status, msg)
		}
		return ans
	}
	bad := func(path, auth string, body map[string]any, why string) {
		t.Helper()
		if status, _, msg := post(path, auth, body); status != http.StatusBadRequest {
			t.Fatalf("%s (%s): %d %s", path, why, status, msg)
		}
	}

	bad("p2p/song/request/", dAuth, map[string]any{"song_path": songPath, "owners": []int64{int64(a.UserID)}}, "not a member")
	bad("p2p/song/request/", bAuth, map[string]any{"song_path": "/Game/Shell/Thing.Thing", "owners": []int64{int64(a.UserID)}}, "not a song path")
	bad("p2p/song/request/", bAuth, map[string]any{"song_path": songPath, "owners": []int64{int64(b.UserID), 424242}}, "no owner in the session")

	// A polls and sends, C doesn't poll yet: A is asked first.
	must("p2p/song/poll/", aAuth, map[string]any{"sending": true})
	req := must("p2p/song/request/", bAuth, map[string]any{"song_path": songPath, "owners": []int64{int64(c.UserID), int64(a.UserID)}}).Transfer
	if req.Song != "JoWo_SeinThem_ShpQn" || req.State != "asked" || req.Owner != int64(a.UserID) || req.Role != "requester" || req.Key != "" || req.Relay != nil {
		t.Fatalf("request: %+v", req)
	}
	pa := must("p2p/song/poll/", aAuth, map[string]any{"sending": true})
	if len(pa.Transfers) != 1 || pa.Transfers[0].Role != "owner" || pa.PollMs != songPollMs {
		t.Fatalf("A's poll: %+v", pa)
	}
	if pc := must("p2p/song/poll/", cAuth, map[string]any{"sending": true}); len(pc.Transfers) != 0 {
		t.Fatalf("C isn't asked yet: %+v", pc)
	}
	must("p2p/song/decline/", aAuth, map[string]any{"transfer_id": req.TransferID, "reason": "not_found"})

	// C offers; B sees the file and C's candidates, then registers its own; C sees B's.
	pc := must("p2p/song/poll/", cAuth, map[string]any{"sending": true})
	if len(pc.Transfers) != 1 || pc.Transfers[0].Owner != int64(c.UserID) || pc.Transfers[0].Attempt != 2 {
		t.Fatalf("C's poll: %+v", pc)
	}
	offer := map[string]any{"transfer_id": req.TransferID, "size": 7404350, "sha256": strings.Repeat("ab", 32), "sig_size": 980,
		"sig_sha256": strings.Repeat("cd", 32)}
	bad("p2p/song/offer/", aAuth, with(with(offer, "candidates", candidates(40000)["candidates"]), "mapping", "endpoint-independent"), "A isn't the owner now")
	bad("p2p/song/offer/", cAuth, offer, "no candidates")
	of := must("p2p/song/offer/", cAuth, with(with(offer, "candidates", candidates(40000)["candidates"]), "mapping", "endpoint-independent")).Transfer
	if of.State != "offered" || of.PairID == 0 || len(of.Key) != 64 || of.Peer != nil {
		t.Fatalf("offer (C has no peer yet): %+v", of)
	}
	pb := must("p2p/song/poll/", bAuth, map[string]any{"sending": false})
	if len(pb.Transfers) != 1 || pb.Transfers[0].File == nil || pb.Transfers[0].File.Size != 7404350 || pb.Transfers[0].Key != of.Key ||
		pb.Transfers[0].Peer == nil || pb.Transfers[0].Peer.Candidates[0].Port != 40000 {
		t.Fatalf("B's poll after the offer: %+v", pb)
	}
	if pa := must("p2p/song/poll/", aAuth, map[string]any{"sending": true}); len(pa.Transfers) != 0 {
		t.Fatalf("A is no longer a party: %+v", pa)
	}
	must("p2p/song/accept/", bAuth, with(candidates(50000), "transfer_id", req.TransferID))
	pc = must("p2p/song/poll/", cAuth, map[string]any{"sending": true})
	if pc.Transfers[0].State != "connecting" || pc.Transfers[0].Peer == nil || pc.Transfers[0].Peer.Candidates[0].Port != 50000 {
		t.Fatalf("C's poll after the accept: %+v", pc)
	}

	// Reports: C may say the bytes move; only B says done.
	must("p2p/song/report/", cAuth, map[string]any{"transfer_id": req.TransferID, "state": "transferring", "path": "direct"})
	bad("p2p/song/report/", cAuth, map[string]any{"transfer_id": req.TransferID, "state": "done"}, "the owner can't finish it")
	done := must("p2p/song/report/", bAuth, map[string]any{"transfer_id": req.TransferID, "state": "done", "bytes": 7405330, "ms": 6100,
		"path": "direct"}).Transfer
	if done.State != "done" || done.Key != "" || done.File != nil {
		t.Fatalf("done (the pair is no longer shown): %+v", done)
	}

	// The sync round: B fetching opens it; B done (1 song missing) closes it for everyone.
	if s := must("p2p/song/poll/", bAuth, map[string]any{"syncing": true}).Sync; s == nil || !s.Active || s.Round != 1 {
		t.Fatalf("B syncing: %+v", s)
	}
	must("p2p/song/poll/", bAuth, map[string]any{"syncing": false, "sync_failed": 1})
	if s := must("p2p/song/poll/", aAuth, map[string]any{"sending": true}).Sync; s == nil || s.Active || s.Done != 1 || s.Players != 1 || s.Failed != 1 {
		t.Fatalf("A sees the finished round: %+v", s)
	}
}

func TestSongTransferDisabled(t *testing.T) {
	srv := newTestServerWith(t, func(c *config.Config) {
		c.P2P.Enabled = true
		c.P2P.SongTransfers = false
	})
	aAuth, a := login(t, srv, "epc", "account-a")
	sid := hostPublic(t, srv, aAuth, a.UserID)
	if status, _ := call(t, srv, "POST", "p2p/song/poll/", aAuth, map[string]any{"game_session_id": sid}); status != http.StatusNotFound {
		t.Fatalf("song transfers off: %d", status)
	}
	if status, _ := call(t, srv, "POST", "p2p/song/poll/", "", map[string]any{"game_session_id": sid}); status != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", status)
	}
}
