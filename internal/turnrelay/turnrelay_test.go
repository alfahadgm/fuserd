package turnrelay

import (
	"bytes"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v4"
)

func start(t *testing.T, maxPerUser, bytesPerSecond int, log *slog.Logger) *Relay {
	t.Helper()
	r, err := Listen(Options{Bind: "127.0.0.1", RelayIP: net.IPv4(127, 0, 0, 1), RelayBind: "127.0.0.1",
		MinPort: 41000, MaxPort: 41999, Secret: "test secret", MaxPerUser: maxPerUser, BytesPerSecond: bytesPerSecond, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// allocate makes a pion TURN client on a fresh socket and allocates a relay with the given credentials.
func allocate(t *testing.T, r *Relay, username, password string) (net.PacketConn, error) {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	quiet := logging.NewDefaultLoggerFactory()
	quiet.DefaultLogLevel = logging.LogLevelDisabled
	c, err := turn.NewClient(&turn.ClientConfig{TURNServerAddr: r.Addr().String(), STUNServerAddr: r.Addr().String(),
		Username: username, Password: password, Realm: r.Realm(), Conn: conn, LoggerFactory: quiet,
		RTO: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Close()
		conn.Close()
	})
	return c.Allocate()
}

func TestRelayRoundTrip(t *testing.T) {
	var logBuf bytes.Buffer
	r := start(t, 2, 0, slog.New(slog.NewTextHandler(&logBuf, nil)))
	user, pass, expires := r.Credentials(42, time.Hour)
	if !strings.HasSuffix(user, ":42") || time.Until(expires) < 59*time.Minute {
		t.Fatalf("credentials %q %v", user, expires)
	}
	relay, err := allocate(t, r, user, pass)
	if err != nil {
		t.Fatal(err)
	}
	ra := relay.LocalAddr().(*net.UDPAddr)
	if !ra.IP.Equal(net.IPv4(127, 0, 0, 1)) || ra.Port < 41000 || ra.Port > 41999 {
		t.Fatalf("relayed address %v", ra)
	}
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	// Client -> peer through the relay (the client creates the permission on its first send).
	if _, err := relay.WriteTo([]byte("hello peer"), peer.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1500)
	n, from, err := peer.ReadFromUDP(buf)
	if err != nil || string(buf[:n]) != "hello peer" || from.Port != ra.Port {
		t.Fatalf("peer got %q from %v: %v", buf[:n], from, err)
	}
	// Peer -> client: the peer answers the relayed address.
	if _, err := peer.WriteToUDP([]byte("hello client"), from); err != nil {
		t.Fatal(err)
	}
	relay.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, from2, err := relay.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "hello client" || from2.(*net.UDPAddr).Port != peer.LocalAddr().(*net.UDPAddr).Port {
		t.Fatalf("client got %q from %v: %v", buf[:n], from2, err)
	}
	if r.Allocations() != 1 {
		t.Fatalf("allocations %d", r.Allocations())
	}
	out := logBuf.String()
	for _, want := range []string{"relay allocated", "player=42", "relay permission"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestRelayRefusesBadCredentials(t *testing.T) {
	r := start(t, 2, 0, nil)
	user, _, _ := r.Credentials(7, time.Hour)
	if _, err := allocate(t, r, user, "wrong password"); err == nil {
		t.Fatal("allocated with a wrong password")
	}
	expired := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10) + ":7"
	if _, err := allocate(t, r, expired, Password("test secret", expired)); err == nil {
		t.Fatal("allocated with expired credentials")
	}
	if _, err := allocate(t, r, "7", Password("test secret", "7")); err == nil {
		t.Fatal("allocated with a username that has no expiry")
	}
}

func TestRelayQuota(t *testing.T) {
	r := start(t, 1, 0, nil)
	user, pass, _ := r.Credentials(9, time.Hour)
	if _, err := allocate(t, r, user, pass); err != nil {
		t.Fatal(err)
	}
	if _, err := allocate(t, r, user, pass); err == nil {
		t.Fatal("a second allocation went over the per-player quota")
	}
	other, otherPass, _ := r.Credentials(10, time.Hour)
	if _, err := allocate(t, r, other, otherPass); err != nil {
		t.Fatalf("another player was refused: %v", err)
	}
}

func TestBucket(t *testing.T) {
	b := newBucket(1000)
	if !b.take(600) || !b.take(400) || b.take(1) {
		t.Fatal("burst of one second not enforced")
	}
	time.Sleep(120 * time.Millisecond)
	if !b.take(100) {
		t.Fatal("bucket did not refill")
	}
}

func TestListenRequiresSettings(t *testing.T) {
	if _, err := Listen(Options{Port: 0}); err == nil {
		t.Fatal("started without a secret, relay IP or port range")
	}
}
