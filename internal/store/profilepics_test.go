package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

func TestProfilePics(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "pics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	p, err := s.LoginPlayer(ctx, "epic", "user1", "Player One")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProfilePic(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("picture before any upload: %v", err)
	}
	add := func(pic int32, size string) int32 {
		t.Helper()
		replaced, err := s.AddProfilePicSize(ctx, p.ID, pic, size)
		if err != nil {
			t.Fatal(err)
		}
		return replaced
	}
	if add(3, "large") != 0 || add(3, "small") != 0 || add(3, "small") != 0 {
		t.Fatal("first picture replaced something")
	}
	if pic, _ := s.ProfilePic(ctx, p.ID); pic.PicID != 3 || !slices.Equal(pic.Sizes, []string{"large", "small"}) {
		t.Fatalf("picture %+v", pic)
	}
	if add(5, "medium") != 3 {
		t.Fatal("a newer picture did not report the one it replaced")
	}
	if _, err := s.AddProfilePicSize(ctx, p.ID, 4, "large"); !errors.Is(err, ErrStalePicture) {
		t.Fatalf("older picture: %v", err)
	}
	if max, _ := s.MaxProfilePicID(ctx); max != 5 {
		t.Fatalf("max id %d", max)
	}
	if id, err := s.RemoveProfilePic(ctx, p.ID); err != nil || id != 5 {
		t.Fatalf("remove: %d %v", id, err)
	}
	if _, err := s.RemoveProfilePic(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
}
