package p2p

import (
	"errors"
	"testing"
	"time"
)

func ep(port int) Endpoint {
	return Endpoint{Candidates: []Candidate{{TypeHost, "192.168.1.5", port}, {TypeSrflx, "203.0.113.10", port}},
		Mapping: "endpoint-independent"}
}

func TestValidate(t *testing.T) {
	if err := ep(50000).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (Endpoint{Candidates: []Candidate{{TypeRelay, "203.0.113.1", 49160}}}).Validate(); err != nil {
		t.Fatalf("relay candidate refused: %v", err)
	}
	many := Endpoint{}
	for i := 0; i <= MaxCandidates; i++ {
		many.Candidates = append(many.Candidates, Candidate{TypeHost, "192.168.1.5", 1000 + i})
	}
	for name, e := range map[string]Endpoint{
		"none":        {},
		"too many":    many,
		"bad type":    {Candidates: []Candidate{{"prflx", "203.0.113.1", 1}}},
		"loopback":    {Candidates: []Candidate{{TypeHost, "127.0.0.1", 1}}},
		"unspecified": {Candidates: []Candidate{{TypeHost, "0.0.0.0", 1}}},
		"not an ip":   {Candidates: []Candidate{{TypeHost, "example.org", 1}}},
		"port 0":      {Candidates: []Candidate{{TypeHost, "192.168.1.5", 0}}},
		"port high":   {Candidates: []Candidate{{TypeHost, "192.168.1.5", 70000}}},
	} {
		if e.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHostAndJoiners(t *testing.T) {
	r := New(time.Minute)
	const sess, host, b, c = "gsess-1", int64(1), int64(2), int64(3)
	all := func(int64) bool { return true }

	if _, err := r.HostPoll(sess, host, all); !errors.Is(err, ErrNoHost) {
		t.Fatalf("host poll before registering: %v", err)
	}
	if _, _, err := r.JoinerPoll(sess, host, b); !errors.Is(err, ErrNoPeer) {
		t.Fatalf("joiner poll before registering: %v", err)
	}

	// A joiner may register before the host: it waits for the host's endpoint.
	pb, h, err := r.SetJoiner(sess, host, b, ep(50001))
	if err != nil || h != nil || pb.ID == 0 || len(pb.Key) != 64 {
		t.Fatalf("early joiner: %+v %v %v", pb, h, err)
	}
	peers := r.SetHost(sess, host, ep(50000), all)
	if len(peers) != 1 || peers[0].ID != pb.ID || peers[0].Key != pb.Key || peers[0].Candidates[0].Port != 50001 {
		t.Fatalf("host sees %+v", peers)
	}
	pc, h, err := r.SetJoiner(sess, host, c, ep(50002))
	if err != nil || h == nil || h.Candidates[0].Port != 50000 || pc.ID == pb.ID || pc.Key == pb.Key {
		t.Fatalf("second joiner: %+v %v %v", pc, h, err)
	}

	// Re-registering keeps the peer id and key and updates the candidates.
	again, _, _ := r.SetJoiner(sess, host, b, ep(50011))
	if again.ID != pb.ID || again.Key != pb.Key || again.Candidates[0].Port != 50011 {
		t.Fatalf("re-register changed identity: %+v", again)
	}
	if p, h, err := r.JoinerPoll(sess, host, b); err != nil || p.ID != pb.ID || h == nil {
		t.Fatalf("joiner poll: %+v %v %v", p, h, err)
	}

	// The host only sees current members; a joiner who left is forgotten.
	peers, err = r.HostPoll(sess, host, func(id int64) bool { return id != b })
	if err != nil || len(peers) != 1 || peers[0].ID != pc.ID {
		t.Fatalf("after b left: %+v %v", peers, err)
	}
	if _, _, err := r.JoinerPoll(sess, host, b); !errors.Is(err, ErrNoPeer) {
		t.Fatalf("a departed joiner still polls: %v", err)
	}

	r.Drop(sess)
	if _, err := r.HostPoll(sess, host, all); !errors.Is(err, ErrNoHost) {
		t.Fatalf("after drop: %v", err)
	}
}

func TestReports(t *testing.T) {
	r := New(time.Minute)
	const sess, host, b, c = "gsess-1", int64(1), int64(2), int64(3)
	r.SetHost(sess, host, ep(50000), nil)
	pb, _, _ := r.SetJoiner(sess, host, b, ep(50001))
	pc, _, _ := r.SetJoiner(sess, host, c, ep(50002))
	if who, err := r.SetReport(sess, host, host, pb.ID, Report{State: "connected", Path: "lan"}); err != nil || who != b {
		t.Fatalf("host report: %d %v", who, err)
	}
	if who, err := r.SetReport(sess, host, b, pb.ID, Report{State: "connected", Path: "direct"}); err != nil || who != b {
		t.Fatalf("own report: %d %v", who, err)
	}
	if _, err := r.SetReport(sess, host, b, pc.ID, Report{}); !errors.Is(err, ErrBadReport) {
		t.Fatalf("a joiner reported on another joiner's pair: %v", err)
	}
	if _, err := r.SetReport(sess, host, host, 12345, Report{}); !errors.Is(err, ErrBadReport) {
		t.Fatalf("unknown peer accepted: %v", err)
	}
	peers := r.SetHost(sess, host, ep(50000), nil)
	for _, p := range peers {
		if p.ID == pb.ID && (p.Report == nil || p.Report.Path != "direct") {
			t.Fatalf("report not kept: %+v", p)
		}
	}
}

func TestExpiry(t *testing.T) {
	r := New(time.Minute)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	const sess, host, b = "gsess-1", int64(1), int64(2)
	r.SetHost(sess, host, ep(50000), nil)
	r.SetJoiner(sess, host, b, ep(50001))

	now = now.Add(50 * time.Second)
	if _, _, err := r.JoinerPoll(sess, host, b); err != nil { // the joiner keeps itself alive, the host doesn't
		t.Fatal(err)
	}
	now = now.Add(20 * time.Second)
	if _, err := r.HostPoll(sess, host, nil); !errors.Is(err, ErrNoHost) {
		t.Fatalf("host registration outlived its ttl: %v", err)
	}
	if _, h, err := r.JoinerPoll(sess, host, b); err != nil || h != nil {
		t.Fatalf("joiner after the host expired: %v %v", h, err)
	}
	now = now.Add(2 * time.Minute)
	r.mu.Lock()
	r.sweep()
	n := len(r.sessions)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d sessions left after everything expired", n)
	}
}

func TestSessionIDReusedByAnotherHost(t *testing.T) {
	r := New(time.Minute)
	r.SetHost("gsess-1", 1, ep(50000), nil)
	r.SetJoiner("gsess-1", 1, 2, ep(50001))
	peers := r.SetHost("gsess-1", 9, ep(50009), nil)
	if len(peers) != 0 {
		t.Fatalf("a new host inherited joiners: %+v", peers)
	}
	if _, err := r.HostPoll("gsess-1", 1, nil); !errors.Is(err, ErrNoHost) {
		t.Fatalf("the old host still polls: %v", err)
	}
}
