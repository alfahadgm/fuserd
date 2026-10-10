// Package community describes the community songs fuserd links to but never hosts: songs listed on
// the FUSER Customs sheet, whose files stay on their creators' public Google Drive. An entry says where the song's
// <X>_P.pak and <X>_P.sig are - one archive and the two entries inside it, or two loose files in a folder - so the
// loader can fetch, extract and check them itself.
package community

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Kinds of entry.
const (
	KindArchive = "archive" // one .zip / .rar holding <X>_P.pak and <X>_P.sig
	KindFolder  = "folder"  // a Drive folder holding the two files loose
)

// Archives are the archive formats the loader extracts (libarchive).
var Archives = []string{"zip", "rar4", "rar5"}

// MaxFileBytes caps one downloaded file (an archive holds one ~4-20 MB pak).
const MaxFileBytes = 128 << 20

// File is one Google Drive file of an entry.
type File struct {
	ID   string `json:"id"`   // the Drive file id
	Name string `json:"name"` // its file name on Drive
	Size int64  `json:"size"` // its size in bytes
}

// Song is one community song entry. It is also the line format of "fuserd community import" (one JSON object per
// line), which is what tools/community_sync.py writes.
type Song struct {
	X         string `json:"x"`         // the in-game shortname: the crate path is /Game/DLC/Songs/<X>/Meta_<X>
	Kind      string `json:"kind"`      // KindArchive or KindFolder
	Archive   string `json:"archive"`   // zip, rar4 or rar5 (archive only)
	Files     []File `json:"files"`     // archive: the archive; folder: the pak and the sig
	PakEntry  string `json:"pak_entry"` // archive only: the pak's path inside the archive, '/'-separated
	SigEntry  string `json:"sig_entry"` // archive only: the sig's path inside the archive
	Link      string `json:"link"`      // the link as the sheet gives it
	Artist    string `json:"artist"`
	Title     string `json:"title"`
	Creator   string `json:"creator"`
	Submitted string `json:"submitted"` // the sheet's Submit Date
	Sheet     string `json:"sheet"`     // <gid>:<row> on the sheet, for the log
	Note      string `json:"note"`      // free text: the creator's permission, say
}

var (
	shortNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	driveIDPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{10,128}$`)
)

// ErrHarmonix refuses rows credited to Harmonix: those are the game's own songs.
var ErrHarmonix = errors.New("the row is credited to Harmonix (official content is never linked)")

// Validate checks an entry's shape. It doesn't contact Drive.
func (s *Song) Validate() error {
	if !shortNamePattern.MatchString(s.X) {
		return fmt.Errorf("x %q is not a shortname (letters, digits, '_' and '-', at most 64)", s.X)
	}
	if strings.Contains(strings.ToLower(s.Creator), "harmonix") {
		return ErrHarmonix
	}
	if u, err := url.Parse(s.Link); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("link %q is not an http(s) URL", s.Link)
	}
	for i, f := range s.Files {
		if !driveIDPattern.MatchString(f.ID) {
			return fmt.Errorf("files[%d]: %q is not a Drive file id", i, f.ID)
		}
		if f.Size <= 0 || f.Size > MaxFileBytes {
			return fmt.Errorf("files[%d]: size %d is outside 1..%d", i, f.Size, MaxFileBytes)
		}
		if f.Name == "" || strings.ContainsAny(f.Name, "/\\") {
			return fmt.Errorf("files[%d]: name %q must be a plain file name", i, f.Name)
		}
	}
	pak, sig := s.X+"_P.pak", s.X+"_P.sig"
	switch s.Kind {
	case KindArchive:
		if len(s.Files) != 1 {
			return fmt.Errorf("an archive entry has exactly one file, not %d", len(s.Files))
		}
		known := false
		for _, a := range Archives {
			known = known || s.Archive == a
		}
		if !known {
			return fmt.Errorf("archive %q is not one of %s", s.Archive, strings.Join(Archives, ", "))
		}
		if err := checkEntry(s.PakEntry, pak); err != nil {
			return fmt.Errorf("pak_entry: %w", err)
		}
		if err := checkEntry(s.SigEntry, sig); err != nil {
			return fmt.Errorf("sig_entry: %w", err)
		}
	case KindFolder:
		if s.Archive != "" || s.PakEntry != "" || s.SigEntry != "" {
			return errors.New("a folder entry has no archive, pak_entry or sig_entry")
		}
		if len(s.Files) != 2 || !strings.EqualFold(s.Files[0].Name, pak) || !strings.EqualFold(s.Files[1].Name, sig) {
			return fmt.Errorf("a folder entry has exactly two files, %s then %s", pak, sig)
		}
	default:
		return fmt.Errorf("kind %q is not %s or %s", s.Kind, KindArchive, KindFolder)
	}
	return nil
}

// checkEntry accepts a relative '/'-separated archive path whose last element is want (any case).
func checkEntry(entry, want string) error {
	if entry == "" {
		return errors.New("missing")
	}
	if strings.HasPrefix(entry, "/") || strings.Contains(entry, "\\") || strings.Contains(entry, ":") {
		return fmt.Errorf("%q must be a relative path with '/' separators", entry)
	}
	parts := strings.Split(entry, "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return fmt.Errorf("%q has an empty, '.' or '..' element", entry)
		}
	}
	if !strings.EqualFold(parts[len(parts)-1], want) {
		return fmt.Errorf("%q does not end in %s", entry, want)
	}
	return nil
}

// DownloadURL is where the loader fetches a Drive file: the direct download, which skips the virus-scan page and
// honours byte ranges.
func DownloadURL(id string) string {
	return "https://drive.usercontent.google.com/download?id=" + url.QueryEscape(id) + "&export=download&confirm=t"
}

// ShortNameOfPath is the <X> of a crate song path (/Game/DLC/Songs/<X>/Meta_<X>[.Meta_<X>], or under
// /Game/Audio/Songs/), in the path's own case; "" when the path isn't a song Meta path.
func ShortNameOfPath(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, ':'); i >= 0 {
		p = p[:i]
	}
	if slash := strings.LastIndexByte(p, '/'); slash >= 0 {
		if dot := strings.IndexByte(p[slash:], '.'); dot >= 0 {
			p = p[:slash+dot]
		}
	}
	parts := strings.Split(p, "/")
	// "", "Game", "DLC"|"Audio", "Songs", <X>, "Meta_<X>"
	if len(parts) != 6 || parts[0] != "" || !strings.EqualFold(parts[1], "Game") ||
		!(strings.EqualFold(parts[2], "DLC") || strings.EqualFold(parts[2], "Audio")) || !strings.EqualFold(parts[3], "Songs") {
		return ""
	}
	x := parts[4]
	if !strings.EqualFold(parts[5], "Meta_"+x) || !shortNamePattern.MatchString(x) {
		return ""
	}
	return x
}
