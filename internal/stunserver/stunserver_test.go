package stunserver

import (
	"bytes"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/stun/v3"
)

func start(t *testing.T, rps int, log *slog.Logger) *Server {
	t.Helper()
	s, err := Listen(Options{Bind: "127.0.0.1", Ports: []int{0, 0}, RequestsPerSecond: rps, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func client(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestBindingAnswersBothPorts(t *testing.T) {
	var logBuf bytes.Buffer
	s := start(t, 0, slog.New(slog.NewTextHandler(&logBuf, nil)))
	c := client(t)
	local := c.LocalAddr().(*net.UDPAddr)
	addrs := s.Addrs()
	if len(addrs) != 2 || addrs[0].Port == addrs[1].Port {
		t.Fatalf("addrs %v", addrs)
	}
	var seen []*net.UDPAddr
	for _, a := range addrs {
		got, err := Probe(c, a, 3, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !got.IP.Equal(local.IP) || got.Port != local.Port {
			t.Fatalf("mapped %v, want %v", got, local)
		}
		seen = append(seen, got)
	}
	if m := Classify(local, seen[0], seen[1]); m != MappingNone {
		t.Fatalf("classify: %s", m)
	}
	// A repeat within the minute isn't logged again; each port logs its own line, with the client port in clear.
	if _, err := Probe(c, addrs[0], 3, time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	out := logBuf.String()
	if n := strings.Count(out, "stun binding"); n != 2 {
		t.Fatalf("want one log line per port and client endpoint per minute, got %d:\n%s", n, out)
	}
	for _, a := range addrs {
		if !strings.Contains(out, "port="+strconv.Itoa(a.Port)) {
			t.Errorf("no line for port %d:\n%s", a.Port, out)
		}
	}
	if !strings.Contains(out, "client_port="+strconv.Itoa(local.Port)) {
		t.Errorf("client port not logged:\n%s", out)
	}
}

func TestAnswerAttributes(t *testing.T) {
	s := start(t, 0, nil)
	c := client(t)
	req, _ := stun.Build(stun.TransactionID, stun.BindingRequest, stun.Fingerprint)
	if _, err := c.WriteToUDP(req.Raw, s.Addrs()[0]); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := c.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	resp := &stun.Message{Raw: buf[:n]}
	if err := resp.Decode(); err != nil {
		t.Fatal(err)
	}
	if resp.Type != stun.BindingSuccess || resp.TransactionID != req.TransactionID {
		t.Fatalf("type %v, transaction %x", resp.Type, resp.TransactionID)
	}
	if err := stun.Fingerprint.Check(resp); err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	var soft stun.Software
	if err := soft.GetFrom(resp); err != nil || soft.String() != Software {
		t.Fatalf("software %q %v", soft, err)
	}
	var mapped stun.MappedAddress
	if err := mapped.GetFrom(resp); err != nil || mapped.Port != c.LocalAddr().(*net.UDPAddr).Port {
		t.Fatalf("mapped-address %v %v", mapped, err)
	}
}

func TestIgnoresOtherPackets(t *testing.T) {
	s := start(t, 0, nil)
	c := client(t)
	srv := s.Addrs()[0]
	badFingerprint, _ := stun.Build(stun.TransactionID, stun.BindingRequest, stun.Fingerprint)
	badFingerprint.Raw[len(badFingerprint.Raw)-1] ^= 0xff
	indication, _ := stun.Build(stun.TransactionID, stun.NewType(stun.MethodBinding, stun.ClassIndication))
	for _, p := range [][]byte{[]byte("hello, not stun"), make([]byte, 20), badFingerprint.Raw, indication.Raw} {
		c.WriteToUDP(p, srv)
	}
	c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, _, err := c.ReadFromUDP(make([]byte, 1500)); err == nil {
		t.Fatalf("answered a packet it should ignore (%d bytes)", n)
	}
	c.SetReadDeadline(time.Time{})
	if _, err := Probe(c, srv, 3, time.Second); err != nil {
		t.Fatalf("server stopped answering after junk: %v", err)
	}
}

func TestRateLimit(t *testing.T) {
	s := start(t, 2, nil)
	c := client(t)
	srv := s.Addrs()[0]
	for i := 0; i < 2; i++ {
		if _, err := Probe(c, srv, 1, time.Second); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	if _, err := Probe(c, srv, 1, 200*time.Millisecond); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("third request in a second: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := Probe(c, srv, 1, time.Second); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestProbeTimesOut(t *testing.T) {
	c := client(t)
	dead := client(t) // a bound socket that never answers
	if _, err := Probe(c, dead.LocalAddr().(*net.UDPAddr), 2, 100*time.Millisecond); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("want ErrNoAnswer, got %v", err)
	}
}

func TestClassify(t *testing.T) {
	ep := func(s string) *net.UDPAddr {
		a, _ := net.ResolveUDPAddr("udp", s)
		return a
	}
	local := ep("192.168.1.5:50000")
	for _, tc := range []struct {
		primary, alt *net.UDPAddr
		want         Mapping
	}{
		{ep("203.0.113.10:62000"), ep("203.0.113.10:62000"), MappingIndependent},
		{ep("203.0.113.10:62000"), ep("203.0.113.10:62017"), MappingDependent},
		{ep("203.0.113.10:62000"), ep("203.0.113.11:62000"), MappingDependent},
		{ep("192.168.1.5:50000"), ep("192.168.1.5:50000"), MappingNone},
		{ep("203.0.113.10:62000"), nil, MappingUnknown},
		{ep("192.168.1.5:50000"), nil, MappingNone},
		{nil, nil, MappingUnknown},
	} {
		if got := Classify(local, tc.primary, tc.alt); got != tc.want {
			t.Errorf("Classify(%v, %v) = %s, want %s", tc.primary, tc.alt, got, tc.want)
		}
	}
}

func TestListenErrors(t *testing.T) {
	if _, err := Listen(Options{}); err == nil {
		t.Fatal("listened on no ports")
	}
	s := start(t, 0, nil)
	taken := s.Addrs()[0].Port
	if _, err := Listen(Options{Bind: "127.0.0.1", Ports: []int{0, taken}}); err == nil {
		t.Fatal("bound a port already in use")
	}
}
