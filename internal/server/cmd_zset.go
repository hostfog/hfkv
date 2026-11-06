package server

import (
	"math"
	"strconv"
	"strings"

	"github.com/hostfog/hfkv/internal/resp"
	"github.com/hostfog/hfkv/internal/store"
)

func init() {
	register("zadd", -4, fDenyOOM, cmdZAdd)
	register("zincrby", 4, fDenyOOM, cmdZIncrBy)
	register("zrem", -3, 0, cmdZRem)
	register("zcard", 2, 0, cmdZCard)
	register("zscore", 3, 0, cmdZScore)
	register("zmscore", -3, 0, cmdZMScore)
	register("zrank", -3, 0, rankCmd(false))
	register("zrevrank", -3, 0, rankCmd(true))
	register("zcount", 4, 0, cmdZCount)
	register("zrange", -4, 0, cmdZRange)
	register("zrevrange", -4, 0, cmdZRange)
	register("zrangebyscore", -4, 0, cmdZRange)
	register("zrevrangebyscore", -4, 0, cmdZRange)
	register("zremrangebyrank", 4, 0, cmdZRemRangeByRank)
	register("zremrangebyscore", 4, 0, cmdZRemRangeByScore)
	register("zpopmin", -2, 0, zpopCmd(false))
	register("zpopmax", -2, 0, zpopCmd(true))
	register("zscan", -3, 0, cmdZScan)
}

func getZSet(x *ctx, key string) (*store.Value, bool) {
	v, err := x.db.GetKind(key, store.KindZSet)
	if err != nil {
		x.fail(err)
		return nil, false
	}
	return v, true
}

// ZADD key [NX|XX] [GT|LT] [CH] [INCR] score member [score member ...]
func cmdZAdd(x *ctx) {
	var nx, xx, gt, lt, ch, incr bool
	i := 2
	for ; i < len(x.args); i++ {
		switch strings.ToLower(x.str(i)) {
		case "nx":
			nx = true
		case "xx":
			xx = true
		case "gt":
			gt = true
		case "lt":
			lt = true
		case "ch":
			ch = true
		case "incr":
			incr = true
		default:
			goto pairs
		}
	}
pairs:
	rest := x.args[i:]
	if len(rest) == 0 || len(rest)%2 != 0 {
		x.fail(errSyntax)
		return
	}
	if nx && (xx || gt || lt) || gt && lt {
		x.w.Error("XX and NX options at the same time are not compatible")
		return
	}
	if incr && len(rest) != 2 {
		x.w.Error("INCR option supports a single increment-element pair")
		return
	}
	scores := make([]float64, len(rest)/2)
	for j := range scores {
		f, ok := parseFloat(string(rest[j*2]))
		if !ok {
			x.fail(errFloat)
			return
		}
		scores[j] = f
	}
	v, err := x.db.GetOrCreate(x.key(), store.KindZSet)
	if err != nil {
		x.fail(err)
		return
	}
	defer x.db.Touched(x.key())

	added, changed := 0, 0
	var last float64
	var skipped bool
	for j, score := range scores {
		m := string(rest[j*2+1])
		old, exists := v.ZSet.Score(m)
		if (nx && exists) || (xx && !exists) {
			skipped = true
			continue
		}
		if incr && exists {
			score += old
			if math.IsNaN(score) {
				x.w.Error("resulting score is not a number (NaN)")
				return
			}
		}
		if exists && ((gt && score <= old) || (lt && score >= old)) {
			skipped = true
			continue
		}
		skipped = false
		last = score
		if v.ZAdd(m, score) {
			added++
		} else if exists && old != score {
			changed++
		}
	}
	if incr {
		if skipped {
			x.w.Null()
		} else {
			x.w.Double(last)
		}
		return
	}
	if ch {
		x.w.Int(int64(added + changed))
	} else {
		x.w.Int(int64(added))
	}
}

func cmdZIncrBy(x *ctx) {
	by, ok := x.float(2)
	if !ok {
		return
	}
	v, err := x.db.GetOrCreate(x.key(), store.KindZSet)
	if err != nil {
		x.fail(err)
		return
	}
	defer x.db.Touched(x.key())
	old, _ := v.ZSet.Score(x.str(3))
	score := old + by
	if math.IsNaN(score) {
		x.w.Error("resulting score is not a number (NaN)")
		return
	}
	v.ZAdd(x.str(3), score)
	x.w.Double(score)
}

func cmdZRem(x *ctx) {
	v, ok := getZSet(x, x.key())
	if !ok {
		return
	}
	n := 0
	if v != nil {
		for _, m := range x.args[2:] {
			if v.ZRem(string(m)) {
				n++
			}
		}
		x.db.Touched(x.key())
	}
	x.w.Int(int64(n))
}

func cmdZCard(x *ctx) {
	if v, ok := getZSet(x, x.key()); ok {
		if v == nil {
			x.w.Int(0)
			return
		}
		x.w.Int(int64(v.Len()))
	}
}

func cmdZScore(x *ctx) {
	v, ok := getZSet(x, x.key())
	if !ok {
		return
	}
	if v == nil {
		x.w.Null()
		return
	}
	s, found := v.ZSet.Score(x.str(2))
	optionalDouble(x, s, found)
}

func optionalDouble(x *ctx, f float64, found bool) {
	if found {
		x.w.Double(f)
	} else {
		x.w.Null()
	}
}

func cmdZMScore(x *ctx) {
	v, ok := getZSet(x, x.key())
	if !ok {
		return
	}
	x.w.Array(len(x.args) - 2)
	for _, m := range x.args[2:] {
		if v == nil {
			x.w.Null()
			continue
		}
		s, found := v.ZSet.Score(string(m))
		optionalDouble(x, s, found)
	}
}

func rankCmd(rev bool) func(*ctx) {
	return func(x *ctx) {
		withScore := len(x.args) == 4 && x.is(3, "withscore")
		if len(x.args) > 4 || (len(x.args) == 4 && !withScore) {
			x.fail(errSyntax)
			return
		}
		v, ok := getZSet(x, x.key())
		if !ok {
			return
		}
		r := -1
		if v != nil {
			r = v.ZSet.Rank(x.str(2))
		}
		if r < 0 {
			if withScore {
				x.w.NullArray()
			} else {
				x.w.Null()
			}
			return
		}
		if rev {
			r = v.Len() - 1 - r
		}
		if withScore {
			s, _ := v.ZSet.Score(x.str(2))
			x.w.Array(2)
			x.w.Int(int64(r))
			x.w.Double(s)
			return
		}
		x.w.Int(int64(r))
	}
}

// scoreBound parses a ZRANGEBYSCORE bound such as 5, (5, -inf or +inf.
type scoreBound struct {
	value     float64
	exclusive bool
}

func parseBound(s string) (scoreBound, bool) {
	b := scoreBound{}
	if strings.HasPrefix(s, "(") {
		b.exclusive = true
		s = s[1:]
	}
	f, ok := parseFloat(s)
	b.value = f
	return b, ok
}

// scoreSpan returns the index range [lo, hi) of items with from <= score <= to.
func scoreSpan(z *store.ZSet, from, to scoreBound) (int, int) {
	lo := z.CountBelow(from.value, from.exclusive)
	hi := z.CountBelow(to.value, !to.exclusive)
	if hi < lo {
		hi = lo
	}
	return lo, hi
}

func cmdZCount(x *ctx) {
	from, ok1 := parseBound(x.str(2))
	to, ok2 := parseBound(x.str(3))
	if !ok1 || !ok2 {
		x.fail(errNotFlt)
		return
	}
	v, ok := getZSet(x, x.key())
	if !ok {
		return
	}
	if v == nil {
		x.w.Int(0)
		return
	}
	lo, hi := scoreSpan(v.ZSet, from, to)
	x.w.Int(int64(hi - lo))
}

// cmdZRange handles ZRANGE with all its options as well as the older
// ZREVRANGE, ZRANGEBYSCORE and ZREVRANGEBYSCORE forms.
func cmdZRange(x *ctx) {
	name := strings.ToLower(x.str(0))
	byScore := strings.Contains(name, "byscore")
	rev := strings.HasPrefix(name, "zrev")
	withScores := false
	var offset, limit int64 = 0, -1
	for i := 4; i < len(x.args); i++ {
		switch {
		case x.is(i, "withscores"):
			withScores = true
		case x.is(i, "byscore") && name == "zrange":
			byScore = true
		case x.is(i, "rev") && name == "zrange":
			rev = true
		case x.is(i, "limit") && i+2 < len(x.args):
			var ok bool
			if offset, ok = x.int(i + 1); !ok {
				return
			}
			if limit, ok = x.int(i + 2); !ok {
				return
			}
			i += 2
		default:
			x.fail(errSyntax)
			return
		}
	}
	if !byScore && limit >= 0 {
		x.w.Error("syntax error, LIMIT is only supported in combination with either BYSCORE or BYLEX")
		return
	}

	var lo, hi int
	var items []store.ZItem
	if byScore {
		a, b := x.str(2), x.str(3)
		if rev {
			a, b = b, a // reverse forms take max first
		}
		from, ok1 := parseBound(a)
		to, ok2 := parseBound(b)
		if !ok1 || !ok2 {
			x.fail(errNotFlt)
			return
		}
		v, ok := getZSet(x, x.key())
		if !ok {
			return
		}
		if v != nil {
			items = v.ZSet.Items()
			lo, hi = scoreSpan(v.ZSet, from, to)
		}
	} else {
		start, ok := x.int(2)
		if !ok {
			return
		}
		stop, ok := x.int(3)
		if !ok {
			return
		}
		v, ok := getZSet(x, x.key())
		if !ok {
			return
		}
		if v != nil {
			items = v.ZSet.Items()
			lo, hi = span(start, stop, len(items))
			if rev {
				lo, hi = len(items)-hi, len(items)-lo
			}
		}
	}

	picked := make([]store.ZItem, 0, hi-lo)
	for i := lo; i < hi; i++ {
		picked = append(picked, items[i])
	}
	if rev {
		for i, j := 0, len(picked)-1; i < j; i, j = i+1, j-1 {
			picked[i], picked[j] = picked[j], picked[i]
		}
	}
	if byScore {
		if offset < 0 || offset >= int64(len(picked)) {
			picked = nil
		} else {
			picked = picked[offset:]
			if limit >= 0 && limit < int64(len(picked)) {
				picked = picked[:limit]
			}
		}
	}
	writeZItems(x, picked, withScores)
}

// writeZItems writes members, with scores as a flat list in RESP2 or as
// [member, score] pairs in RESP3.
func writeZItems(x *ctx, items []store.ZItem, withScores bool) {
	if !withScores {
		x.w.Array(len(items))
		for _, it := range items {
			x.w.BulkString(it.Member)
		}
		return
	}
	pairs(x.w, len(items))
	for _, it := range items {
		pair(x.w)
		x.w.BulkString(it.Member)
		x.w.Double(it.Score)
	}
}

// pairs and pair write n two-element entries: nested in RESP3, flat in RESP2.
func pairs(w *resp.Writer, n int) {
	if w.Proto == 3 {
		w.Array(n)
	} else {
		w.Array(n * 2)
	}
}

func pair(w *resp.Writer) {
	if w.Proto == 3 {
		w.Array(2)
	}
}

func removeItems(x *ctx, v *store.Value, items []store.ZItem) {
	members := make([]string, len(items))
	for i, it := range items {
		members[i] = it.Member
	}
	for _, m := range members {
		v.ZRem(m)
	}
	x.db.Touched(x.key())
	x.w.Int(int64(len(members)))
}

func cmdZRemRangeByRank(x *ctx) {
	start, ok := x.int(2)
	if !ok {
		return
	}
	stop, ok := x.int(3)
	if !ok {
		return
	}
	v, ok := getZSet(x, x.key())
	if !ok {
		return
	}
	if v == nil {
		x.w.Int(0)
		return
	}
	lo, hi := span(start, stop, v.Len())
	removeItems(x, v, v.ZSet.Items()[lo:hi])
}

func cmdZRemRangeByScore(x *ctx) {
	from, ok1 := parseBound(x.str(2))
	to, ok2 := parseBound(x.str(3))
	if !ok1 || !ok2 {
		x.fail(errNotFlt)
		return
	}
	v, ok := getZSet(x, x.key())
	if !ok {
		return
	}
	if v == nil {
		x.w.Int(0)
		return
	}
	lo, hi := scoreSpan(v.ZSet, from, to)
	removeItems(x, v, v.ZSet.Items()[lo:hi])
}

func zpopCmd(highest bool) func(*ctx) {
	return func(x *ctx) {
		count := int64(1)
		if len(x.args) == 3 {
			var ok bool
			if count, ok = x.int(2); !ok {
				return
			}
			if count < 0 {
				x.w.Error("value is out of range, must be positive")
				return
			}
		} else if len(x.args) > 3 {
			x.fail(errSyntax)
			return
		}
		v, ok := getZSet(x, x.key())
		if !ok {
			return
		}
		if v == nil {
			x.w.Array(0)
			return
		}
		items := v.ZSet.Items()
		n := int(min(count, int64(len(items))))
		var picked []store.ZItem
		if highest {
			for i := len(items) - 1; i >= len(items)-n; i-- {
				picked = append(picked, items[i])
			}
		} else {
			picked = append(picked, items[:n]...)
		}
		for _, it := range picked {
			v.ZRem(it.Member)
		}
		x.db.Touched(x.key())
		if len(x.args) == 2 && x.w.Proto == 3 {
			// Without a count RESP3 gets a single flat [member, score].
			x.w.Array(2 * len(picked))
			for _, it := range picked {
				x.w.BulkString(it.Member)
				x.w.Double(it.Score)
			}
			return
		}
		writeZItems(x, picked, true)
	}
}

func cmdZScan(x *ctx) {
	cursor, err := strconv.ParseUint(x.str(2), 10, 64)
	if err != nil {
		x.w.Error("invalid cursor")
		return
	}
	pattern, count, _, ok := scanOptions(x, 3)
	if !ok {
		return
	}
	v, ok := getZSet(x, x.key())
	if !ok {
		return
	}
	var items []store.ZItem
	if v != nil {
		for _, it := range v.ZSet.Items() {
			if pattern == "" || store.Match(pattern, it.Member) {
				items = append(items, it)
			}
		}
	}
	if cursor >= uint64(len(items)) {
		cursor = uint64(len(items))
	}
	end := cursor + uint64(count)
	next := end
	if end >= uint64(len(items)) {
		end, next = uint64(len(items)), 0
	}
	x.w.Array(2)
	x.w.BulkString(strconv.FormatUint(next, 10))
	x.w.Array((int(end) - int(cursor)) * 2)
	for _, it := range items[cursor:end] {
		x.w.BulkString(it.Member)
		x.w.BulkString(formatFloat(it.Score))
	}
}
