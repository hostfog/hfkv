package server

import (
	"math"
	"strconv"
	"strings"

	"github.com/hostfog/hfkv/internal/store"
)

func init() {
	register("get", 2, 0, cmdGet)
	register("set", -3, fDenyOOM, cmdSet)
	register("setnx", 3, fDenyOOM, cmdSetNX)
	register("setex", 4, fDenyOOM, setexCmd(1000))
	register("psetex", 4, fDenyOOM, setexCmd(1))
	register("getset", 3, fDenyOOM, cmdGetSet)
	register("getdel", 2, 0, cmdGetDel)
	register("getex", -2, 0, cmdGetEx)
	register("mget", -2, 0, cmdMGet)
	register("mset", -3, fDenyOOM, cmdMSet)
	register("msetnx", -3, fDenyOOM, cmdMSet)
	register("append", 3, fDenyOOM, cmdAppend)
	register("strlen", 2, 0, cmdStrlen)
	register("getrange", 4, 0, cmdGetRange)
	register("substr", 4, 0, cmdGetRange)
	register("setrange", 4, fDenyOOM, cmdSetRange)
	register("incr", 2, fDenyOOM, func(x *ctx) { incrBy(x, 1) })
	register("decr", 2, fDenyOOM, func(x *ctx) { incrBy(x, -1) })
	register("incrby", 3, fDenyOOM, func(x *ctx) {
		if n, ok := x.int(2); ok {
			incrBy(x, n)
		}
	})
	register("decrby", 3, fDenyOOM, func(x *ctx) {
		if n, ok := x.int(2); ok {
			if n == math.MinInt64 {
				x.fail(errOverflow)
				return
			}
			incrBy(x, -n)
		}
	})
	register("incrbyfloat", 3, fDenyOOM, cmdIncrByFloat)
}

// getString returns the string at key; ok is false if a WRONGTYPE error
// was written.
func getString(x *ctx, key string) (s string, found, ok bool) {
	v, err := x.db.GetKind(key, store.KindString)
	if err != nil {
		x.fail(err)
		return "", false, false
	}
	if v == nil {
		return "", false, true
	}
	return v.Str, true, true
}

func cmdGet(x *ctx) {
	s, found, ok := getString(x, x.key())
	if !ok {
		return
	}
	if !found {
		x.w.Null()
		return
	}
	x.w.BulkString(s)
}

// SET key value [NX|XX] [GET] [EX s|PX ms|EXAT s|PXAT ms|KEEPTTL]
func cmdSet(x *ctx) {
	var (
		nx, xx, get, keepTTL bool
		at                   int64
	)
	for i := 3; i < len(x.args); i++ {
		switch {
		case x.is(i, "nx"):
			nx = true
		case x.is(i, "xx"):
			xx = true
		case x.is(i, "get"):
			get = true
		case x.is(i, "keepttl"):
			keepTTL = true
		case (x.is(i, "ex") || x.is(i, "px") || x.is(i, "exat") || x.is(i, "pxat")) && i+1 < len(x.args):
			if at != 0 {
				x.fail(errSyntax)
				return
			}
			n, ok := x.int(i + 1)
			if !ok {
				return
			}
			if n <= 0 {
				x.w.Error("invalid expire time in 'set' command")
				return
			}
			switch strings.ToLower(x.str(i)) {
			case "ex":
				at = x.s.store.Now() + n*1000
			case "px":
				at = x.s.store.Now() + n
			case "exat":
				at = n * 1000
			case "pxat":
				at = n
			}
			i++
		default:
			x.fail(errSyntax)
			return
		}
	}
	if (nx && xx) || (keepTTL && at != 0) {
		x.fail(errSyntax)
		return
	}

	key := x.key()
	var (
		old   string
		found bool
	)
	if get {
		var ok bool
		if old, found, ok = getString(x, key); !ok {
			return
		}
	}
	exists := x.db.Exists(key)
	if (nx && exists) || (xx && !exists) {
		if get {
			replyOptional(x, old, found)
		} else {
			x.w.Null()
		}
		return
	}
	x.db.Set(key, store.NewString(x.str(2)), keepTTL)
	if at != 0 {
		x.db.ExpireAt(key, at)
	}
	if get {
		replyOptional(x, old, found)
	} else {
		x.w.OK()
	}
}

func replyOptional(x *ctx, s string, found bool) {
	if found {
		x.w.BulkString(s)
	} else {
		x.w.Null()
	}
}

func cmdSetNX(x *ctx) {
	if x.db.Exists(x.key()) {
		x.w.Int(0)
		return
	}
	x.db.Set(x.key(), store.NewString(x.str(2)), false)
	x.w.Int(1)
}

func setexCmd(unit int64) func(*ctx) {
	return func(x *ctx) {
		n, ok := x.int(2)
		if !ok {
			return
		}
		if n <= 0 {
			x.w.Error("invalid expire time in '" + strings.ToLower(x.str(0)) + "' command")
			return
		}
		x.db.Set(x.key(), store.NewString(x.str(3)), false)
		x.db.ExpireAt(x.key(), x.s.store.Now()+n*unit)
		x.w.OK()
	}
}

func cmdGetSet(x *ctx) {
	old, found, ok := getString(x, x.key())
	if !ok {
		return
	}
	x.db.Set(x.key(), store.NewString(x.str(2)), false)
	replyOptional(x, old, found)
}

func cmdGetDel(x *ctx) {
	s, found, ok := getString(x, x.key())
	if !ok {
		return
	}
	if found {
		x.db.Delete(x.key())
	}
	replyOptional(x, s, found)
}

// GETEX key [EX s|PX ms|EXAT s|PXAT ms|PERSIST]
func cmdGetEx(x *ctx) {
	s, found, ok := getString(x, x.key())
	if !ok {
		return
	}
	var at int64
	persist := false
	if len(x.args) > 2 {
		switch {
		case len(x.args) == 3 && x.is(2, "persist"):
			persist = true
		case len(x.args) == 4:
			n, good := x.int(3)
			if !good {
				return
			}
			switch strings.ToLower(x.str(2)) {
			case "ex":
				at = x.s.store.Now() + n*1000
			case "px":
				at = x.s.store.Now() + n
			case "exat":
				at = n * 1000
			case "pxat":
				at = n
			default:
				x.fail(errSyntax)
				return
			}
		default:
			x.fail(errSyntax)
			return
		}
	}
	if found {
		if persist {
			x.db.Persist(x.key())
		} else if at != 0 {
			x.db.ExpireAt(x.key(), at)
		}
	}
	replyOptional(x, s, found)
}

func cmdMGet(x *ctx) {
	x.w.Array(len(x.args) - 1)
	for _, k := range x.args[1:] {
		v := x.db.Get(string(k))
		if v == nil || v.Kind != store.KindString {
			x.w.Null()
			continue
		}
		x.w.BulkString(v.Str)
	}
}

func cmdMSet(x *ctx) {
	if len(x.args)%2 == 0 {
		x.w.Error("wrong number of arguments for '" + strings.ToLower(x.str(0)) + "' command")
		return
	}
	nx := x.is(0, "msetnx")
	if nx {
		for i := 1; i < len(x.args); i += 2 {
			if x.db.Exists(x.str(i)) {
				x.w.Int(0)
				return
			}
		}
	}
	for i := 1; i < len(x.args); i += 2 {
		x.db.Set(x.str(i), store.NewString(x.str(i+1)), false)
	}
	if nx {
		x.w.Int(1)
	} else {
		x.w.OK()
	}
}

func cmdAppend(x *ctx) {
	v, err := x.db.GetKind(x.key(), store.KindString)
	if err != nil {
		x.fail(err)
		return
	}
	if v == nil {
		x.db.Set(x.key(), store.NewString(x.str(2)), false)
		x.w.Int(int64(len(x.args[2])))
		return
	}
	v.SetStr(v.Str + x.str(2))
	x.db.Touched(x.key())
	x.w.Int(int64(len(v.Str)))
}

func cmdStrlen(x *ctx) {
	s, _, ok := getString(x, x.key())
	if ok {
		x.w.Int(int64(len(s)))
	}
}

func cmdGetRange(x *ctx) {
	start, ok1 := x.int(2)
	if !ok1 {
		return
	}
	stop, ok2 := x.int(3)
	if !ok2 {
		return
	}
	s, _, ok := getString(x, x.key())
	if !ok {
		return
	}
	lo, hi := span(start, stop, len(s))
	x.w.BulkString(s[lo:hi])
}

func cmdSetRange(x *ctx) {
	off, ok := x.int(2)
	if !ok {
		return
	}
	if off < 0 || off+int64(len(x.args[3])) > 512<<20 {
		x.w.Error("offset is out of range")
		return
	}
	s, found, ok := getString(x, x.key())
	if !ok {
		return
	}
	patch := x.str(3)
	if len(patch) == 0 {
		x.w.Int(int64(len(s)))
		return
	}
	b := []byte(s)
	if need := int(off) + len(patch); need > len(b) {
		b = append(b, make([]byte, need-len(b))...)
	}
	copy(b[off:], patch)
	if found {
		v := x.db.Get(x.key())
		v.SetStr(string(b))
		x.db.Touched(x.key())
	} else {
		x.db.Set(x.key(), store.NewString(string(b)), false)
	}
	x.w.Int(int64(len(b)))
}

func incrBy(x *ctx, by int64) {
	s, found, ok := getString(x, x.key())
	if !ok {
		return
	}
	var n int64
	if found {
		var err error
		if n, err = strconv.ParseInt(s, 10, 64); err != nil {
			x.fail(errInt)
			return
		}
	}
	if (by > 0 && n > math.MaxInt64-by) || (by < 0 && n < math.MinInt64-by) {
		x.fail(errOverflow)
		return
	}
	n += by
	setNumber(x, strconv.FormatInt(n, 10), found)
	x.w.Int(n)
}

func cmdIncrByFloat(x *ctx) {
	by, ok := x.float(2)
	if !ok {
		return
	}
	s, found, ok := getString(x, x.key())
	if !ok {
		return
	}
	var f float64
	if found {
		var good bool
		if f, good = parseFloat(s); !good {
			x.fail(errFloat)
			return
		}
	}
	f += by
	if math.IsInf(f, 0) || math.IsNaN(f) {
		x.w.Error("increment would produce NaN or Infinity")
		return
	}
	out := formatFloat(f)
	setNumber(x, out, found)
	x.w.BulkString(out)
}

// setNumber stores a new numeric string, keeping the key's expiry.
func setNumber(x *ctx, s string, found bool) {
	if found {
		v := x.db.Get(x.key())
		v.SetStr(s)
		x.db.Touched(x.key())
		return
	}
	x.db.Set(x.key(), store.NewString(s), false)
}
