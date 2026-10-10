package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestBlocks(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "blocks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	var ids []int64
	for _, u := range []string{"u1", "u2", "u3"} {
		p, err := s.LoginPlayer(ctx, "epic", u, u)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
	}
	a, b, c := ids[0], ids[1], ids[2]
	if added, _ := s.Block(ctx, a, b); !added {
		t.Fatal("first block not added")
	}
	if added, _ := s.Block(ctx, a, b); added {
		t.Fatal("repeated block added again")
	}
	if _, err := s.Block(ctx, c, a); err != nil {
		t.Fatal(err)
	}
	if isBlocked, blockedYou, _ := s.BlockRelation(ctx, a, b); !isBlocked || blockedYou {
		t.Fatalf("a->b relation %v %v", isBlocked, blockedYou)
	}
	if isBlocked, blockedYou, _ := s.BlockRelation(ctx, b, a); isBlocked || !blockedYou {
		t.Fatalf("b->a relation %v %v", isBlocked, blockedYou)
	}
	if set, _ := s.BlockedEitherWay(ctx, a); len(set) != 2 || !set[b] || !set[c] {
		t.Fatalf("a's either-way set %v", set)
	}
	if list, total, _ := s.BlockedPlayers(ctx, a, 0, 10); total != 1 || len(list) != 1 || list[0].ID != b {
		t.Fatalf("a's list %d %+v", total, list)
	}
	if removed, _ := s.Unblock(ctx, a, b); !removed {
		t.Fatal("unblock")
	}
	if removed, _ := s.Unblock(ctx, a, b); removed {
		t.Fatal("second unblock removed something")
	}
	if ok, _ := s.HasBlocked(ctx, c, a); !ok {
		t.Fatal("HasBlocked")
	}
}
