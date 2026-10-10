package community

import (
	"errors"
	"strings"
	"testing"
)

func archiveSong() Song {
	return Song{X: "JoWo_SeinThem_ShpQn", Kind: KindArchive, Archive: "zip",
		Files:    []File{{ID: "1wJNLylzC6twJujxImriBGD5cRSE4ogW0", Name: "Seinfeld.zip", Size: 9_000_000}},
		PakEntry: "Seinfeld/JoWo_SeinThem_ShpQn_P.pak", SigEntry: "Seinfeld/JoWo_SeinThem_ShpQn_P.sig",
		Link:    "https://drive.google.com/file/d/1wJNLylzC6twJujxImriBGD5cRSE4ogW0/view",
		Creator: "SheepQueen"}
}

func TestValidate(t *testing.T) {
	ok := archiveSong()
	if err := ok.Validate(); err != nil {
		t.Fatalf("good archive entry: %v", err)
	}
	folder := Song{X: "Bey_DrunkInLove_DSR", Kind: KindFolder, Link: "https://drive.google.com/drive/folders/abcdefghijkl",
		Files: []File{{ID: "abcdefghijkl1", Name: "Bey_DrunkInLove_DSR_P.pak", Size: 10},
			{ID: "abcdefghijkl2", Name: "bey_drunkinlove_dsr_P.sig", Size: 10}}}
	if err := folder.Validate(); err != nil {
		t.Fatalf("good folder entry: %v", err)
	}
	for name, mutate := range map[string]func(*Song){
		"bad shortname":  func(s *Song) { s.X = "../x" },
		"harmonix":       func(s *Song) { s.Creator = "Harmonix/FurryLucasD10" },
		"no link":        func(s *Song) { s.Link = "Base game" },
		"two files":      func(s *Song) { s.Files = append(s.Files, s.Files[0]) },
		"bad archive":    func(s *Song) { s.Archive = "7z" },
		"other pak":      func(s *Song) { s.PakEntry = "Other_P.pak" },
		"dotdot entry":   func(s *Song) { s.PakEntry = "../JoWo_SeinThem_ShpQn_P.pak" },
		"absolute entry": func(s *Song) { s.SigEntry = "/JoWo_SeinThem_ShpQn_P.sig" },
		"backslash":      func(s *Song) { s.SigEntry = `a\JoWo_SeinThem_ShpQn_P.sig` },
		"too large":      func(s *Song) { s.Files[0].Size = MaxFileBytes + 1 },
		"bad id":         func(s *Song) { s.Files[0].ID = "short" },
		"bad kind":       func(s *Song) { s.Kind = "mega" },
	} {
		s := archiveSong()
		mutate(&s)
		err := s.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if name == "harmonix" && !errors.Is(err, ErrHarmonix) {
			t.Errorf("harmonix: %v", err)
		}
	}
	folder.Files[0], folder.Files[1] = folder.Files[1], folder.Files[0]
	if err := folder.Validate(); err == nil {
		t.Error("folder with the sig first: accepted")
	}
}

func TestShortNameOfPath(t *testing.T) {
	for in, want := range map[string]string{
		"/Game/DLC/Songs/Bey_DrunkInLove_DSR/Meta_Bey_DrunkInLove_DSR.Meta_Bey_DrunkInLove_DSR": "Bey_DrunkInLove_DSR",
		"/Game/DLC/Songs/X1/Meta_X1":               "X1",
		"/Game/Audio/Songs/X1/Meta_x1.Meta_x1:Sub": "X1",
		"/Game/DLC/Songs/X1/Meta_X2":               "",
		"/Game/DLC/Songs/X1/Sub/Meta_X1":           "",
		"/Game/Mods/Songs/X1/Meta_X1":              "",
		"Game/DLC/Songs/X1/Meta_X1":                "",
		"/Game/DLC/Songs/a.b/Meta_a.b":             "",
	} {
		if got := ShortNameOfPath(in); got != want {
			t.Errorf("ShortNameOfPath(%q) = %q, want %q", in, got, want)
		}
	}
	if u := DownloadURL("abc_DEF-123"); !strings.HasPrefix(u, "https://drive.usercontent.google.com/download?id=abc_DEF-123&") {
		t.Errorf("DownloadURL: %s", u)
	}
}
