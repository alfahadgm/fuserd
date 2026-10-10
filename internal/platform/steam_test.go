package platform

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"testing"
	"time"
)

type ticketSpec struct {
	steamID  uint64
	appID    uint32
	expires  time.Time
	dlc      []uint32
	full     bool // wrap in the GC token + session header of an auth session ticket
	unsigned bool
}

func le32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }

func buildTicket(t *testing.T, key *rsa.PrivateKey, s ticketSpec) []byte {
	t.Helper()
	var own []byte
	own = le32(own, 0) // length, patched below
	own = le32(own, 4) // version
	own = binary.LittleEndian.AppendUint64(own, s.steamID)
	own = le32(own, s.appID)
	own = le32(own, 0x0100007f)
	own = le32(own, 0x0100a8c0)
	own = le32(own, 0)
	own = le32(own, uint32(time.Now().Unix()))
	own = le32(own, uint32(s.expires.Unix()))
	own = binary.LittleEndian.AppendUint16(own, 1)
	own = le32(own, 12345)
	own = binary.LittleEndian.AppendUint16(own, uint16(len(s.dlc)))
	for _, d := range s.dlc {
		own = le32(own, d)
		own = binary.LittleEndian.AppendUint16(own, 1)
		own = le32(own, 999)
	}
	own = binary.LittleEndian.AppendUint16(own, 0) // reserved
	binary.LittleEndian.PutUint32(own, uint32(len(own)))

	if !s.unsigned {
		digest := sha1.Sum(own)
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA1, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		own = append(own, sig...)
	}
	if !s.full {
		return own
	}
	var b []byte
	b = le32(b, 20)
	b = binary.LittleEndian.AppendUint64(b, 0xdeadbeef)
	b = binary.LittleEndian.AppendUint64(b, s.steamID)
	b = le32(b, uint32(time.Now().Unix()))
	b = le32(b, 24)
	b = append(b, make([]byte, 24)...)
	b = le32(b, uint32(len(own)))
	return append(b, own...)
}

func testSteamVerifier(t *testing.T) (*SteamVerifier, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	v, err := NewSteamVerifier(1331440, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
	if err != nil {
		t.Fatal(err)
	}
	return v, key
}

func TestBuiltInSteamKeyParses(t *testing.T) {
	v, err := NewSteamVerifier(1331440, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.key.N.BitLen() != 1024 || v.key.E != 17 {
		t.Fatalf("unexpected Valve key: %d bits, e=%d", v.key.N.BitLen(), v.key.E)
	}
}

func TestSteamVerify(t *testing.T) {
	v, key := testSteamVerifier(t)
	other, _ := rsa.GenerateKey(rand.Reader, 1024)
	const user = "76561198000000001"
	good := ticketSpec{steamID: 76561198000000001, appID: 1331440, expires: time.Now().Add(time.Hour),
		dlc: []uint32{1500001, 1500002}}
	mod := func(f func(*ticketSpec)) ticketSpec { s := good; f(&s); return s }

	cases := []struct {
		name string
		spec ticketSpec
		key  *rsa.PrivateKey
		user string
		ok   bool
	}{
		{"bare ownership ticket", good, key, user, true},
		{"full auth session ticket", mod(func(s *ticketSpec) { s.full = true }), key, user, true},
		{"other app", mod(func(s *ticketSpec) { s.appID = 440 }), key, user, false},
		{"other steamid", good, key, "76561198000000002", false},
		{"expired", mod(func(s *ticketSpec) { s.expires = time.Now().Add(-time.Minute) }), key, user, false},
		{"unsigned", mod(func(s *ticketSpec) { s.unsigned = true }), key, user, false},
		{"signed by someone else", good, other, user, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tk := hex.EncodeToString(buildTicket(t, c.key, c.spec))
			id, err := v.Verify(context.Background(), c.user, tk)
			if c.ok {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				if len(id.OwnedDLC) != 2 || id.OwnedDLC[0] != 1500001 {
					t.Fatalf("DLC = %v", id.OwnedDLC)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestSteamTamperedTicket(t *testing.T) {
	v, key := testSteamVerifier(t)
	raw := buildTicket(t, key, ticketSpec{steamID: 1, appID: 1331440, expires: time.Now().Add(time.Hour),
		dlc: []uint32{7}})
	raw[40] ^= 1 // inside the licence list, covered by the signature
	if _, err := v.Verify(context.Background(), "1", hex.EncodeToString(raw)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tampered ticket accepted: %v", err)
	}
	if _, err := v.Verify(context.Background(), "1", "zz"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-hex accepted: %v", err)
	}
	if _, err := ParseSteamTicket(raw[:30]); err == nil {
		t.Fatal("truncated ticket parsed")
	}
}
