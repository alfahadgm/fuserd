package api

// Main Stage event cards: instead of stock FUSER's spectator screenshots, fuserd renders each event's image from its
// headliners: one tile per booked slot with the headliner's profile picture and display name (without the #NNNN
// suffix), under a "Headliners" subheader. While slots can still be claimed (the next event, open for purchase),
// the open slots are drawn too. An event's or the rotation's image_url always replaces the card. The card is keyed
// by its content, so a claimed or freed slot, or a new profile picture, gives a new key: new URLs and a new
// modified_time under the event's fixed image uuid, and clients fetch it again
// (cardImagesJSON). Cards are rendered on the schedule snapshot that first sees a key and stored under
// main_stage.image_dir in three sizes.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"fuseronline/internal/mainstage"
	"fuseronline/internal/store"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// cardSizes are the rendered sizes: FMainStageImages' image_download_url, image_medium_download_url and
// image_small_download_url.
var cardSizes = []cardSize{{"large", 1280, 720}, {"medium", 640, 360}, {"small", 320, 180}}

type cardSize struct {
	name string
	w, h int
}

// cardSlot is one headliner slot on a card: an empty Name is an open slot, drawn only while the card is Open.
type cardSlot struct {
	Blocked bool
	Slot    int
	Player  int64
	PicID   int32
	Pic     string // the profile picture file to draw (large if present), "" = none
	Name    string
}

// cardSpec is what a card shows; its key names the files.
type cardSpec struct {
	Event string
	Title string
	Slots []cardSlot
	Open  bool // slots can still be claimed (the next event, NEXT_OPEN): open slots are drawn
}

func (c cardSpec) key() string {
	h := sha256.New()
	fmt.Fprintf(h, "card-v7\x00%s\x00%s\x00%t", c.Event, c.Title, c.Open) // bump the version when the layout changes
	for _, s := range c.Slots {
		fmt.Fprintf(h, "\x00%d\x00%d\x00%d\x00%s", s.Slot, s.Player, s.PicID, s.Name)
		fmt.Fprintf(h, "%t", s.Blocked)
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

var cardKey = regexp.MustCompile(`^[0-9a-f]{24}$`)

// cardFonts are parsed once.
var cardFonts struct {
	once        sync.Once
	title, name font.Face
	small       font.Face
	err         error
	// noHeadliners is the empty card's text, with the first sad face the bold font has a glyph for (an emoji, then
	// U+2639, then ":(").
	noHeadliners string
}

func loadCardFonts() error {
	cardFonts.once.Do(func() {
		bold, err := opentype.Parse(gobold.TTF)
		if err != nil {
			cardFonts.err = err
			return
		}
		regular, err := opentype.Parse(goregular.TTF)
		if err != nil {
			cardFonts.err = err
			return
		}
		face := func(f *opentype.Font, size float64) font.Face {
			if cardFonts.err != nil {
				return nil
			}
			fc, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
			if err != nil {
				cardFonts.err = err
			}
			return fc
		}
		cardFonts.title = face(bold, 60)
		cardFonts.name = face(bold, 34)
		cardFonts.small = face(regular, 28)
		cardFonts.noHeadliners = "No headliners"
	})
	return cardFonts.err
}

// eventCard returns the card's key and when it was rendered, rendering it first if needed. Called while the
// schedule snapshot is built (once a second at most).
func (s *Server) eventCard(spec cardSpec) (string, time.Time, error) {
	key := spec.key()
	dir := s.cfg.MainStage.ImageDir
	large := filepath.Join(dir, key+"_large.jpg")
	if st, err := os.Stat(large); err == nil {
		return key, st.ModTime(), nil
	}
	imgs, err := renderCard(spec)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", time.Time{}, err
	}
	for i, size := range cardSizes {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, imgs[i], &jpeg.Options{Quality: 88}); err != nil {
			return "", time.Time{}, err
		}
		if err := writeFileAtomic(filepath.Join(dir, key+"_"+size.name+".jpg"), buf.Bytes()); err != nil {
			return "", time.Time{}, err
		}
	}
	pruneCards(dir, 14*24*time.Hour)
	st, err := os.Stat(large)
	if err != nil {
		return "", time.Time{}, err
	}
	return key, st.ModTime(), nil
}

// pruneCards removes cards not rendered for maxAge (an event's card is re-rendered whenever its headliners change,
// so old ones pile up).
func pruneCards(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() && time.Since(info.ModTime()) > maxAge {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// cardSlotFor is a booked slot's tile: the player's display name and the file of their current profile picture
// (the largest size uploaded), if any.
func (s *Server) cardSlotFor(ctx context.Context, slot int, p *store.Player) (cardSlot, error) {
	cs := cardSlot{Slot: slot, Player: p.ID, Name: displayName(p.HypeUsername)}
	if cs.Name == "" {
		cs.Name = "DJ"
	}
	if !s.cfg.ProfilePics.Enabled {
		return cs, nil
	}
	pic, err := s.store.ProfilePic(ctx, p.ID)
	if errors.Is(err, store.ErrNotFound) {
		return cs, nil
	}
	if err != nil {
		return cs, err
	}
	cs.PicID = pic.PicID
	dir := filepath.Join(s.cfg.ProfilePics.Dir, strconv.FormatInt(p.ID, 10))
	for _, sz := range picFallback["large"] {
		if !slices.Contains(pic.Sizes, sz) {
			continue
		}
		for _, ext := range []string{".jpg", ".png"} {
			if f := filepath.Join(dir, fmt.Sprintf("%d_%s%s", pic.PicID, sz, ext)); fileExists(f) {
				cs.Pic = f
				return cs, nil
			}
		}
	}
	return cs, nil
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// serveCard is GET /mainstage/image/<key>/<size>.jpg.
func (s *Server) serveCard(w http.ResponseWriter, r *http.Request) {
	key, size := r.PathValue("key"), strings.TrimSuffix(r.PathValue("file"), ".jpg")
	known := slices.ContainsFunc(cardSizes, func(c cardSize) bool { return c.name+".jpg" == r.PathValue("file") })
	if !known || !cardKey.MatchString(key) || !s.cfg.MainStage.Enabled {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.cfg.MainStage.ImageDir, key+"_"+size+".jpg"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // a new card has a new key
	http.ServeContent(w, r, "", time.Time{}, f)
}

// cardImagesJSON is FMainStageImages for a rendered card. The card is served like stock FUSER's live screenshots: one
// image slot per event with a fixed uuid, whose modified_time and URLs change on every re-render. The client keeps
// event images by uuid (UMainStageScheduleMgr +0x1b0, FUN_140638ff0): a known uuid with a new modified_time
// re-downloads from the new URL (the net cache is keyed by the URL's MD5) and swaps the texture in place, and the hub's
// WBP_MainStage_Button re-reads that uuid's texture every 5 s (ChangingBackgroundTimer → _SetNextBackgroundImage →
// GetNetCachedImage). A new uuid per render would instead depend on the widget picking up a new id list.
func (s *Server) cardImagesJSON(event, key string, rendered time.Time) []map[string]any {
	base := s.cfg.PublicURL + Prefix + "mainstage/image/" + key + "/"
	return []map[string]any{{"uuid": mainstage.DerivedUUID("mainstage-card", event),
		"image_download_url": base + "large.jpg", "image_medium_download_url": base + "medium.jpg",
		"image_small_download_url": base + "small.jpg", "modified_time": isoTime(rendered), "image_upload_url": ""}}
}

// ---------------------------------------------------------------------------------------------- drawing

var (
	cardTop    = color.RGBA{0x2c, 0x0d, 0x52, 0xff}
	cardBottom = color.RGBA{0x07, 0x03, 0x12, 0xff}
	cardRing   = color.RGBA{0xc9, 0xa7, 0xff, 0xff}
	cardOpen   = color.RGBA{0x2a, 0x22, 0x3d, 0xff}
	cardText   = color.RGBA{0xff, 0xff, 0xff, 0xff}
	cardDim    = color.RGBA{0x9d, 0x94, 0xb3, 0xff}
)

// renderCard draws the large card and scales it to the other sizes.
func renderCard(spec cardSpec) ([]image.Image, error) {
	if err := loadCardFonts(); err != nil {
		return nil, err
	}
	const W, H = 1280, 720
	dst := image.NewRGBA(image.Rect(0, 0, W, H))
	for y := 0; y < H; y++ { // vertical gradient
		t := float64(y) / float64(H-1)
		c := color.RGBA{lerp(cardTop.R, cardBottom.R, t), lerp(cardTop.G, cardBottom.G, t), lerp(cardTop.B, cardBottom.B, t), 0xff}
		draw.Draw(dst, image.Rect(0, y, W, y+1), &image.Uniform{c}, image.Point{}, draw.Src)
	}
	// The title sits low enough that the hub button's hover zoom doesn't cut it off.
	drawCentered(dst, cardFonts.title, fit(cardFonts.title, spec.Title, W-120), W/2, 170, cardText)

	// The tiles, one centred row in slot order: the booked headliners, plus the open slots
	// while slots can still be claimed (spec.Open: the next event, open for purchase). The subheader is "Headliners",
	// or "Headliner slots available!" over open slots alone. Nothing to draw (no headliners, purchase closed): "No
	// headliners" and a sad face under the title.
	var tiles []cardSlot
	booked := 0
	for _, sl := range spec.Slots {
		if sl.Name != "" {
			booked++
		}
		if !sl.Blocked && (sl.Name != "" || spec.Open) {
			tiles = append(tiles, sl)
		}
	}
	n := max(len(tiles), 1)
	const margin, gap = 70, 40
	size := min(240, (W-2*margin-(n-1)*gap)/n)
	rowW := n*size + (n-1)*gap
	x0, y0 := (W-rowW)/2, 310
	switch {
	case booked > 0:
		drawCentered(dst, cardFonts.small, "Headliners", W/2, y0-36, cardRing)
	case len(tiles) > 0:
		drawCentered(dst, cardFonts.small, "Headliner slots available!", W/2, y0-36, cardRing)
	default: // nothing to draw: say so under the title
		drawCentered(dst, cardFonts.name, cardFonts.noHeadliners, W/2, 260, cardDim)
	}
	for i, sl := range tiles {
		x := x0 + i*(size+gap)
		box := image.Rect(x, y0, x+size, y0+size)
		ring := box.Inset(-6)
		if sl.Name == "" {
			fillCircle(dst, ring, cardDim)
			fillCircle(dst, box, cardOpen)
			drawCentered(dst, cardFonts.title, strconv.Itoa(sl.Slot), x+size/2, y0+size/2+22, cardDim)
			drawCentered(dst, cardFonts.small, "Open slot", x+size/2, y0+size+60, cardDim)
			continue
		}
		fillCircle(dst, ring, cardRing)
		if avatar := loadAvatar(sl.Pic); avatar != nil {
			drawCircleImage(dst, box, avatar)
		} else {
			fillCircle(dst, box, cardOpen)
			drawCentered(dst, cardFonts.title, initial(sl.Name), x+size/2, y0+size/2+22, cardText)
		}
		drawCentered(dst, cardFonts.name, fit(cardFonts.name, sl.Name, size+gap-8), x+size/2, y0+size+64, cardText)
	}
	out := []image.Image{dst}
	for _, cs := range cardSizes[1:] {
		small := image.NewRGBA(image.Rect(0, 0, cs.w, cs.h))
		xdraw.CatmullRom.Scale(small, small.Bounds(), dst, dst.Bounds(), xdraw.Src, nil)
		out = append(out, small)
	}
	return out, nil
}

func lerp(a, b uint8, t float64) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t) }

func initial(name string) string {
	for _, r := range name {
		return string(r)
	}
	return "?"
}

// fit shortens text with an ellipsis until it is at most width pixels wide.
func fit(face font.Face, text string, width int) string {
	if font.MeasureString(face, text).Ceil() <= width {
		return text
	}
	runes := []rune(text)
	for len(runes) > 1 {
		runes = runes[:len(runes)-1]
		if t := string(runes) + "…"; font.MeasureString(face, t).Ceil() <= width {
			return t
		}
	}
	return string(runes)
}

func drawCentered(dst draw.Image, face font.Face, text string, cx, baseline int, c color.Color) {
	d := font.Drawer{Dst: dst, Src: &image.Uniform{c}, Face: face}
	w := d.MeasureString(text)
	d.Dot = fixed.Point26_6{X: fixed.I(cx) - w/2, Y: fixed.I(baseline)}
	d.DrawString(text)
}

// circleMask is an anti-aliased disc filling r.
func circleMask(r image.Rectangle) *image.Alpha {
	m := image.NewAlpha(r)
	cx, cy := float64(r.Min.X+r.Max.X)/2, float64(r.Min.Y+r.Max.Y)/2
	rad := float64(r.Dx()) / 2
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			d := rad - math.Hypot(dx, dy)
			switch {
			case d >= 1:
				m.SetAlpha(x, y, color.Alpha{0xff})
			case d > 0:
				m.SetAlpha(x, y, color.Alpha{uint8(d * 0xff)})
			}
		}
	}
	return m
}

func fillCircle(dst draw.Image, r image.Rectangle, c color.Color) {
	draw.DrawMask(dst, r, &image.Uniform{c}, image.Point{}, circleMask(r), r.Min, draw.Over)
}

// drawCircleImage draws src, cropped to a centred square and scaled, inside the disc r.
func drawCircleImage(dst draw.Image, r image.Rectangle, src image.Image) {
	b := src.Bounds()
	side := min(b.Dx(), b.Dy())
	crop := image.Rect(b.Min.X+(b.Dx()-side)/2, b.Min.Y+(b.Dy()-side)/2, 0, 0)
	crop.Max = crop.Min.Add(image.Pt(side, side))
	scaled := image.NewRGBA(r)
	xdraw.CatmullRom.Scale(scaled, r, src, crop, xdraw.Src, nil)
	draw.DrawMask(dst, r, scaled, r.Min, circleMask(r), r.Min, draw.Over)
}

func loadAvatar(path string) image.Image {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var img image.Image
	if filepath.Ext(path) == ".png" {
		img, err = png.Decode(f)
	} else {
		img, err = jpeg.Decode(f)
	}
	if err != nil {
		return nil
	}
	return img
}
