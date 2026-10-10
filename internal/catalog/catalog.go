// Package catalog is the community song catalogue: licence-permitted custom song paks that fuserd serves, the
// metadata the game's store endpoints describe them with, and the builder that adds a pak:
// a player's loader resolves a crate path it can't load to a catalogue entry, downloads the pak + .sig, checks the
// hashes and mounts it.
package catalog

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// CommunityMountPoint is the mount point of a community song pak: the content root. Its stems live under
// /Game/Audio/Songs/<shortname>/, the song's CelSongSet under /Game/DLC/Songs/<shortname>/ (seen in real community
// paks; /Game/Audio/Songs/<shortname>/ is also accepted) and its album art at /Game/UI/AlbumArt/T_<shortname>[_small].
const CommunityMountPoint = "../../../Fuser/Content/"

// sigMagic starts a .sig file (FPakSignatureFile).
const sigMagic = 0x73832DAA

type Artist struct {
	ID   int32  `json:"artist_id"`
	Name string `json:"artist_name"`
}

type Genre struct {
	ID   int32  `json:"genre_id"`
	Name string `json:"genre_name"`
}

// Genres are EGenre's names by index; genre_id is the index.
var Genres = []string{"None", "Classical", "Country", "Dance", "HipHop", "LatinAndCaribbean", "Pop", "RnB", "Rock"}

// Item is one catalogue song: the FStoreItemResponse fields the game is sent, plus server-only fields (below
// Version) that never leave fuserd except through the loader's resolve endpoint.
type Item struct {
	UUID        string `json:"uuid"`
	ShortName   string `json:"shortName"`
	Name        string `json:"name"`
	SongName    string `json:"song_name"`
	AlbumName   string `json:"album_name"`
	Artist      Artist `json:"artist"`
	Genre       Genre  `json:"genre"`
	Year        int32  `json:"year"`
	Version     int32  `json:"version"`
	ReleaseDate string `json:"release_date"`

	Licence    string `json:"licence"`
	Source     string `json:"source"`
	MetaPath   string `json:"meta_path"` // the song CelSongSet's object path, what a crate's soft reference holds
	MountPoint string `json:"mount_point"`
	PakSHA256  string `json:"pak_sha256"`
	PakSize    int64  `json:"pak_size"`
	SigSHA256  string `json:"sig_sha256"`
	SigSize    int64  `json:"sig_size"`
	Added      string `json:"added"`
	// Art is the album art image stored next to the pak (<shortname>_art.png/.jpg), served from /dl/ for the store
	// tiles of a song that isn't installed yet. Empty: none.
	Art string `json:"art,omitempty"`
	// ElderCredit publishes the song in the game's Elder Credit ("Diamond") store. Nil: not listed.
	ElderCredit *Listing `json:"elder_credit,omitempty"`
}

// Listing is a song's Elder Credit store offer. The game refuses to buy an offer whose end date has passed, and
// shows at most 2 featured and 8 standard offers.
type Listing struct {
	Price     int32  `json:"price"`
	Featured  bool   `json:"featured"`
	StartDate string `json:"start_date"` // ISO 8601, as the game parses it (FDateTime::ParseIso8601)
	EndDate   string `json:"end_date"`
}

// DefaultListingEnd is a published offer's end date when none is given: far enough away to never expire.
const DefaultListingEnd = "2099-12-31T23:59:59Z"

// Active reports whether the offer is open at t. Unparseable dates count as open (start) / never ending (end).
func (l *Listing) Active(t time.Time) bool {
	if l == nil {
		return false
	}
	if st, err := parseListingTime(l.StartDate); err == nil && t.Before(st) {
		return false
	}
	if end, err := parseListingTime(l.EndDate); err == nil && !t.Before(end) {
		return false
	}
	return true
}

func parseListingTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}

// PakName / SigName are the stored (and served) file names: the _P suffix keeps the loader's existing pak-signing
// bypass in force when the loader saves them as-is.
func (it Item) PakName() string { return it.ShortName + "_P.pak" }
func (it Item) SigName() string { return it.ShortName + "_P.sig" }

// BinName is the game's DLC download container (int32 sigLen, .sig, .pak), served for pc_asset_download_url.
func (it Item) BinName() string { return it.ShortName + ".bin" }

// Catalog is a loaded catalogue file plus the directory its files are stored in (<dir>/<shortname>/<version>/).
type Catalog struct {
	path, filesDir string
	items          []Item
	byShort        map[string]int
	byMeta         map[string]int
	byUUID         map[string]int
}

// Load reads the catalogue at path. A missing file is an empty catalogue. Every entry is validated, and its files
// must exist with the recorded sizes.
func Load(path, filesDir string) (*Catalog, error) {
	c := &Catalog{path: path, filesDir: filesDir}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		c.index()
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &c.items); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for _, it := range c.items {
		if err := it.validate(); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, it.ShortName, err)
		}
		k := strings.ToLower(it.ShortName)
		if seen[k] {
			return nil, fmt.Errorf("%s: duplicate shortName %s", path, it.ShortName)
		}
		seen[k] = true
		for _, f := range []struct {
			name string
			size int64
		}{{it.PakName(), it.PakSize}, {it.SigName(), it.SigSize}, {it.Art, -1}} {
			if f.name == "" {
				continue
			}
			st, err := os.Stat(filepath.Join(c.Dir(it), f.name))
			if err != nil {
				return nil, fmt.Errorf("%s: %s: %w", path, it.ShortName, err)
			}
			if f.size >= 0 && st.Size() != f.size {
				return nil, fmt.Errorf("%s: %s: %s is %d bytes, catalogue says %d", path, it.ShortName, f.name,
					st.Size(), f.size)
			}
		}
	}
	c.index()
	return c, nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var shortNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

func (it Item) validate() error {
	switch {
	case !uuidPattern.MatchString(it.UUID):
		return errors.New("uuid is not a GUID")
	case !shortNamePattern.MatchString(it.ShortName):
		return errors.New("shortName must be 1-64 letters, digits or underscores")
	case it.Version < 1:
		return errors.New("version must be at least 1")
	case it.Licence == "":
		return errors.New("licence is required")
	case it.MetaPath != MetaPathFor("Audio", it.ShortName) && it.MetaPath != MetaPathFor("DLC", it.ShortName):
		return errors.New("meta_path does not match shortName")
	case len(it.PakSHA256) != 64 || len(it.SigSHA256) != 64:
		return errors.New("pak_sha256 / sig_sha256 must be hex SHA-256")
	case it.Genre.ID < 0 || int(it.Genre.ID) >= len(Genres):
		return errors.New("genre_id is not an EGenre index")
	case it.Art != "" && it.Art != it.ArtName(filepath.Ext(it.Art)):
		return errors.New("art must be <shortName>_art.png or .jpg")
	}
	if l := it.ElderCredit; l != nil {
		if l.Price < 0 {
			return errors.New("elder_credit.price must not be negative")
		}
		for _, d := range []string{l.StartDate, l.EndDate} {
			if _, err := parseListingTime(d); d != "" && err != nil {
				return fmt.Errorf("elder_credit date %q is not ISO 8601", d)
			}
		}
	}
	return nil
}

// ArtName is the stored album art file name for an image with extension ext (".png" / ".jpg").
func (it Item) ArtName(ext string) string {
	ext = strings.ToLower(ext)
	if ext == ".jpeg" {
		ext = ".jpg"
	}
	if ext != ".png" && ext != ".jpg" {
		return ""
	}
	return it.ShortName + "_art" + ext
}

func (c *Catalog) index() {
	sort.SliceStable(c.items, func(i, j int) bool {
		return strings.ToLower(c.items[i].ShortName) < strings.ToLower(c.items[j].ShortName)
	})
	c.byShort = map[string]int{}
	c.byMeta = map[string]int{}
	c.byUUID = map[string]int{}
	for i, it := range c.items {
		c.byShort[strings.ToLower(it.ShortName)] = i
		c.byMeta[NormalizeMetaPath(it.MetaPath)] = i
		c.byUUID[strings.ToLower(it.UUID)] = i
	}
}

// Items returns a copy of every entry, sorted by shortname.
func (c *Catalog) Items() []Item { return append([]Item(nil), c.items...) }

// ByShortName finds an entry, ignoring case.
func (c *Catalog) ByShortName(shortName string) (Item, bool) {
	i, ok := c.byShort[strings.ToLower(strings.TrimSpace(shortName))]
	if !ok {
		return Item{}, false
	}
	return c.items[i], true
}

// ByMetaPath finds an entry by the song's object or package path (see NormalizeMetaPath).
func (c *Catalog) ByMetaPath(path string) (Item, bool) {
	i, ok := c.byMeta[NormalizeMetaPath(path)]
	if !ok {
		return Item{}, false
	}
	return c.items[i], true
}

// ByUUID finds an entry by its uuid (the Elder Credit store's offer id), ignoring case.
func (c *Catalog) ByUUID(uuid string) (Item, bool) {
	i, ok := c.byUUID[strings.ToLower(strings.TrimSpace(uuid))]
	if !ok {
		return Item{}, false
	}
	return c.items[i], true
}

// Publish lists an entry in the Elder Credit store (or replaces its listing); an empty end date never expires.
// The catalogue is not saved; call Save.
func (c *Catalog) Publish(shortName string, l Listing) (Item, error) {
	i, ok := c.byShort[strings.ToLower(strings.TrimSpace(shortName))]
	if !ok {
		return Item{}, fmt.Errorf("%s is not in the catalogue", shortName)
	}
	if l.EndDate == "" {
		l.EndDate = DefaultListingEnd
	}
	it := c.items[i]
	it.ElderCredit = &l
	if err := it.validate(); err != nil {
		return Item{}, err
	}
	c.items[i] = it
	return it, nil
}

// Unpublish removes an entry's Elder Credit listing. Players who bought it keep it.
func (c *Catalog) Unpublish(shortName string) (Item, error) {
	i, ok := c.byShort[strings.ToLower(strings.TrimSpace(shortName))]
	if !ok {
		return Item{}, fmt.Errorf("%s is not in the catalogue", shortName)
	}
	c.items[i].ElderCredit = nil
	return c.items[i], nil
}

// Dir is where an entry's files are stored.
func (c *Catalog) Dir(it Item) string {
	return filepath.Join(c.filesDir, it.ShortName, fmt.Sprint(it.Version))
}

// MetaPathFor is a song CelSongSet's object path under /Game/<root>/Songs/ (root "Audio" or "DLC").
func MetaPathFor(root, shortName string) string {
	return "/Game/" + root + "/Songs/" + shortName + "/Meta_" + shortName + ".Meta_" + shortName
}

// NormalizeMetaPath folds the forms a crate's soft reference may take - "/Game/.../Meta_X.Meta_X", the package path
// "/Game/.../Meta_X", either with a ":SubObject" - to one lower-case package path.
func NormalizeMetaPath(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, ':'); i >= 0 {
		p = p[:i]
	}
	if slash := strings.LastIndexByte(p, '/'); slash >= 0 {
		if dot := strings.IndexByte(p[slash:], '.'); dot >= 0 {
			p = p[:slash+dot]
		}
	}
	return strings.ToLower(p)
}

// Save writes the catalogue atomically.
func (c *Catalog) Save() error {
	items := c.items
	if items == nil {
		items = []Item{}
	}
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(c.path, append(b, '\n'))
}

// ---- the builder ----------------------------------------------------------------------------------------------

// Inspection is what a pak tells the builder about itself.
type Inspection struct {
	ShortName  string
	MetaPath   string
	MountPoint string
	Entries    int
}

var songMetaPattern = regexp.MustCompile(`^(Audio|DLC)/Songs/([^/]+)/Meta_([^/]+)\.uasset$`)

// InspectPak checks that the pak at path is a community song pak - the community mount point, no encrypted entries,
// exactly one song CelSongSet ({Audio,DLC}/Songs/<sn>/Meta_<sn>) and nothing outside that song's own folders
// (Audio/Songs/<sn>/, DLC/Songs/<sn>/, UI/AlbumArt/T_<sn>[_small]), so it can't override other game content - and
// returns its shortname and Meta path.
func InspectPak(path string) (Inspection, error) {
	f, err := os.Open(path)
	if err != nil {
		return Inspection{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Inspection{}, err
	}
	idx, err := ReadPakIndex(f, st.Size())
	if err != nil {
		return Inspection{}, err
	}
	in := Inspection{MountPoint: idx.MountPoint, Entries: len(idx.Entries)}
	if !strings.EqualFold(idx.MountPoint, CommunityMountPoint) {
		return in, fmt.Errorf("mount point %q is not the community song layout %q", idx.MountPoint, CommunityMountPoint)
	}
	var songs, roots []string
	for _, e := range idx.Entries {
		if e.Encrypted {
			return in, fmt.Errorf("entry %s is encrypted", e.Name)
		}
		if m := songMetaPattern.FindStringSubmatch(e.Name); m != nil && m[2] == m[3] {
			songs, roots = append(songs, m[2]), append(roots, m[1])
		}
	}
	if len(songs) != 1 {
		return in, fmt.Errorf("found %d song Meta assets ({Audio,DLC}/Songs/<sn>/Meta_<sn>.uasset), want exactly 1", len(songs))
	}
	in.ShortName = songs[0]
	if !shortNamePattern.MatchString(in.ShortName) {
		return in, fmt.Errorf("shortname %q must be 1-64 letters, digits or underscores", in.ShortName)
	}
	sn := strings.ToLower(in.ShortName)
	for _, e := range idx.Entries {
		if !ownedBySong(strings.ToLower(e.Name), sn) {
			return in, fmt.Errorf("entry %s is outside the song's own folders", e.Name)
		}
	}
	in.MetaPath = MetaPathFor(roots[0], in.ShortName)
	return in, nil
}

// ownedBySong says whether a (lower-case) pak entry belongs to song sn: its stems, its CelSongSet folder or its album
// art. Anything else could replace base-game content.
func ownedBySong(name, sn string) bool {
	if strings.HasPrefix(name, "audio/songs/"+sn+"/") || strings.HasPrefix(name, "dlc/songs/"+sn+"/") {
		return true
	}
	if art, ok := strings.CutPrefix(name, "ui/albumart/t_"+sn); ok {
		art = strings.TrimSuffix(strings.TrimSuffix(art, ".uasset"), ".uexp")
		return art == "" || art == "_small"
	}
	return false
}

// ValidateSig checks a .sig file's shape: its magic. Its cryptographic content is not checked (the loader's bypass
// trusts the hash in it), only that the game will parse it.
func ValidateSig(b []byte) error {
	if len(b) < 12 || binary.LittleEndian.Uint32(b) != sigMagic {
		return errors.New(".sig file has no FPakSignatureFile magic")
	}
	return nil
}

// AddOptions describes a pak to add. Name/SongName default to the shortname; Reserved holds shortnames that must
// not be used (official DLC, base-game songs), lower-cased.
type AddOptions struct {
	PakPath, SigPath string
	Licence, Source  string
	Name, SongName   string
	Album, Artist    string
	GenreID, Year    int32
	ReleaseDate      string
	Reserved         map[string]bool
	MaxPakBytes      int64
	Update           bool   // replace an existing entry's files (its version goes up)
	ArtPath          string // optional album art (.png / .jpg); an update without one keeps the current art
	Now              time.Time
}

// maxArtBytes bounds an album art image.
const maxArtBytes = 4 << 20

// ErrUnchanged is Add's answer for a pak + .sig identical to the current entry's.
var ErrUnchanged = errors.New("the catalogue already has these exact files")

// Add inspects a pak and its .sig, copies them into the files directory and records the entry (version 1, or the
// current version + 1 with Update). The catalogue is not saved; call Save.
func (c *Catalog) Add(o AddOptions) (Item, error) {
	if strings.TrimSpace(o.Licence) == "" {
		return Item{}, errors.New("a licence is required")
	}
	if o.GenreID < 0 || int(o.GenreID) >= len(Genres) {
		return Item{}, fmt.Errorf("genre %d is not an EGenre index (0-%d)", o.GenreID, len(Genres)-1)
	}
	in, err := InspectPak(o.PakPath)
	if err != nil {
		return Item{}, err
	}
	sig, err := os.ReadFile(o.SigPath)
	if err != nil {
		return Item{}, err
	}
	if err := ValidateSig(sig); err != nil {
		return Item{}, err
	}
	if o.Reserved[strings.ToLower(in.ShortName)] {
		return Item{}, fmt.Errorf("shortname %s is reserved (an official DLC or base-game song)", in.ShortName)
	}
	if o.ArtPath != "" {
		st, err := os.Stat(o.ArtPath)
		if err != nil {
			return Item{}, err
		}
		if (Item{ShortName: in.ShortName}).ArtName(filepath.Ext(o.ArtPath)) == "" {
			return Item{}, errors.New("album art must be a .png or .jpg file")
		}
		if st.Size() > maxArtBytes {
			return Item{}, fmt.Errorf("album art is %d bytes, over the %d byte limit", st.Size(), maxArtBytes)
		}
	}
	pakSum, pakSize, err := hashFile(o.PakPath)
	if err != nil {
		return Item{}, err
	}
	if o.MaxPakBytes > 0 && pakSize > o.MaxPakBytes {
		return Item{}, fmt.Errorf("pak is %d bytes, over the %d byte limit", pakSize, o.MaxPakBytes)
	}
	sigSum := sha256.Sum256(sig)
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	it := Item{
		ShortName: in.ShortName, Name: or(o.Name, in.ShortName), SongName: or(o.SongName, or(o.Name, in.ShortName)),
		AlbumName: o.Album, Artist: Artist{Name: or(o.Artist, "Community")},
		Genre: Genre{ID: o.GenreID, Name: Genres[o.GenreID]}, Year: o.Year, Version: 1,
		ReleaseDate: or(o.ReleaseDate, now.UTC().Format("2006-01-02")),
		Licence:     o.Licence, Source: o.Source, MetaPath: in.MetaPath, MountPoint: in.MountPoint,
		PakSHA256: pakSum, PakSize: pakSize, SigSHA256: hex.EncodeToString(sigSum[:]), SigSize: int64(len(sig)),
		Added: now.UTC().Format(time.RFC3339),
	}
	pos, exists := c.byShort[strings.ToLower(in.ShortName)]
	if exists {
		old := c.items[pos]
		if old.PakSHA256 == it.PakSHA256 && old.SigSHA256 == it.SigSHA256 && o.ArtPath == "" {
			return old, ErrUnchanged
		}
		if !o.Update {
			return Item{}, fmt.Errorf("%s is already in the catalogue (version %d); use update to replace it",
				in.ShortName, old.Version)
		}
		it.UUID, it.Version, it.ElderCredit = old.UUID, old.Version+1, old.ElderCredit
	} else if it.UUID, err = newUUID(); err != nil {
		return Item{}, err
	}
	dir := c.Dir(it)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Item{}, err
	}
	if err := copyAtomic(o.PakPath, filepath.Join(dir, it.PakName())); err != nil {
		return Item{}, err
	}
	if err := writeAtomic(filepath.Join(dir, it.SigName()), sig); err != nil {
		return Item{}, err
	}
	artSrc := o.ArtPath
	if artSrc == "" && exists && c.items[pos].Art != "" {
		artSrc = filepath.Join(c.Dir(c.items[pos]), c.items[pos].Art) // carried over to the new version
	}
	if artSrc != "" {
		it.Art = it.ArtName(filepath.Ext(artSrc))
		if err := copyAtomic(artSrc, filepath.Join(dir, it.Art)); err != nil {
			return Item{}, err
		}
	}
	if exists {
		c.items[pos] = it
	} else {
		c.items = append(c.items, it)
	}
	c.index()
	return it, nil
}

func or(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func copyAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// ---- a catalogue that follows its file ------------------------------------------------------------------------

// Live holds the catalogue a running server answers from, and reloads it when the file changes (so "fuserd
// catalogue add" takes effect without a restart). A reload that fails keeps the previous catalogue.
type Live struct {
	path, filesDir string
	onError        func(error)

	mu      sync.Mutex
	cur     *Catalog
	modTime time.Time
	checked time.Time
}

// NewLive loads the catalogue now; a failure here is returned (the server should not start with a bad file).
func NewLive(path, filesDir string, onError func(error)) (*Live, error) {
	l := &Live{path: path, filesDir: filesDir, onError: onError}
	c, err := Load(path, filesDir)
	if err != nil {
		return nil, err
	}
	l.cur = c
	if st, err := os.Stat(path); err == nil {
		l.modTime = st.ModTime()
	}
	l.checked = time.Now()
	return l, nil
}

// Static wraps a fixed catalogue (tests).
func Static(c *Catalog) *Live { return &Live{cur: c} }

// Get returns the current catalogue, checking the file at most every few seconds.
func (l *Live) Get() *Catalog {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.path != "" && time.Since(l.checked) > 3*time.Second {
		l.checked = time.Now()
		if st, err := os.Stat(l.path); err == nil && !st.ModTime().Equal(l.modTime) {
			l.modTime = st.ModTime() // a bad file is reported once, not every check
			if c, err := Load(l.path, l.filesDir); err != nil {
				if l.onError != nil {
					l.onError(err)
				}
			} else {
				l.cur = c
			}
		}
	}
	return l.cur
}
