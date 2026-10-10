package platform

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// steamSystemKey is Valve's public key for app ownership ticket signatures (the same key SteamKit and other
// ticket validators embed).
const steamSystemKey = `-----BEGIN PUBLIC KEY-----
MIGdMA0GCSqGSIb3DQEBAQUAA4GLADCBhwKBgQDf7BrWLBBmLBc1OhSwfFkRf53T
2Ct64+AVzRkeRuh7h3SiGEYxqQMUeYKO6UWiSRKpI2hzic9pobFhRr3Bvr/WARvY
gdTckPv+T1JzZsuVcNfFjrocejN1oWI0Rrtgt4Bo+hOneoo3S57G9F1fOpn5nsQ6
6WOiu4gZKODnFMBCiQIBEQ==
-----END PUBLIC KEY-----`

const steamSignatureLen = 128

// SteamTicket is a parsed app ownership ticket.
type SteamTicket struct {
	Version   uint32
	SteamID   uint64
	AppID     uint32
	Flags     uint32
	Issued    time.Time
	Expires   time.Time
	Licenses  []uint32
	DLC       []uint32
	signed    []byte // the ownership ticket bytes the signature covers
	signature []byte
}

// ParseSteamTicket parses either a full auth session ticket (GC token + session header + ownership ticket) or a
// bare ownership ticket. All fields are little-endian.
func ParseSteamTicket(b []byte) (*SteamTicket, error) {
	r := reader{b: b}
	if len(b) >= 4 && binary.LittleEndian.Uint32(b) == 20 {
		r.u32()            // GC token section length (20)
		r.u64()            // GC token
		r.u64()            // SteamID, repeated in the ownership ticket
		r.u32()            // token generated
		if r.u32() != 24 { // session header length
			return nil, errors.New("steam ticket: bad session header")
		}
		r.skip(24)
		sectionLen := int(r.u32()) // ownership section length, with signature
		if sectionLen != len(b)-r.off {
			return nil, errors.New("steam ticket: bad ownership section length")
		}
	}

	start := r.off
	ticketLen := int(r.u32()) // includes this length field
	end := start + ticketLen
	if ticketLen < 4 || (end != len(b) && end+steamSignatureLen != len(b)) {
		return nil, errors.New("steam ticket: bad ownership ticket length")
	}
	t := &SteamTicket{Version: r.u32(), SteamID: r.u64(), AppID: r.u32()}
	r.u32() // external IP
	r.u32() // internal IP
	t.Flags = r.u32()
	t.Issued = time.Unix(int64(r.u32()), 0)
	t.Expires = time.Unix(int64(r.u32()), 0)
	for n := r.u16(); n > 0 && r.err == nil; n-- {
		t.Licenses = append(t.Licenses, r.u32())
	}
	for n := r.u16(); n > 0 && r.err == nil; n-- {
		t.DLC = append(t.DLC, r.u32())
		for l := r.u16(); l > 0 && r.err == nil; l-- {
			r.u32() // the DLC's licences
		}
	}
	r.u16() // reserved
	if r.err != nil || r.off != end {
		return nil, errors.New("steam ticket: truncated ownership ticket")
	}
	t.signed = b[start:end]
	if end+steamSignatureLen == len(b) {
		t.signature = b[end:]
	}
	return t, nil
}

type SteamVerifier struct {
	appID uint32
	key   *rsa.PublicKey
	now   func() time.Time
}

// NewSteamVerifier builds a verifier for appID. keyPEM overrides the built-in Valve key when non-empty.
func NewSteamVerifier(appID uint32, keyPEM string) (*SteamVerifier, error) {
	if keyPEM == "" {
		keyPEM = steamSystemKey
	}
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		return nil, errors.New("steam: public key is not PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("steam: %w", err)
	}
	key, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("steam: public key is not RSA")
	}
	return &SteamVerifier{appID: appID, key: key, now: time.Now}, nil
}

// Verify takes the hex-encoded ticket the client sends as FPlayerProfileAuthRequest.Token.
func (v *SteamVerifier) Verify(_ context.Context, userID, ticket string) (Identity, error) {
	raw, err := hex.DecodeString(ticket)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: ticket is not hex", ErrInvalid)
	}
	t, err := ParseSteamTicket(raw)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if t.signature == nil {
		return Identity{}, fmt.Errorf("%w: ticket is unsigned", ErrInvalid)
	}
	digest := sha1.Sum(t.signed)
	if err := rsa.VerifyPKCS1v15(v.key, crypto.SHA1, digest[:], t.signature); err != nil {
		return Identity{}, fmt.Errorf("%w: bad signature", ErrInvalid)
	}
	if t.AppID != v.appID {
		return Identity{}, fmt.Errorf("%w: ticket is for app %d", ErrInvalid, t.AppID)
	}
	if strconv.FormatUint(t.SteamID, 10) != userID {
		return Identity{}, fmt.Errorf("%w: ticket is for another SteamID", ErrInvalid)
	}
	if !v.now().Before(t.Expires) {
		return Identity{}, fmt.Errorf("%w: ticket expired", ErrInvalid)
	}
	return Identity{Platform: Steam, UserID: userID, OwnedDLC: t.DLC}, nil
}

// reader is a little-endian cursor that latches the first out-of-bounds read.
type reader struct {
	b   []byte
	off int
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil || r.off+n > len(r.b) {
		r.err = errors.New("short read")
		return make([]byte, n)
	}
	p := r.b[r.off : r.off+n]
	r.off += n
	return p
}

func (r *reader) skip(n int)  { r.take(n) }
func (r *reader) u16() uint16 { return binary.LittleEndian.Uint16(r.take(2)) }
func (r *reader) u32() uint32 { return binary.LittleEndian.Uint32(r.take(4)) }
func (r *reader) u64() uint64 { return binary.LittleEndian.Uint64(r.take(8)) }
