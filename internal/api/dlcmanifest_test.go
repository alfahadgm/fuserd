package api

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestDLCManifestDownload(t *testing.T) {
	dir := t.TempDir()
	pakPath := filepath.Join(dir, "dlcmanifest.pak")
	pak := make([]byte, 1000)
	binary.LittleEndian.PutUint32(pak[len(pak)-204:], pakFooterMagic)
	sig := bytes.Repeat([]byte{0xAB}, 640)
	if err := os.WriteFile(pakPath, pak, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDLCManifest(pakPath); err == nil {
		t.Fatal("loaded without a .sig")
	}
	if err := os.WriteFile(filepath.Join(dir, "dlcmanifest.sig"), sig, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadDLCManifest(pakPath)
	if err != nil {
		t.Fatal(err)
	}
	if m.Size() != 4+len(sig)+len(pak) {
		t.Fatalf("size %d", m.Size())
	}

	srv, s := newTestServerAndAPI(t, nil)
	if resp, _ := getPage(t, srv.URL+"/static/dlcmanifest/pc.bin"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unconfigured: status %d", resp.StatusCode)
	}
	s.SetDLCManifest(m)
	resp, body := getPage(t, srv.URL+"/static/dlcmanifest/pc.bin")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	b := []byte(body)
	if n := binary.LittleEndian.Uint32(b); n != uint32(len(sig)) || !bytes.Equal(b[4:4+n], sig) || !bytes.Equal(b[4+n:], pak) {
		t.Fatal("download is not sig length | sig | pak")
	}
	if resp, _ := getPage(t, srv.URL+"/static/dlcmanifest/ps.bin"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("ps.bin: status %d", resp.StatusCode)
	}

	notPak := filepath.Join(dir, "other.pak")
	if err := os.WriteFile(notPak, make([]byte, 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDLCManifest(notPak); err == nil {
		t.Fatal("accepted a file without the pak footer")
	}
}
