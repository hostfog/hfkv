package server

import (
	"math/rand/v2"
	"sort"
	"strconv"

	"github.com/hostfog/hfkv/internal/store"
)

func init() {
	register("sadd", -3, fDenyOOM, cmdSAdd)
	register("srem", -3, 0, cmdSRem)
	register("smembers", 2, 0, cmdSMembers)
	register("sismember", 3, 0, cmdSIsMember)
	register("smismember", -3, 0, cmdSMIsMember)
	register("scard", 2, 0, cmdSCard)
	register("spop", -2, 0, cmdSPop)
	register("srandmember", -2, 0, cmdSRandMember)
	register("smove", 4, 0, cmdSMove)
	register("sinter", -2, 0, setOpCmd(opInter, false))
	register("sunion", -2, 0, setOpCmd(opUnion, false))
	register("sdiff", -2, 0, setOpCmd(opDiff, false))
	register("sinterstore", -3, fDenyOOM, setOpCmd(opInter, true))
	register("sunionstore", -3, fDenyOOM, setOpCmd(opUnion, true))
	register("sdiffstore", -3, fDenyOOM, setOpCmd(opDiff, true))
	register("sintercard", -3, 0, cmdSInterCard)
	register("sscan", -3, 0, cmdSScan)
}

func getSet(x *ctx, key string) (*store.Value, bool) {
	v, err := x.db.GetKind(key, store.KindSet)
	if err != nil {
		x.fail(err)
		return nil, false
	}
	return v, true
}

func cmdSAdd(x *ctx) {
	v, err := x.db.GetOrCreate(x.key(), store.KindSet)
	if err != nil {
		x.fail(err)
		return
	}
	n := 0
	for _, m := range x.args[2:] {
		if v.SAdd(string(m)) {
			n++
		}
	}
	x.db.Touched(x.key())
	x.w.Int(int64(n))
}

func cmdSRem(x *ctx) {
	v, ok := getSet(x, x.key())
	if !ok {
		return
	}
	n := 0
	if v != nil {
		for _, m := range x.args[2:] {
			if v.SRem(string(m)) {
				n++
			}
		}
		x.db.Touched(x.key())
	}
	x.w.Int(int64(n))
}

func members(v *store.Value) []string {
	if v == nil {
		return nil
	}
	out := make([]string, 0, len(v.Set))
	for m := range v.Set {
		out = append(out, m)
	}
	return out
}

func cmdSMembers(x *ctx) {
	if v, ok := getSet(x, x.key()); ok {
		writeSet(x, members(v))
	}
}

func writeSet(x *ctx, items []string) {
	x.w.Set(len(items))
	for _, s := range items {
		x.w.BulkString(s)
	}
}

func cmdSIsMember(x *ctx) {
	if v, ok := getSet(x, x.key()); ok {
		found := false
		if v != nil {
			_, found = v.Set[x.str(2)]
		}
		x.w.Int(b2i(found))
	}
}

func cmdSMIsMember(x *ctx) {
	v, ok := getSet(x, x.key())
	if !ok {
		return
	}
	x.w.Array(len(x.args) - 2)
	for _, m := range x.args[2:] {
		found := false
		if v != nil {
			_, found = v.Set[string(m)]
		}
		x.w.Int(b2i(found))
	}
}

func cmdSCard(x *ctx) {
	if v, ok := getSet(x, x.key()); ok {
		x.w.Int(int64(len(members(v))))
	}
}

func cmdSPop(x *ctx) {
	v, ok := getSet(x, x.key())
	if !ok {
		return
	}
	if len(x.args) == 2 {
		if v == nil {
			x.w.Null()
			return
		}
		for m := range v.Set {
			v.SRem(m)
			x.db.Touched(x.key())
			x.w.BulkString(m)
			return
		}
	}
	count, ok := x.int(2)
	if !ok {
		return
	}
	if count < 0 {
		x.w.Error("value is out of range, must be positive")
		return
	}
	picked := randomPick(members(v), count)
	for _, m := range picked {
		v.SRem(m)
	}
	if v != nil {
		x.db.Touched(x.key())
	}
	x.w.Strings(picked)
}

func cmdSRandMember(x *ctx) {
	v, ok := getSet(x, x.key())
	if !ok {
		return
	}
	if len(x.args) == 2 {
		for m := range v.Set {
			x.w.BulkString(m)
			return
		}
		x.w.Null()
		return
	}
	count, ok := x.int(2)
	if !ok {
		return
	}
	x.w.Strings(randomPick(members(v), count))
}

// randomPick returns count distinct items, or with a negative count, -count
// items that may repeat (the SRANDMEMBER rules).
func randomPick(items []string, count int64) []string {
	if len(items) == 0 || count == 0 {
		return []string{}
	}
	if count < 0 {
		out := make([]string, -count)
		for i := range out {
			out[i] = items[rand.IntN(len(items))]
		}
		return out
	}
	rand.Shuffle(len(items), func(i, j int) { items[i], items[j] = items[j], items[i] })
	if count < int64(len(items)) {
		items = items[:count]
	}
	return items
}

func cmdSMove(x *ctx) {
	src, ok := getSet(x, x.key())
	if !ok {
		return
	}
	if _, ok := getSet(x, x.str(2)); !ok {
		return
	}
	m := x.str(3)
	if src == nil || !src.SRem(m) {
		x.w.Int(0)
		return
	}
	x.db.Touched(x.key())
	dst, _ := x.db.GetOrCreate(x.str(2), store.KindSet)
	dst.SAdd(m)
	x.db.Touched(x.str(2))
	x.w.Int(1)
}

type setOp int

const (
	opInter setOp = iota
	opUnion
	opDiff
)

// combine applies op to the sets at keys; ok is false if an error was written.
func combine(x *ctx, op setOp, keys [][]byte) (map[string]struct{}, bool) {
	var out map[string]struct{}
	for i, k := range keys {
		v, ok := getSet(x, string(k))
		if !ok {
			return nil, false
		}
		var set map[string]struct{}
		if v != nil {
			set = v.Set
		}
		if i == 0 {
			out = make(map[string]struct{}, len(set))
			for m := range set {
				out[m] = struct{}{}
			}
			continue
		}
		switch op {
		case opInter:
			for m := range out {
				if _, ok := set[m]; !ok {
					delete(out, m)
				}
			}
		case opUnion:
			for m := range set {
				out[m] = struct{}{}
			}
		case opDiff:
			for m := range set {
				delete(out, m)
			}
		}
	}
	return out, true
}

func setOpCmd(op setOp, storeResult bool) func(*ctx) {
	return func(x *ctx) {
		keys := x.args[1:]
		if storeResult {
			keys = x.args[2:]
		}
		result, ok := combine(x, op, keys)
		if !ok {
			return
		}
		if !storeResult {
			out := make([]string, 0, len(result))
			for m := range result {
				out = append(out, m)
			}
			writeSet(x, out)
			return
		}
		dst := x.key()
		x.db.Delete(dst)
		if len(result) > 0 {
			v := store.NewSet()
			for m := range result {
				v.SAdd(m)
			}
			x.db.Set(dst, v, false)
		}
		x.w.Int(int64(len(result)))
	}
}

// SINTERCARD numkeys key [key ...] [LIMIT limit]
func cmdSInterCard(x *ctx) {
	n, ok := x.int(1)
	if !ok {
		return
	}
	if n <= 0 || n > int64(len(x.args)-2) {
		x.w.Error("numkeys should be greater than 0")
		return
	}
	var limit int64
	rest := x.args[2+n:]
	if len(rest) == 2 && x.is(int(2+n), "limit") {
		if limit, ok = x.int(int(3 + n)); !ok {
			return
		}
	} else if len(rest) != 0 {
		x.fail(errSyntax)
		return
	}
	result, ok := combine(x, opInter, x.args[2:2+n])
	if !ok {
		return
	}
	count := int64(len(result))
	if limit > 0 && count > limit {
		count = limit
	}
	x.w.Int(count)
}

func cmdSScan(x *ctx) {
	cursor, err := strconv.ParseUint(x.str(2), 10, 64)
	if err != nil {
		x.w.Error("invalid cursor")
		return
	}
	pattern, count, _, ok := scanOptions(x, 3)
	if !ok {
		return
	}
	v, ok := getSet(x, x.key())
	if !ok {
		return
	}
	var all []string
	for _, m := range members(v) {
		if pattern == "" || store.Match(pattern, m) {
			all = append(all, m)
		}
	}
	sort.Strings(all)
	page, next := scanPage(all, cursor, count)
	x.w.Array(2)
	x.w.BulkString(strconv.FormatUint(next, 10))
	x.w.Strings(page)
}
