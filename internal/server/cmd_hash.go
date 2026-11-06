package server

import (
	"math"
	"sort"
	"strconv"

	"github.com/hostfog/hfkv/internal/store"
)

func init() {
	register("hset", -4, fDenyOOM, cmdHSet)
	register("hmset", -4, fDenyOOM, cmdHSet)
	register("hsetnx", 4, fDenyOOM, cmdHSetNX)
	register("hget", 3, 0, cmdHGet)
	register("hmget", -3, 0, cmdHMGet)
	register("hdel", -3, 0, cmdHDel)
	register("hlen", 2, 0, cmdHLen)
	register("hstrlen", 3, 0, cmdHStrlen)
	register("hexists", 3, 0, cmdHExists)
	register("hgetall", 2, 0, hashListCmd(true, true))
	register("hkeys", 2, 0, hashListCmd(true, false))
	register("hvals", 2, 0, hashListCmd(false, true))
	register("hincrby", 4, fDenyOOM, cmdHIncrBy)
	register("hincrbyfloat", 4, fDenyOOM, cmdHIncrByFloat)
	register("hscan", -3, 0, cmdHScan)
	register("hrandfield", -2, 0, cmdHRandField)
}

func getHash(x *ctx) (*store.Value, bool) {
	v, err := x.db.GetKind(x.key(), store.KindHash)
	if err != nil {
		x.fail(err)
		return nil, false
	}
	return v, true
}

func cmdHSet(x *ctx) {
	if len(x.args)%2 != 0 {
		x.w.Error("wrong number of arguments for '" + string(x.args[0]) + "' command")
		return
	}
	v, err := x.db.GetOrCreate(x.key(), store.KindHash)
	if err != nil {
		x.fail(err)
		return
	}
	added := 0
	for i := 2; i < len(x.args); i += 2 {
		if v.HSet(x.str(i), x.str(i+1)) {
			added++
		}
	}
	x.db.Touched(x.key())
	if x.is(0, "hmset") {
		x.w.OK()
		return
	}
	x.w.Int(int64(added))
}

func cmdHSetNX(x *ctx) {
	v, err := x.db.GetOrCreate(x.key(), store.KindHash)
	if err != nil {
		x.fail(err)
		return
	}
	added := false
	if _, ok := v.Hash[x.str(2)]; !ok {
		added = v.HSet(x.str(2), x.str(3))
	}
	x.db.Touched(x.key())
	x.w.Int(b2i(added))
}

func cmdHGet(x *ctx) {
	v, ok := getHash(x)
	if !ok {
		return
	}
	if v == nil {
		x.w.Null()
		return
	}
	s, found := v.Hash[x.str(2)]
	replyOptional(x, s, found)
}

func cmdHMGet(x *ctx) {
	v, ok := getHash(x)
	if !ok {
		return
	}
	x.w.Array(len(x.args) - 2)
	for _, f := range x.args[2:] {
		if v == nil {
			x.w.Null()
			continue
		}
		s, found := v.Hash[string(f)]
		replyOptional(x, s, found)
	}
}

func cmdHDel(x *ctx) {
	v, ok := getHash(x)
	if !ok {
		return
	}
	n := 0
	if v != nil {
		for _, f := range x.args[2:] {
			if v.HDel(string(f)) {
				n++
			}
		}
		x.db.Touched(x.key())
	}
	x.w.Int(int64(n))
}

func cmdHLen(x *ctx) {
	if v, ok := getHash(x); ok {
		if v == nil {
			x.w.Int(0)
			return
		}
		x.w.Int(int64(len(v.Hash)))
	}
}

func cmdHStrlen(x *ctx) {
	if v, ok := getHash(x); ok {
		if v == nil {
			x.w.Int(0)
			return
		}
		x.w.Int(int64(len(v.Hash[x.str(2)])))
	}
}

func cmdHExists(x *ctx) {
	if v, ok := getHash(x); ok {
		found := false
		if v != nil {
			_, found = v.Hash[x.str(2)]
		}
		x.w.Int(b2i(found))
	}
}

func hashListCmd(keys, vals bool) func(*ctx) {
	return func(x *ctx) {
		v, ok := getHash(x)
		if !ok {
			return
		}
		if v == nil {
			if keys && vals {
				x.w.Map(0)
			} else {
				x.w.Array(0)
			}
			return
		}
		if keys && vals {
			x.w.Map(len(v.Hash))
		} else {
			x.w.Array(len(v.Hash))
		}
		for f, s := range v.Hash {
			if keys {
				x.w.BulkString(f)
			}
			if vals {
				x.w.BulkString(s)
			}
		}
	}
}

func cmdHIncrBy(x *ctx) {
	by, ok := x.int(3)
	if !ok {
		return
	}
	v, err := x.db.GetOrCreate(x.key(), store.KindHash)
	if err != nil {
		x.fail(err)
		return
	}
	defer x.db.Touched(x.key())
	var n int64
	if s, found := v.Hash[x.str(2)]; found {
		if n, err = strconv.ParseInt(s, 10, 64); err != nil {
			x.w.Error("hash value is not an integer")
			return
		}
	}
	if (by > 0 && n > math.MaxInt64-by) || (by < 0 && n < math.MinInt64-by) {
		x.fail(errOverflow)
		return
	}
	n += by
	v.HSet(x.str(2), strconv.FormatInt(n, 10))
	x.w.Int(n)
}

func cmdHIncrByFloat(x *ctx) {
	by, ok := x.float(3)
	if !ok {
		return
	}
	v, err := x.db.GetOrCreate(x.key(), store.KindHash)
	if err != nil {
		x.fail(err)
		return
	}
	defer x.db.Touched(x.key())
	var f float64
	if s, found := v.Hash[x.str(2)]; found {
		if f, ok = parseFloat(s); !ok {
			x.w.Error("hash value is not a float")
			return
		}
	}
	f += by
	if math.IsInf(f, 0) || math.IsNaN(f) {
		x.w.Error("increment would produce NaN or Infinity")
		return
	}
	out := formatFloat(f)
	v.HSet(x.str(2), out)
	x.w.BulkString(out)
}

func cmdHScan(x *ctx) {
	cursor, err := strconv.ParseUint(x.str(2), 10, 64)
	if err != nil {
		x.w.Error("invalid cursor")
		return
	}
	pattern, count, _, ok := scanOptions(x, 3)
	if !ok {
		return
	}
	v, ok := getHash(x)
	if !ok {
		return
	}
	var fields []string
	if v != nil {
		for f := range v.Hash {
			if pattern == "" || store.Match(pattern, f) {
				fields = append(fields, f)
			}
		}
	}
	sort.Strings(fields)
	page, next := scanPage(fields, cursor, count)
	x.w.Array(2)
	x.w.BulkString(strconv.FormatUint(next, 10))
	x.w.Array(len(page) * 2)
	for _, f := range page {
		x.w.BulkString(f)
		x.w.BulkString(v.Hash[f])
	}
}

// HRANDFIELD key [count [WITHVALUES]]
func cmdHRandField(x *ctx) {
	v, ok := getHash(x)
	if !ok {
		return
	}
	if len(x.args) == 2 {
		for f := range v.Hash {
			x.w.BulkString(f)
			return
		}
		x.w.Null()
		return
	}
	count, ok := x.int(2)
	if !ok {
		return
	}
	withValues := len(x.args) == 4 && x.is(3, "withvalues")
	if len(x.args) > 4 || (len(x.args) == 4 && !withValues) {
		x.fail(errSyntax)
		return
	}
	var fields []string
	if v != nil {
		for f := range v.Hash {
			fields = append(fields, f)
		}
	}
	picked := randomPick(fields, count)
	if withValues {
		pairs(x.w, len(picked))
		for _, f := range picked {
			pair(x.w)
			x.w.BulkString(f)
			x.w.BulkString(v.Hash[f])
		}
		return
	}
	x.w.Strings(picked)
}
