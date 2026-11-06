package store

import (
	"bytes"
	"testing"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "anything", true},
		{"user:*", "user:42", true},
		{"user:*", "users", false},
		{"h?llo", "hello", true},
		{"h?llo", "hllo", false},
		{"h[ae]llo", "hallo", true},
		{"h[ae]llo", "hillo", false},
		{"h[^e]llo", "hallo", true},
		{"h[a-c]llo", "hbllo", true},
		{"a\\*b", "a*b", true},
		{"a\\*b", "axb", false},
		{"*/*", "a/b", true},
	}
	for _, tt := range tests {
		if got := Match(tt.pattern, tt.s); got != tt.want {
			t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
		}
	}
}

func TestExpiry(t *testing.T) {
	s := New(1)
	db := s.DBs[0]
	db.Set("k", NewString("v"), false)
	db.ExpireAt("k", s.Now()+1000)
	if ttl := db.TTL("k"); ttl <= 0 || ttl > 1000 {
		t.Fatalf("TTL = %d", ttl)
	}
	s.clock += 1001
	if db.Get("k") != nil {
		t.Fatal("key should have expired")
	}
	if db.TTL("k") != -2 {
		t.Fatal("expired key should report -2")
	}
}

func TestEmptyCollectionsAreRemoved(t *testing.T) {
	s := New(1)
	db := s.DBs[0]
	v, _ := db.GetOrCreate("l", KindList)
	v.RPush("a")
	db.Touched("l")
	v.LPop()
	db.Touched("l")
	if db.Exists("l") {
		t.Fatal("empty list should be deleted")
	}
	if s.UsedMemory() != 0 {
		t.Fatalf("used memory = %d after deleting everything", s.UsedMemory())
	}
}

func TestWrongType(t *testing.T) {
	db := New(1).DBs[0]
	db.Set("s", NewString("x"), false)
	if _, err := db.GetKind("s", KindHash); err != ErrWrongType {
		t.Fatalf("err = %v, want ErrWrongType", err)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	s := New(2)
	db := s.DBs[0]
	db.Set("str", NewString("hello"), false)
	db.ExpireAt("str", s.Now()+60_000)
	l, _ := db.GetOrCreate("list", KindList)
	l.LPush("b")
	l.LPush("a")
	l.RPush("c")
	db.Touched("list")
	h, _ := db.GetOrCreate("hash", KindHash)
	h.HSet("f", "v")
	db.Touched("hash")
	set, _ := s.DBs[1].GetOrCreate("set", KindSet)
	set.SAdd("x")
	s.DBs[1].Touched("set")
	z, _ := db.GetOrCreate("zset", KindZSet)
	z.ZAdd("m", 2.5)
	z.ZAdd("n", -1)
	db.Touched("zset")

	var buf bytes.Buffer
	if err := s.Snapshot(&buf); err != nil {
		t.Fatal(err)
	}
	r := New(2)
	if err := r.Restore(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	rdb := r.DBs[0]
	if v := rdb.Get("str"); v == nil || v.Str != "hello" || rdb.TTL("str") <= 0 {
		t.Errorf("string not restored with its expiry")
	}
	if got := rdb.Get("list").List.Items(); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("list = %v", got)
	}
	if rdb.Get("hash").Hash["f"] != "v" {
		t.Error("hash not restored")
	}
	if _, ok := r.DBs[1].Get("set").Set["x"]; !ok {
		t.Error("set in db 1 not restored")
	}
	if items := rdb.Get("zset").ZSet.Items(); len(items) != 2 || items[0].Member != "n" {
		t.Errorf("zset = %v", items)
	}
	if r.UsedMemory() != s.UsedMemory() {
		t.Errorf("used memory %d after restore, %d before", r.UsedMemory(), s.UsedMemory())
	}

	damaged := buf.Bytes()
	damaged[10] ^= 0xFF
	if err := New(2).Restore(damaged); err == nil {
		t.Error("a damaged snapshot should be rejected")
	}
}

func TestEviction(t *testing.T) {
	s := New(1)
	s.Policy = AllKeysLRU
	db := s.DBs[0]
	for i := 0; i < 100; i++ {
		db.Set(string(rune('a'+i%26))+string(rune(i)), NewString("0123456789"), false)
	}
	s.MaxMemory = s.UsedMemory() / 2
	if err := s.Reserve(); err != nil {
		t.Fatal(err)
	}
	if s.UsedMemory() > s.MaxMemory || s.Evicted == 0 {
		t.Fatalf("used %d of %d after evicting %d", s.UsedMemory(), s.MaxMemory, s.Evicted)
	}

	s.Policy = NoEviction
	s.MaxMemory = 1
	if err := s.Reserve(); err != ErrOOM {
		t.Fatalf("err = %v, want ErrOOM", err)
	}
}

func TestZSetOrder(t *testing.T) {
	v := NewZSet()
	v.ZAdd("b", 1)
	v.ZAdd("a", 1)
	v.ZAdd("c", 0)
	v.ZAdd("b", 5)
	var got []string
	for _, it := range v.ZSet.Items() {
		got = append(got, it.Member)
	}
	if want := "c a b"; join(got) != want {
		t.Errorf("order = %q, want %q", join(got), want)
	}
	if r := v.ZSet.Rank("b"); r != 2 {
		t.Errorf("rank = %d", r)
	}
}

func join(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += " "
		}
		out += x
	}
	return out
}
