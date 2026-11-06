package store

import "sort"

type Kind byte

const (
	KindString Kind = iota
	KindList
	KindHash
	KindSet
	KindZSet
)

func (k Kind) String() string {
	return [...]string{"string", "list", "hash", "set", "zset"}[k]
}

// Value is one stored value. Only the field for its Kind is used. size is a
// rough count of the bytes it holds, kept up to date by the methods below so
// that maxmemory can be enforced without walking the data.
type Value struct {
	Kind Kind
	Str  string
	List *List
	Hash map[string]string
	Set  map[string]struct{}
	ZSet *ZSet
	size int64
}

const overhead = 48 // per element, roughly what Go spends on map/slice entries

func NewString(s string) *Value {
	return &Value{Kind: KindString, Str: s, size: int64(len(s))}
}

func newValue(k Kind) *Value {
	v := &Value{Kind: k}
	switch k {
	case KindList:
		v.List = &List{}
	case KindHash:
		v.Hash = make(map[string]string)
	case KindSet:
		v.Set = make(map[string]struct{})
	case KindZSet:
		v.ZSet = newZSet()
	}
	return v
}

// Len is the number of elements in a collection (or bytes in a string).
func (v *Value) Len() int {
	switch v.Kind {
	case KindList:
		return v.List.Len()
	case KindHash:
		return len(v.Hash)
	case KindSet:
		return len(v.Set)
	case KindZSet:
		return len(v.ZSet.dict)
	}
	return len(v.Str)
}

func (v *Value) SetStr(s string) {
	v.size += int64(len(s) - len(v.Str))
	v.Str = s
}

// HSet sets a field and reports whether it is new.
func (v *Value) HSet(field, val string) bool {
	old, ok := v.Hash[field]
	if ok {
		v.size += int64(len(val) - len(old))
	} else {
		v.size += int64(len(field)+len(val)) + overhead
	}
	v.Hash[field] = val
	return !ok
}

func (v *Value) HDel(field string) bool {
	old, ok := v.Hash[field]
	if ok {
		delete(v.Hash, field)
		v.size -= int64(len(field)+len(old)) + overhead
	}
	return ok
}

func (v *Value) SAdd(m string) bool {
	if _, ok := v.Set[m]; ok {
		return false
	}
	v.Set[m] = struct{}{}
	v.size += int64(len(m)) + overhead
	return true
}

func (v *Value) SRem(m string) bool {
	if _, ok := v.Set[m]; !ok {
		return false
	}
	delete(v.Set, m)
	v.size -= int64(len(m)) + overhead
	return true
}

// List is a double-ended queue: front holds the head in reverse order, so
// pushes and pops at either end are cheap.
type List struct {
	front []string
	back  []string
}

func (l *List) Len() int { return len(l.front) + len(l.back) }

func (l *List) At(i int) string {
	if i < len(l.front) {
		return l.front[len(l.front)-1-i]
	}
	return l.back[i-len(l.front)]
}

func (l *List) Set(i int, s string) {
	if i < len(l.front) {
		l.front[len(l.front)-1-i] = s
	} else {
		l.back[i-len(l.front)] = s
	}
}

func (l *List) Items() []string {
	out := make([]string, 0, l.Len())
	for i := len(l.front) - 1; i >= 0; i-- {
		out = append(out, l.front[i])
	}
	return append(out, l.back...)
}

func (l *List) Replace(items []string) {
	l.front = nil
	l.back = items
}

func (v *Value) LPush(s string) {
	v.List.front = append(v.List.front, s)
	v.size += int64(len(s)) + overhead
}

func (v *Value) RPush(s string) {
	v.List.back = append(v.List.back, s)
	v.size += int64(len(s)) + overhead
}

func (v *Value) LPop() (string, bool) {
	l := v.List
	var s string
	switch {
	case len(l.front) > 0:
		s = l.front[len(l.front)-1]
		l.front[len(l.front)-1] = ""
		l.front = l.front[:len(l.front)-1]
	case len(l.back) > 0:
		s = l.back[0]
		l.back[0] = ""
		l.back = l.back[1:]
	default:
		return "", false
	}
	v.size -= int64(len(s)) + overhead
	return s, true
}

func (v *Value) RPop() (string, bool) {
	l := v.List
	var s string
	switch {
	case len(l.back) > 0:
		s = l.back[len(l.back)-1]
		l.back[len(l.back)-1] = ""
		l.back = l.back[:len(l.back)-1]
	case len(l.front) > 0:
		s = l.front[0]
		l.front[0] = ""
		l.front = l.front[1:]
	default:
		return "", false
	}
	v.size -= int64(len(s)) + overhead
	return s, true
}

// Resize recomputes size after a bulk change such as LTRIM or LREM.
func (v *Value) Resize() {
	var n int64
	switch v.Kind {
	case KindString:
		n = int64(len(v.Str))
	case KindList:
		for _, s := range v.List.front {
			n += int64(len(s)) + overhead
		}
		for _, s := range v.List.back {
			n += int64(len(s)) + overhead
		}
	case KindHash:
		for f, s := range v.Hash {
			n += int64(len(f)+len(s)) + overhead
		}
	case KindSet:
		for m := range v.Set {
			n += int64(len(m)) + overhead
		}
	case KindZSet:
		for m := range v.ZSet.dict {
			n += int64(len(m))*2 + overhead
		}
	}
	v.size = n
}

// ZSet keeps members ordered by (score, member) in a sorted slice, with a
// map for score lookups.
type ZSet struct {
	dict   map[string]float64
	sorted []ZItem
}

type ZItem struct {
	Member string
	Score  float64
}

func newZSet() *ZSet { return &ZSet{dict: make(map[string]float64)} }

func less(a, b ZItem) bool {
	if a.Score != b.Score {
		return a.Score < b.Score
	}
	return a.Member < b.Member
}

func (z *ZSet) find(it ZItem) int {
	return sort.Search(len(z.sorted), func(i int) bool { return !less(z.sorted[i], it) })
}

func (z *ZSet) Score(m string) (float64, bool) {
	s, ok := z.dict[m]
	return s, ok
}

func (z *ZSet) Items() []ZItem { return z.sorted }

// Rank is the 0-based position of m, or -1.
func (z *ZSet) Rank(m string) int {
	s, ok := z.dict[m]
	if !ok {
		return -1
	}
	return z.find(ZItem{m, s})
}

// CountBelow is the number of items ordered before score (or at it, when
// inclusive is false the items equal to score are not counted).
func (z *ZSet) CountBelow(score float64, inclusive bool) int {
	return sort.Search(len(z.sorted), func(i int) bool {
		if inclusive {
			return z.sorted[i].Score > score
		}
		return z.sorted[i].Score >= score
	})
}

// ZAdd sets m's score and reports whether m is new.
func (v *Value) ZAdd(m string, score float64) bool {
	z := v.ZSet
	old, ok := z.dict[m]
	if ok {
		if old == score {
			return false
		}
		v.zremove(m, old)
	}
	z.dict[m] = score
	it := ZItem{m, score}
	i := z.find(it)
	z.sorted = append(z.sorted, ZItem{})
	copy(z.sorted[i+1:], z.sorted[i:])
	z.sorted[i] = it
	if !ok {
		v.size += int64(len(m))*2 + overhead
	}
	return !ok
}

func (v *Value) ZRem(m string) bool {
	old, ok := v.ZSet.dict[m]
	if !ok {
		return false
	}
	v.zremove(m, old)
	delete(v.ZSet.dict, m)
	v.size -= int64(len(m))*2 + overhead
	return true
}

func (v *Value) zremove(m string, score float64) {
	z := v.ZSet
	i := z.find(ZItem{m, score})
	z.sorted = append(z.sorted[:i], z.sorted[i+1:]...)
}

func NewList() *Value { return newValue(KindList) }
func NewHash() *Value { return newValue(KindHash) }
func NewSet() *Value  { return newValue(KindSet) }
func NewZSet() *Value { return newValue(KindZSet) }
