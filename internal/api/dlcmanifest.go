package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"fuseronline/internal/logx"
)

// The DLC manifest download. store/song_manifest/get/ hands the client
// pc_asset_download_url; a client with no manifest recorded in PersistentDownloadDir/cache_contents fetches it
// whatever the version (UDLCManager's response handler), retries six times on failure, and then finishes its DLC
// scan without one. The download is "uint32 LE sig length | dlcmanifest.sig | dlcmanifest.pak": the client writes
// the .sig, then mounts the rest as DLC/Manifest/dlcmanifest.pak.

const dlcManifestPath = "/static/dlcmanifest/"

// pakFooterMagic sits 204 bytes from the end of a version 8 pak (UE 4.24).
const pakFooterMagic = 0x5A6F12E1

type dlcManifest struct {
	bin  []byte
	etag string
	mod  time.Time
}

// LoadDLCManifest reads the manifest pak and the .sig beside it and builds the download.
func LoadDLCManifest(pakPath string) (*dlcManifest, error) {
	pak, err := os.ReadFile(pakPath)
	if err != nil {
		return nil, err
	}
	if len(pak) < 204 || binary.LittleEndian.Uint32(pak[len(pak)-204:]) != pakFooterMagic {
		return nil, fmt.Errorf("%s: not a version 8 pak", pakPath)
	}
	sigPath := strings.TrimSuffix(pakPath, ".pak") + ".sig"
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		return nil, err
	}
	if len(sig) == 0 {
		return nil, errors.New(sigPath + ": empty")
	}
	fi, err := os.Stat(pakPath)
	if err != nil {
		return nil, err
	}
	bin := make([]byte, 4, 4+len(sig)+len(pak))
	binary.LittleEndian.PutUint32(bin, uint32(len(sig)))
	bin = append(append(bin, sig...), pak...)
	sum := sha256.Sum256(bin)
	return &dlcManifest{bin: bin, etag: `"` + hex.EncodeToString(sum[:16]) + `"`, mod: fi.ModTime()}, nil
}

// Size is the download's length, as the client records it in cache_contents.
func (m *dlcManifest) Size() int { return len(m.bin) }

// SetDLCManifest makes pc_asset_download_url downloadable.
func (s *Server) SetDLCManifest(m *dlcManifest) { s.dlcManifest = m }

// serveDLCManifest serves /static/dlcmanifest/pc.bin. The other platforms' URLs stay 404: their paks differ.
func (s *Server) serveDLCManifest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		s.logs.For(logx.Store).Info("dlc manifest download", "file", r.PathValue("file"), "status", sw.status,
			"bytes", sw.bytes, "ms", time.Since(start).Milliseconds(), "client", s.logs.Tag(s.remoteHost(r)))
	}()
	m := s.dlcManifest
	if m == nil || r.PathValue("file") != "pc.bin" {
		http.NotFound(sw, r)
		return
	}
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(modPackWriteTimeout))
	h := sw.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("ETag", m.etag)
	http.ServeContent(sw, r, "pc.bin", m.mod, bytes.NewReader(m.bin))
}
