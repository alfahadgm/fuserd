package api

// Featured Diamond Store art. A featured tile (WBP_Element_Collection_Browse_DLCSongWidget with A_Featured_Item played
// to its end) is 396 x 741, 8:15, and its art fills the whole tile; it loads featured_image_url itself (SetImageAsync)
// and applies it with SetBrushFromTexture(texture, bMatchSize=false), which stretches any image to fill. A standard
// tile's art is square (288 x 288) and comes from the song itself. So a featured item's image that isn't 8:15 (square
// album art, or an upload of another shape) is centre-cropped to 8:15 here and published as control media; an 8:15
// image is sent as it is. Crops are made once per source URL and kept for the process's lifetime: a new upload or a
// new catalogue version is a new URL.

import (
	"bytes"
	"context"
	"image"
	"image/draw"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"fuseronline/internal/catalog"
	"fuseronline/internal/logx"

	xdraw "golang.org/x/image/draw"
)

const (
	featuredAspectW, featuredAspectH = 8, 15
	featuredMaxHeight                = 1482 // 2x the tile's 741: larger crops are scaled down
)

// featuredCrops maps a featured item's source art URL to the URL sent as its featured_image_url.
type featuredCrops struct {
	mu sync.Mutex
	m  map[string]string
}

// isFeaturedAspect reports whether w x h is 8:15, within 1% (an exact-ratio upload from an editor may round).
func isFeaturedAspect(w, h int) bool {
	diff := w*featuredAspectH - h*featuredAspectW
	if diff < 0 {
		diff = -diff
	}
	return diff*100 <= h*featuredAspectW
}

// cropFeatured centre-crops im to 8:15 (the larger dimension is trimmed equally on both sides) and scales the crop
// down to featuredMaxHeight if it is taller.
func cropFeatured(im image.Image) image.Image {
	b := im.Bounds()
	w, h := b.Dx(), b.Dy()
	crop := b
	if w*featuredAspectH > h*featuredAspectW { // too wide: trim the sides
		cw := h * featuredAspectW / featuredAspectH
		crop.Min.X += (w - cw) / 2
		crop.Max.X = crop.Min.X + cw
	} else { // too tall: trim top and bottom
		ch := w * featuredAspectH / featuredAspectW
		crop.Min.Y += (h - ch) / 2
		crop.Max.Y = crop.Min.Y + ch
	}
	if crop.Dy() > featuredMaxHeight {
		out := image.NewNRGBA(image.Rect(0, 0, featuredMaxHeight*featuredAspectW/featuredAspectH, featuredMaxHeight))
		xdraw.CatmullRom.Scale(out, out.Bounds(), im, crop, xdraw.Src, nil)
		return out
	}
	out := image.NewNRGBA(image.Rect(0, 0, crop.Dx(), crop.Dy()))
	draw.Draw(out, out.Bounds(), im, crop.Min, draw.Src)
	return out
}

// featuredArt is the featured_image_url for a featured item whose art is art (an uploaded panel image, the
// catalogue art, or an external URL): art itself when it is already 8:15 or can't be read (an external URL is never
// fetched), else the URL of its 8:15 centre crop.
func (s *Server) featuredArt(ctx context.Context, it catalog.Item, art string) string {
	if art == "" {
		return ""
	}
	s.featured.mu.Lock()
	if out, ok := s.featured.m[art]; ok {
		s.featured.mu.Unlock()
		return out
	}
	s.featured.mu.Unlock()
	out := s.makeFeaturedArt(ctx, it, art)
	s.featured.mu.Lock()
	if s.featured.m == nil {
		s.featured.m = map[string]string{}
	}
	s.featured.m[art] = out
	s.featured.mu.Unlock()
	return out
}

func (s *Server) makeFeaturedArt(ctx context.Context, it catalog.Item, art string) string {
	lg := s.logs.For(logx.Store)
	var src []byte
	var err error
	switch mediaPrefix := s.cfg.PublicURL + "/control/media/"; {
	case strings.HasPrefix(art, mediaPrefix):
		src, err = s.store.ControlMedia(ctx, strings.TrimPrefix(art, mediaPrefix))
	case it.Art != "" && art == s.artURL(it):
		if c := s.cat(); c != nil {
			src, err = os.ReadFile(filepath.Join(c.Dir(it), it.Art))
		}
	default:
		lg.Info("featured art: external image sent as it is (not cropped to 8:15)", "item", it.UUID)
		return art
	}
	if err != nil || len(src) == 0 {
		lg.Warn("featured art: source not readable; sent as it is", "item", it.UUID, "err", err)
		return art
	}
	im, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		lg.Warn("featured art: source not decodable; sent as it is", "item", it.UUID, "err", err)
		return art
	}
	b := im.Bounds()
	if isFeaturedAspect(b.Dx(), b.Dy()) {
		return art
	}
	crop := cropFeatured(im)
	out, err := encodePNG8(crop)
	if err == nil {
		var hash string
		if hash, err = s.store.ControlMediaSave(ctx, out); err == nil {
			lg.Info("featured art: centre-cropped to 8:15", "item", it.UUID, "from", b.Size().String(), "to", crop.Bounds().Size().String())
			return s.cfg.PublicURL + "/control/media/" + hash
		}
	}
	lg.Warn("featured art: crop not stored; sent as it is", "item", it.UUID, "err", err)
	return art
}
