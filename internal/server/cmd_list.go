package server

import (
	"strconv"
	"strings"
	"time"

	"github.com/hostfog/hfkv/internal/store"
)

func init() {
	register("lpush", -3, fDenyOOM, pushCmd(true, false))
	register("rpush", -3, fDenyOOM, pushCmd(false, false))
	register("lpushx", -3, fDenyOOM, pushCmd(true, true))
	register("rpushx", -3, fDenyOOM, pushCmd(false, true))
	register("lpop", -2, 0, popCmd(true))
	register("rpop", -2, 0, popCmd(false))
	register("llen", 2, 0, cmdLLen)
	register("lrange", 4, 0, cmdLRange)
	register("lindex", 3, 0, cmdLIndex)
	register("lset", 4, fDenyOOM, cmdLSet)
	register("lrem", 4, 0, cmdLRem)
	register("ltrim", 4, 0, cmdLTrim)
	register("linsert", 5, fDenyOOM, cmdLInsert)
	register("lpos", -3, 0, cmdLPos)
	register("lmove", 5, fDenyOOM, cmdLMove)
	register("rpoplpush", 3, fDenyOOM, cmdLMove)
	register("blpop", -3, fNoLock|fBlocking, blockingPop(true))
	register("brpop", -3, fNoLock|fBlocking, blockingPop(false))
	register("blmove", 6, fNoLock|fBlocking, cmdBLMove)
	register("brpoplpush", 4, fNoLock|fBlocking, cmdBLMove)
}

func getList(x *ctx, key string) (*store.Value, bool) {
	v, err := x.db.GetKind(key, store.KindList)
	if err != nil {
		x.fail(err)
		return nil, false
	}
	return v, true
}

func pushCmd(left, onlyIfExists bool) func(*ctx) {
	return func(x *ctx) {
		key := x.key()
		var v *store.Value
		var err error
		if onlyIfExists {
			if v, err = x.db.GetKind(key, store.KindList); err == nil && v == nil {
				x.w.Int(0)
				return
			}
		} else {
			v, err = x.db.GetOrCreate(key, store.KindList)
		}
		if err != nil {
			x.fail(err)
			return
		}
		for _, a := range x.args[2:] {
			if left {
				v.LPush(string(a))
			} else {
				v.RPush(string(a))
			}
		}
		x.db.Touched(key)
		x.s.wake(x.db.Index, key)
		x.w.Int(int64(v.Len()))
	}
}

func popCmd(left bool) func(*ctx) {
	return func(x *ctx) {
		count, withCount := int64(1), len(x.args) == 3
		if len(x.args) > 3 {
			x.fail(errSyntax)
			return
		}
		if withCount {
			n, ok := x.int(2)
			if !ok {
				return
			}
			if n < 0 {
				x.w.Error("value is out of range, must be positive")
				return
			}
			count = n
		}
		v, ok := getList(x, x.key())
		if !ok {
			return
		}
		if v == nil {
			if withCount {
				x.w.NullArray()
			} else {
				x.w.Null()
			}
			return
		}
		var out []string
		for i := int64(0); i < count; i++ {
			s, ok := pop(v, left)
			if !ok {
				break
			}
			out = append(out, s)
		}
		x.db.Touched(x.key())
		if withCount {
			x.w.Strings(out)
		} else {
			x.w.BulkString(out[0])
		}
	}
}

func pop(v *store.Value, left bool) (string, bool) {
	if left {
		return v.LPop()
	}
	return v.RPop()
}

func cmdLLen(x *ctx) {
	if v, ok := getList(x, x.key()); ok {
		if v == nil {
			x.w.Int(0)
			return
		}
		x.w.Int(int64(v.Len()))
	}
}

func cmdLRange(x *ctx) {
	start, ok := x.int(2)
	if !ok {
		return
	}
	stop, ok := x.int(3)
	if !ok {
		return
	}
	v, ok := getList(x, x.key())
	if !ok {
		return
	}
	if v == nil {
		x.w.Array(0)
		return
	}
	lo, hi := span(start, stop, v.Len())
	x.w.Array(hi - lo)
	for i := lo; i < hi; i++ {
		x.w.BulkString(v.List.At(i))
	}
}

func listIndex(i int64, n int) (int, bool) {
	if i < 0 {
		i += int64(n)
	}
	return int(i), i >= 0 && i < int64(n)
}

func cmdLIndex(x *ctx) {
	i, ok := x.int(2)
	if !ok {
		return
	}
	v, ok := getList(x, x.key())
	if !ok {
		return
	}
	if v == nil {
		x.w.Null()
		return
	}
	idx, in := listIndex(i, v.Len())
	if !in {
		x.w.Null()
		return
	}
	x.w.BulkString(v.List.At(idx))
}

func cmdLSet(x *ctx) {
	i, ok := x.int(2)
	if !ok {
		return
	}
	v, ok := getList(x, x.key())
	if !ok {
		return
	}
	if v == nil {
		x.fail(errNoKey)
		return
	}
	idx, in := listIndex(i, v.Len())
	if !in {
		x.fail(errIndex)
		return
	}
	v.List.Set(idx, x.str(3))
	v.Resize()
	x.db.Touched(x.key())
	x.w.OK()
}

func cmdLRem(x *ctx) {
	count, ok := x.int(2)
	if !ok {
		return
	}
	v, ok := getList(x, x.key())
	if !ok {
		return
	}
	if v == nil {
		x.w.Int(0)
		return
	}
	target, items := x.str(3), v.List.Items()
	removed := 0
	keep := make([]bool, len(items))
	for i := range keep {
		keep[i] = true
	}
	if count >= 0 {
		for i := 0; i < len(items) && (count == 0 || int64(removed) < count); i++ {
			if items[i] == target {
				keep[i] = false
				removed++
			}
		}
	} else {
		for i := len(items) - 1; i >= 0 && int64(removed) < -count; i-- {
			if items[i] == target {
				keep[i] = false
				removed++
			}
		}
	}
	if removed > 0 {
		out := items[:0]
		for i, s := range items {
			if keep[i] {
				out = append(out, s)
			}
		}
		v.List.Replace(out)
		v.Resize()
		x.db.Touched(x.key())
	}
	x.w.Int(int64(removed))
}

func cmdLTrim(x *ctx) {
	start, ok := x.int(2)
	if !ok {
		return
	}
	stop, ok := x.int(3)
	if !ok {
		return
	}
	v, ok := getList(x, x.key())
	if !ok {
		return
	}
	if v != nil {
		lo, hi := span(start, stop, v.Len())
		v.List.Replace(append([]string(nil), v.List.Items()[lo:hi]...))
		v.Resize()
		x.db.Touched(x.key())
	}
	x.w.OK()
}

func cmdLInsert(x *ctx) {
	before := x.is(2, "before")
	if !before && !x.is(2, "after") {
		x.fail(errSyntax)
		return
	}
	v, ok := getList(x, x.key())
	if !ok {
		return
	}
	if v == nil {
		x.w.Int(0)
		return
	}
	items := v.List.Items()
	pivot := x.str(3)
	for i, s := range items {
		if s != pivot {
			continue
		}
		if !before {
			i++
		}
		items = append(items[:i], append([]string{x.str(4)}, items[i:]...)...)
		v.List.Replace(items)
		v.Resize()
		x.db.Touched(x.key())
		x.w.Int(int64(len(items)))
		return
	}
	x.w.Int(-1)
}

// LPOS key element [RANK rank] [COUNT n] [MAXLEN len]
func cmdLPos(x *ctx) {
	rank, count, maxLen := int64(1), int64(-1), int64(0)
	for i := 3; i < len(x.args); i += 2 {
		if i+1 >= len(x.args) {
			x.fail(errSyntax)
			return
		}
		n, ok := x.int(i + 1)
		if !ok {
			return
		}
		switch {
		case x.is(i, "rank"):
			if n == 0 {
				x.w.Error("RANK can't be zero: use 1 to start from the first match, 2 from the second ... or use negative to start from the end of the list")
				return
			}
			rank = n
		case x.is(i, "count") && n >= 0:
			count = n
		case x.is(i, "maxlen") && n >= 0:
			maxLen = n
		default:
			x.fail(errSyntax)
			return
		}
	}
	v, ok := getList(x, x.key())
	if !ok {
		return
	}
	var found []int64
	if v != nil {
		target, n := x.str(2), v.Len()
		skip := rank - 1
		step, i := 1, 0
		if rank < 0 {
			skip, step, i = -rank-1, -1, n-1
		}
		for scanned := int64(0); i >= 0 && i < n && (maxLen == 0 || scanned < maxLen); i, scanned = i+step, scanned+1 {
			if v.List.At(i) != target {
				continue
			}
			if skip > 0 {
				skip--
				continue
			}
			found = append(found, int64(i))
			if count != 0 && int64(len(found)) >= max(count, 1) {
				break
			}
		}
	}
	if count < 0 {
		if len(found) == 0 {
			x.w.Null()
		} else {
			x.w.Int(found[0])
		}
		return
	}
	x.w.Array(len(found))
	for _, i := range found {
		x.w.Int(i)
	}
}

// move pops from src and pushes to dst; the store lock must be held.
func move(x *ctx, src, dst string, fromLeft, toLeft bool) (string, bool, bool) {
	sv, ok := getList(x, src)
	if !ok || sv == nil {
		return "", false, ok
	}
	if _, err := x.db.GetKind(dst, store.KindList); err != nil {
		x.fail(err)
		return "", false, false
	}
	s, _ := pop(sv, fromLeft)
	x.db.Touched(src)
	dv, _ := x.db.GetOrCreate(dst, store.KindList)
	if toLeft {
		dv.LPush(s)
	} else {
		dv.RPush(s)
	}
	x.db.Touched(dst)
	x.s.wake(x.db.Index, dst)
	return s, true, true
}

func directions(x *ctx, i int) (fromLeft, toLeft, ok bool) {
	if x.is(0, "rpoplpush") || x.is(0, "brpoplpush") {
		return false, true, true
	}
	side := func(a string) (bool, bool) {
		switch strings.ToLower(a) {
		case "left":
			return true, true
		case "right":
			return false, true
		}
		return false, false
	}
	var ok1, ok2 bool
	fromLeft, ok1 = side(x.str(i))
	toLeft, ok2 = side(x.str(i + 1))
	if !ok1 || !ok2 {
		x.fail(errSyntax)
		return false, false, false
	}
	return fromLeft, toLeft, true
}

func cmdLMove(x *ctx) {
	fromLeft, toLeft, ok := directions(x, 3)
	if !ok {
		return
	}
	s, moved, ok := move(x, x.key(), x.str(2), fromLeft, toLeft)
	if !ok {
		return
	}
	replyOptional(x, s, moved)
}

// Blocking commands wait for a push to one of their keys.

const fBlocking = fNoQueue << 1

type waiter struct {
	ch    chan struct{}
	fired bool
}

// wake releases clients blocked on key; the store lock must be held.
func (s *Server) wake(db int, key string) {
	k := dbKey{db, key}
	for _, w := range s.waiters[k] {
		if !w.fired {
			w.fired = true
			close(w.ch)
		}
	}
	delete(s.waiters, k)
}

func (s *Server) unwait(db int, keys []string, w *waiter) {
	for _, key := range keys {
		k := dbKey{db, key}
		list := s.waiters[k]
		for i, o := range list {
			if o == w {
				list = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(list) == 0 {
			delete(s.waiters, k)
		} else {
			s.waiters[k] = list
		}
	}
}

func timeoutArg(x *ctx, i int) (time.Duration, bool) {
	f, err := strconv.ParseFloat(x.str(i), 64)
	if err != nil {
		x.w.Error("timeout is not a float or out of range")
		return 0, false
	}
	if f < 0 {
		x.w.Error("timeout is negative")
		return 0, false
	}
	return time.Duration(f * float64(time.Second)), true
}

// block runs try until it succeeds, the timeout passes, the client hangs up
// or the server stops. try is called with the store lock held and writes
// the reply when it succeeds. Inside EXEC it runs once without waiting.
func (s *Server) block(x *ctx, keys []string, timeout time.Duration, try func() bool) {
	if x.inExec {
		x.db = s.store.DBs[x.c.db]
		if !try() {
			x.w.NullArray()
		}
		return
	}
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	// Watch for the client disconnecting while it waits.
	gone := make(chan error, 1)
	watching := true
	go func() { gone <- x.c.r.Peek() }()
	defer func() {
		if watching {
			x.c.conn.SetReadDeadline(time.Now())
			<-gone
			x.c.conn.SetReadDeadline(time.Time{})
		}
	}()

	for {
		s.store.Mu.Lock()
		x.db = s.store.DBs[x.c.db]
		if try() {
			s.store.Mu.Unlock()
			return
		}
		w := &waiter{ch: make(chan struct{})}
		for _, k := range keys {
			kk := dbKey{x.c.db, k}
			s.waiters[kk] = append(s.waiters[kk], w)
		}
		s.store.Mu.Unlock()

		var stop bool
		select {
		case <-w.ch:
		case <-deadline:
			x.w.NullArray()
			stop = true
		case err := <-gone:
			watching = false
			if err != nil {
				stop = true // client went away; nothing to reply to
			}
		case <-s.done:
			stop = true
		}
		s.store.Mu.Lock()
		s.unwait(x.c.db, keys, w)
		s.store.Mu.Unlock()
		if stop {
			return
		}
	}
}

func blockingPop(left bool) func(*ctx) {
	return func(x *ctx) {
		timeout, ok := timeoutArg(x, len(x.args)-1)
		if !ok {
			return
		}
		keys := make([]string, 0, len(x.args)-2)
		for _, a := range x.args[1 : len(x.args)-1] {
			keys = append(keys, string(a))
		}
		x.s.block(x, keys, timeout, func() bool {
			for _, k := range keys {
				v, err := x.db.GetKind(k, store.KindList)
				if err != nil {
					x.fail(err)
					return true
				}
				if v == nil {
					continue
				}
				s, _ := pop(v, left)
				x.db.Touched(k)
				x.w.Strings([]string{k, s})
				return true
			}
			return false
		})
	}
}

func cmdBLMove(x *ctx) {
	fromLeft, toLeft, ok := directions(x, 3)
	if !ok {
		return
	}
	timeout, ok := timeoutArg(x, len(x.args)-1)
	if !ok {
		return
	}
	src, dst := x.key(), x.str(2)
	x.s.block(x, []string{src}, timeout, func() bool {
		s, moved, ok := move(x, src, dst, fromLeft, toLeft)
		if !ok {
			return true
		}
		if moved {
			x.w.BulkString(s)
		}
		return moved
	})
}
