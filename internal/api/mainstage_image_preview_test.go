package api

// A preview of the Main Stage event cards for tweaking renderCard (mainstage_image.go): it renders a set of cards
// from random real profile pictures and writes them, with an index.html contact sheet, to build/card-preview.
// It only runs on request:
//
//	CARD_PREVIEW=1 go test ./internal/api -run TestCardPreview -v
//
// CARD_PREVIEW_PICS is the picture directory (<dir>/<player>/<pic>_<size>.jpg, default ../profile_pics next to the
// server module); CARD_PREVIEW_OUT the output directory; CARD_PREVIEW_SEED repeats a run's picks (the seed is
// logged).

import (
	"fmt"
	"html"
	"image/jpeg"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCardPreview(t *testing.T) {
	if os.Getenv("CARD_PREVIEW") == "" {
		t.Skip("set CARD_PREVIEW=1 to render the card preview")
	}
	picsDir := envOr("CARD_PREVIEW_PICS", filepath.Join("..", "..", "..", "profile_pics"))
	outDir := envOr("CARD_PREVIEW_OUT", filepath.Join("..", "..", "build", "card-preview"))
	seed := time.Now().UnixNano()
	if s := os.Getenv("CARD_PREVIEW_SEED"); s != "" {
		var err error
		if seed, err = strconv.ParseInt(s, 10, 64); err != nil {
			t.Fatalf("CARD_PREVIEW_SEED: %v", err)
		}
	}
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d (CARD_PREVIEW_SEED=%d repeats this run)", seed, seed)

	pics := previewPictures(t, picsDir)
	if len(pics) == 0 {
		t.Fatalf("no profile pictures under %s", picsDir)
	}
	t.Logf("%d profile picture(s) from %s", len(pics), picsDir)
	rng.Shuffle(len(pics), func(i, j int) { pics[i], pics[j] = pics[j], pics[i] })
	nextPic := 0
	pic := func() string { // cycles through the shuffled pictures, so a small set still fills every card
		p := pics[nextPic%len(pics)]
		nextPic++
		return p
	}
	names := []string{"_Gumby#3874", "DJ Night Owl#0042", "bassface#1999", "Mx_Tempo#7710", "Lowkey#0001",
		"xX_DropTheBeat_Xx#5150", "Zoë Ångström#2468", "Ночной_DJ#3131", "a#1111", "TheVeryLongestHeadlinerNameAllowed#9999"}
	rng.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	nextName := 0
	name := func() string { // cycles too, so names differ within a card
		n := names[nextName%len(names)]
		nextName++
		return displayName(n)
	}

	type preview struct {
		file, title string
		spec        cardSpec
	}
	slots := func(n int, fill func(i int) cardSlot) []cardSlot {
		var s []cardSlot
		for i := 1; i <= n; i++ {
			cs := fill(i)
			cs.Slot = i
			s = append(s, cs)
		}
		return s
	}
	booked := func(int) cardSlot { return cardSlot{Player: rng.Int63n(1000) + 1, Name: name(), Pic: pic()} }
	open := func(int) cardSlot { return cardSlot{} }
	previews := []preview{
		{"full", "4 headliners", cardSpec{Title: "Diamond Stage", Slots: slots(4, booked)}},
		{"partial", "2 of 4 booked (slots 1 and 3), purchase open", cardSpec{Title: "Diamond Stage", Open: true, Slots: slots(4, func(i int) cardSlot {
			if i%2 == 1 {
				return booked(i)
			}
			return open(i)
		})}},
		{"empty", "nobody booked, purchase open", cardSpec{Title: "Diamond Stage", Open: true, Slots: slots(4, open)}},
		{"empty-closed", "nobody booked, purchase closed or in progress", cardSpec{Title: "Diamond Stage", Slots: slots(4, open)}},
		{"partial-closed", "2 of 4 booked, purchase closed or in progress", cardSpec{Title: "Diamond Stage", Slots: slots(4, func(i int) cardSlot {
			if i <= 2 {
				return booked(i)
			}
			return open(i)
		})}},
		{"no-pictures", "headliners without pictures, long names", cardSpec{Title: "Diamond Stage",
			Slots: slots(4, func(i int) cardSlot {
				long := []string{"TheVeryLongestHeadlinerNameAllowed#9999", "xX_DropTheBeat_Xx#5150", "Zoë Ångström#2468",
					"Ночной_DJ#3131"}
				return cardSlot{Player: int64(i), Name: displayName(long[i-1])}
			})}},
		{"special", "special event, long title, 3 booked", cardSpec{Title: "Diamond Stage Halloween Special: Spooky Remix Showdown",
			Slots: slots(4, func(i int) cardSlot {
				if i == 4 {
					return open(i)
				}
				return booked(i)
			})}},
		{"one-slot", "1 slot (main_stage.slots = 1)", cardSpec{Title: "Diamond Stage", Slots: slots(1, booked)}},
		{"eight-slots", "8 slots (the maximum), 5 booked, purchase open", cardSpec{Title: "Diamond Stage", Open: true, Slots: slots(8, func(i int) cardSlot {
			if i <= 5 {
				return booked(i)
			}
			return open(i)
		})}},
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var sheet strings.Builder
	fmt.Fprintf(&sheet, "<!doctype html><meta charset=utf-8><title>Card preview</title>"+
		"<style>body{background:#222;color:#ddd;font:14px sans-serif;margin:16px}img{display:block;margin:4px 0 8px;"+
		"max-width:100%%}.row{display:flex;gap:12px;align-items:flex-start;flex-wrap:wrap}</style>"+
		"<p>Seed %d, rendered %s</p>", seed, time.Now().Format(time.RFC3339))
	for _, p := range previews {
		p.spec.Event = p.file
		start := time.Now()
		imgs, err := renderCard(p.spec)
		if err != nil {
			t.Fatalf("%s: %v", p.file, err)
		}
		took := time.Since(start)
		fmt.Fprintf(&sheet, "<h3>%s</h3><div class=row>", html.EscapeString(p.title))
		for i, size := range cardSizes {
			f := fmt.Sprintf("%s_%s.jpg", p.file, size.name)
			out, err := os.Create(filepath.Join(outDir, f))
			if err != nil {
				t.Fatal(err)
			}
			err = jpeg.Encode(out, imgs[i], &jpeg.Options{Quality: 88})
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&sheet, "<div>%s %dx%d<img src=%q width=%d></div>", size.name, size.w, size.h, f, size.w)
		}
		sheet.WriteString("</div>")
		t.Logf("%-12s rendered in %v", p.file, took.Round(time.Millisecond))
	}
	index := filepath.Join(outDir, "index.html")
	if err := os.WriteFile(index, []byte(sheet.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(index)
	t.Logf("contact sheet: %s", abs)
}

// previewPictures returns one file per player under dir: the largest size of their newest picture.
func previewPictures(t *testing.T, dir string) []string {
	t.Helper()
	players, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("CARD_PREVIEW_PICS: %v", err)
	}
	var out []string
	for _, p := range players {
		if !p.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(dir, p.Name()))
		best, bestID, bestRank := "", -1, len(picSizes)
		for _, f := range files {
			base := strings.TrimSuffix(strings.TrimSuffix(f.Name(), ".jpg"), ".png")
			id, size, ok := strings.Cut(base, "_")
			n, err := strconv.Atoi(id)
			rank := slices.Index(picSizes, size) // large first
			if !ok || err != nil || rank < 0 || base == f.Name() {
				continue
			}
			if n > bestID || (n == bestID && rank < bestRank) {
				best, bestID, bestRank = filepath.Join(dir, p.Name(), f.Name()), n, rank
			}
		}
		if best != "" {
			out = append(out, best)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
