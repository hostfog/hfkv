package server

import (
	"sort"
	"strconv"

	"github.com/hostfog/hfkv/internal/store"
)

func init() {
	register("del", -2, 0, cmdDel)
	register("unlink", -2, 0, cmdDel)
	register("exists", -2, 0, cmdExists)
	register("type", 2, 0, cmdType)
	register("keys", 2, 0, cmdKeys)
	register("scan", -2, 0, cmdScan)
	register("randomkey", 1, 0, cmdRandomKey)
	register("rename", 3, 0, cmdRename)
	register("renamenx", 3, 0, cmdRename)
	register("move", 3, 0, cmdMove)
	register("copy", -3, fDenyOOM, cmdCopy)
	register("expire", -3, 0, expireCmd(1000, false))
	register("pexpire", -3, 0, expireCmd(1, false))
	register("expireat", -3, 0, expireCmd(1000, true))
	register("pexpireat", -3, 0, expireCmd(1, true))
	register("ttl", 2, 0, ttlCmd(1000))
	register("pttl", 2, 0, ttlCmd(1))
	register("expiretime", 2, 0, expireTimeCmd(1000))
	register("pexpiretime", 2, 0, expireTimeCmd(1))
	register("persist", 2, 0, cmdPersist)
	register("touch", -2, 0, cmdExists)
	register("dbsize", 1, 0, func(x *ctx) { x.w.Int(int64(x.db.Len())) })
}

func cmdDel(x *ctx) {
	n := 0
	for _, k := range x.args[1:] {
		if x.db.Delete(string(k)) {
			n++
		}
	}
	x.w.Int(int64(n))
}

func cmdExists(x *ctx) {
	n := 0
	for _, k := range x.args[1:] {
		if x.db.Exists(string(k)) {
			n++
		}
	}
	x.w.Int(int64(n))
}

func cmdType(x *ctx) {
	v := x.db.Get(x.key())
	if v == nil {
		x.w.Simple("none")
		return
	}
	x.w.Simple(v.Kind.String())
}

func cmdKeys(x *ctx) {
	pattern := x.str(1)
	var keys []string
	x.db.Each(func(k string, _ *store.Value) bool {
		if store.Match(pattern, k) {
			keys = append(keys, k)
		}
		return true
	})
	x.w.Strings(keys)
}

// cmdScan walks the keys in sorted order and uses the last key's position
// as the cursor. Simple, and it never misses a key that exists for the
// whole scan.
func cmdScan(x *ctx) {
	cursor, err := strconv.ParseUint(x.str(1), 10, 64)
	if err != nil {
		x.w.Error("invalid cursor")
		return
	}
	pattern, count, kind, ok := scanOptions(x, 2)
	if !ok {
		return
	}
	var keys []string
	x.db.Each(func(k string, v *store.Value) bool {
		if (pattern == "" || store.Match(pattern, k)) && (kind == "" || v.Kind.String() == kind) {
			keys = append(keys, k)
		}
		return true
	})
	sort.Strings(keys)
	page, next := scanPage(keys, cursor, count)
	x.w.Array(2)
	x.w.BulkString(strconv.FormatUint(next, 10))
	x.w.Strings(page)
}

func scanOptions(x *ctx, from int) (pattern string, count int, kind string, ok bool) {
	count = 10
	for i := from; i < len(x.args); i += 2 {
		if i+1 >= len(x.args) {
			x.fail(errSyntax)
			return
		}
		switch {
		case x.is(i, "match"):
			pattern = x.str(i + 1)
		case x.is(i, "count"):
			n, good := x.int(i + 1)
			if !good {
				return
			}
			if n < 1 {
				x.fail(errSyntax)
				return
			}
			count = int(n)
		case x.is(i, "type"):
			kind = x.str(i + 1)
		default:
			x.fail(errSyntax)
			return
		}
	}
	return pattern, count, kind, true
}

func scanPage(items []string, cursor uint64, count int) ([]string, uint64) {
	if cursor >= uint64(len(items)) {
		return nil, 0
	}
	end := cursor + uint64(count)
	if end >= uint64(len(items)) {
		return items[cursor:], 0
	}
	return items[cursor:end], end
}

func cmdRandomKey(x *ctx) {
	if k := x.db.RandomKey(); k != "" {
		x.w.BulkString(k)
		return
	}
	x.w.Null()
}

func cmdRename(x *ctx) {
	from, to := x.key(), x.str(2)
	nx := x.is(0, "renamenx")
	if !x.db.Exists(from) {
		x.fail(errNoKey)
		return
	}
	if nx && x.db.Exists(to) {
		x.w.Int(0)
		return
	}
	if from != to {
		x.db.Rename(from, to)
	}
	if nx {
		x.w.Int(1)
	} else {
		x.w.OK()
	}
}

func cmdMove(x *ctx) {
	n, ok := x.int(2)
	if !ok {
		return
	}
	if n < 0 || n >= int64(len(x.s.store.DBs)) {
		x.w.Error("DB index is out of range")
		return
	}
	dst := x.s.store.DBs[n]
	if dst == x.db {
		x.w.Error("source and destination objects are the same")
		return
	}
	x.w.Int(b2i(x.db.Move(x.key(), dst)))
}

func cmdCopy(x *ctx) {
	src, dstKey := x.key(), x.str(2)
	dst, replace := x.db, false
	for i := 3; i < len(x.args); i++ {
		switch {
		case x.is(i, "replace"):
			replace = true
		case x.is(i, "db") && i+1 < len(x.args):
			n, ok := x.int(i + 1)
			if !ok {
				return
			}
			if n < 0 || n >= int64(len(x.s.store.DBs)) {
				x.w.Error("DB index is out of range")
				return
			}
			dst = x.s.store.DBs[n]
			i++
		default:
			x.fail(errSyntax)
			return
		}
	}
	v := x.db.Get(src)
	if v == nil || (!replace && dst.Exists(dstKey)) {
		x.w.Int(0)
		return
	}
	at := x.db.ExpiresAt(src)
	dst.Set(dstKey, cloneValue(v), false)
	if at > 0 {
		dst.ExpireAt(dstKey, at)
	}
	x.w.Int(1)
}

func cloneValue(v *store.Value) *store.Value {
	if v.Kind == store.KindString {
		return store.NewString(v.Str)
	}
	var out *store.Value
	switch v.Kind {
	case store.KindList:
		out = store.NewList()
		out.List.Replace(v.List.Items())
	case store.KindHash:
		out = store.NewHash()
		for f, s := range v.Hash {
			out.HSet(f, s)
		}
	case store.KindSet:
		out = store.NewSet()
		for m := range v.Set {
			out.SAdd(m)
		}
	case store.KindZSet:
		out = store.NewZSet()
		for _, it := range v.ZSet.Items() {
			out.ZAdd(it.Member, it.Score)
		}
	}
	out.Resize()
	return out
}

// expireCmd builds EXPIRE, PEXPIRE, EXPIREAT and PEXPIREAT. unit is the
// number of ms in one unit of the argument.
func expireCmd(unit int64, absolute bool) func(*ctx) {
	return func(x *ctx) {
		n, ok := x.int(2)
		if !ok {
			return
		}
		at := n * unit
		if !absolute {
			at += x.s.store.Now()
		}
		cur := x.db.TTL(x.key())
		if cur == -2 {
			x.w.Int(0)
			return
		}
		curAt := x.db.ExpiresAt(x.key())
		for i := 3; i < len(x.args); i++ {
			var allowed bool
			switch {
			case x.is(i, "nx"):
				allowed = curAt == 0
			case x.is(i, "xx"):
				allowed = curAt != 0
			case x.is(i, "gt"):
				allowed = curAt != 0 && at > curAt
			case x.is(i, "lt"):
				allowed = curAt == 0 || at < curAt
			default:
				x.w.Error("Unsupported option " + x.str(i))
				return
			}
			if !allowed {
				x.w.Int(0)
				return
			}
		}
		x.w.Int(b2i(x.db.ExpireAt(x.key(), at)))
	}
}

func ttlCmd(unit int64) func(*ctx) {
	return func(x *ctx) {
		t := x.db.TTL(x.key())
		if t < 0 {
			x.w.Int(t)
			return
		}
		x.w.Int((t + unit - 1) / unit)
	}
}

func expireTimeCmd(unit int64) func(*ctx) {
	return func(x *ctx) {
		switch t := x.db.TTL(x.key()); t {
		case -2, -1:
			x.w.Int(t)
		default:
			x.w.Int(x.db.ExpiresAt(x.key()) / unit)
		}
	}
}

func cmdPersist(x *ctx) { x.w.Int(b2i(x.db.Persist(x.key()))) }

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
