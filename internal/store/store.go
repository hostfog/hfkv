// Package store holds the keyspace: numbered databases of keys with
// optional expiry, and the memory limit with key eviction.
//
// Store is not safe for concurrent use; the server serialises access to it
// with Store.Mu, which keeps every command atomic like Redis.
package store

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrWrongType = errors.New("WRONGTYPE Operation against a key holding the wrong kind of value")
	ErrOOM       = errors.New("OOM command not allowed when used memory > 'maxmemory'")
)

type Policy int

const (
	NoEviction Policy = iota
	AllKeysLRU
	VolatileLRU
	AllKeysRandom
	VolatileRandom
	VolatileTTL
)

var policyNames = []string{"noeviction", "allkeys-lru", "volatile-lru", "allkeys-random", "volatile-random", "volatile-ttl"}

func (p Policy) String() string { return policyNames[p] }

func ParsePolicy(s string) (Policy, bool) {
	for i, n := range policyNames {
		if n == s {
			return Policy(i), true
		}
	}
	return 0, false
}

type entry struct {
	v      *Value
	size   int64 // v.size + key, as last counted in Store.used
	access int64 // clock value at last access, for LRU
}

type DB struct {
	Index   int
	keys    map[string]*entry
	expires map[string]int64 // unix ms
	s       *Store
}

// Watch marks keys a client is WATCHing; Dirty becomes true when one changes.
type Watch struct {
	Dirty bool
}

type watchKey struct {
	db  int
	key string
}

type Store struct {
	Mu sync.Mutex

	DBs       []*DB
	MaxMemory int64
	Policy    Policy

	watches map[watchKey]map[*Watch]struct{}

	used    int64
	clock   int64 // unix ms, refreshed by Tick
	Dirty   int64 // changes since the last save
	Evicted int64
	Expired int64
}

func New(databases int) *Store {
	s := &Store{clock: time.Now().UnixMilli(), watches: map[watchKey]map[*Watch]struct{}{}}
	for i := 0; i < databases; i++ {
		s.DBs = append(s.DBs, &DB{Index: i, keys: map[string]*entry{}, expires: map[string]int64{}, s: s})
	}
	return s
}

func (s *Store) Now() int64 { return s.clock }

func (s *Store) UsedMemory() int64 { return s.used }

// Tick advances the clock and removes a sample of expired keys. Called
// about ten times a second.
func (s *Store) Tick() {
	s.clock = time.Now().UnixMilli()
	for _, db := range s.DBs {
		for round := 0; round < 16; round++ {
			checked, expired := 0, 0
			for k, at := range db.expires {
				if checked == 20 {
					break
				}
				checked++
				if at <= s.clock {
					db.remove(k)
					s.Expired++
					expired++
				}
			}
			// Keep going only while a good share of the sample was stale.
			if expired*4 < checked || checked == 0 {
				break
			}
		}
	}
}

// Flush empties every database.
func (s *Store) Flush() {
	for _, db := range s.DBs {
		db.Flush()
	}
}

// Keys is the total number of keys in all databases.
func (s *Store) Keys() int {
	n := 0
	for _, db := range s.DBs {
		n += len(db.keys)
	}
	return n
}

// Reserve makes room before a write, evicting keys if the policy allows.
func (s *Store) Reserve() error {
	if s.MaxMemory <= 0 || s.used <= s.MaxMemory {
		return nil
	}
	if s.Policy == NoEviction {
		return ErrOOM
	}
	for s.used > s.MaxMemory {
		if !s.evictOne() {
			return ErrOOM
		}
	}
	return nil
}

// evictOne removes the best of a few randomly sampled keys.
func (s *Store) evictOne() bool {
	volatile := s.Policy == VolatileLRU || s.Policy == VolatileRandom || s.Policy == VolatileTTL
	var (
		bestDB  *DB
		bestKey string
		best    int64
	)
	for _, db := range s.DBs {
		sampled := 0
		consider := func(k string) bool {
			e := db.keys[k]
			var score int64
			switch s.Policy {
			case AllKeysLRU, VolatileLRU:
				score = s.clock - e.access
			case VolatileTTL:
				score = -db.expires[k]
			}
			if bestDB == nil || score > best {
				bestDB, bestKey, best = db, k, score
			}
			sampled++
			return sampled < 5
		}
		if volatile {
			for k := range db.expires {
				if !consider(k) {
					break
				}
			}
		} else {
			for k := range db.keys {
				if !consider(k) {
					break
				}
			}
		}
	}
	if bestDB == nil {
		return false
	}
	bestDB.remove(bestKey)
	s.Evicted++
	return true
}

// AddWatch makes w dirty when key in db changes.
func (s *Store) AddWatch(db int, key string, w *Watch) {
	k := watchKey{db, key}
	if s.watches[k] == nil {
		s.watches[k] = map[*Watch]struct{}{}
	}
	s.watches[k][w] = struct{}{}
}

// RemoveWatch forgets w for key.
func (s *Store) RemoveWatch(db int, key string, w *Watch) {
	k := watchKey{db, key}
	delete(s.watches[k], w)
	if len(s.watches[k]) == 0 {
		delete(s.watches, k)
	}
}

func (db *DB) changed(key string) {
	if len(db.s.watches) == 0 {
		return
	}
	for w := range db.s.watches[watchKey{db.Index, key}] {
		w.Dirty = true
	}
}

func (db *DB) Len() int { return len(db.keys) }

func (db *DB) Expiring() int { return len(db.expires) }

func (db *DB) Flush() {
	for k, e := range db.keys {
		db.s.used -= e.size
		db.changed(k)
	}
	db.keys = map[string]*entry{}
	db.expires = map[string]int64{}
}

func (db *DB) remove(key string) {
	if e, ok := db.keys[key]; ok {
		db.s.used -= e.size
		delete(db.keys, key)
		delete(db.expires, key)
		db.changed(key)
	}
}

// lookup returns the live entry for key, deleting it first if it expired.
func (db *DB) lookup(key string) *entry {
	e, ok := db.keys[key]
	if !ok {
		return nil
	}
	if at, ok := db.expires[key]; ok && at <= db.s.clock {
		db.remove(key)
		db.s.Expired++
		return nil
	}
	e.access = db.s.clock
	return e
}

// Get returns the value at key, or nil.
func (db *DB) Get(key string) *Value {
	if e := db.lookup(key); e != nil {
		return e.v
	}
	return nil
}

// GetKind returns the value at key if it has kind k, nil if the key is
// missing, or ErrWrongType.
func (db *DB) GetKind(key string, k Kind) (*Value, error) {
	v := db.Get(key)
	if v != nil && v.Kind != k {
		return nil, ErrWrongType
	}
	return v, nil
}

// GetOrCreate returns the value at key, creating an empty one of kind k.
func (db *DB) GetOrCreate(key string, k Kind) (*Value, error) {
	v, err := db.GetKind(key, k)
	if err != nil || v != nil {
		return v, err
	}
	v = newValue(k)
	db.keys[key] = &entry{v: v, access: db.s.clock}
	return v, nil
}

// Set stores v at key, clearing any expiry unless keepTTL is set.
func (db *DB) Set(key string, v *Value, keepTTL bool) {
	if e, ok := db.keys[key]; ok {
		db.s.used -= e.size
	}
	db.keys[key] = &entry{v: v, access: db.s.clock}
	if !keepTTL {
		delete(db.expires, key)
	}
	db.Touched(key)
}

// Touched records a change to key: it updates memory accounting, removes
// collections left empty and counts the change for saving.
func (db *DB) Touched(key string) {
	db.s.Dirty++
	db.changed(key)
	e, ok := db.keys[key]
	if !ok {
		return
	}
	if e.v.Kind != KindString && e.v.Len() == 0 {
		db.remove(key)
		return
	}
	size := e.v.size + int64(len(key)) + overhead
	db.s.used += size - e.size
	e.size = size
}

func (db *DB) Delete(key string) bool {
	if db.lookup(key) == nil {
		return false
	}
	db.remove(key)
	db.s.Dirty++
	return true
}

func (db *DB) Exists(key string) bool { return db.lookup(key) != nil }

// ExpireAt sets key to expire at the unix time in ms. A time in the past
// deletes the key.
func (db *DB) ExpireAt(key string, at int64) bool {
	if db.lookup(key) == nil {
		return false
	}
	if at <= db.s.clock {
		db.remove(key)
	} else {
		db.expires[key] = at
		db.changed(key)
	}
	db.s.Dirty++
	return true
}

// TTL returns the time left in ms: -2 if key is missing, -1 if it has no expiry.
func (db *DB) TTL(key string) int64 {
	if db.lookup(key) == nil {
		return -2
	}
	at, ok := db.expires[key]
	if !ok {
		return -1
	}
	return at - db.s.clock
}

// ExpiresAt returns key's expiry time in unix ms, or 0.
func (db *DB) ExpiresAt(key string) int64 { return db.expires[key] }

func (db *DB) Persist(key string) bool {
	if db.lookup(key) == nil {
		return false
	}
	if _, ok := db.expires[key]; !ok {
		return false
	}
	delete(db.expires, key)
	db.changed(key)
	db.s.Dirty++
	return true
}

func (db *DB) Rename(from, to string) bool {
	e := db.lookup(from)
	if e == nil {
		return false
	}
	at, hasTTL := db.expires[from]
	db.remove(from)
	db.remove(to)
	db.keys[to] = &entry{v: e.v, access: db.s.clock}
	if hasTTL {
		db.expires[to] = at
	}
	db.Touched(to)
	return true
}

// Each calls fn for every live key until fn returns false.
func (db *DB) Each(fn func(key string, v *Value) bool) {
	for k, e := range db.keys {
		if at, ok := db.expires[k]; ok && at <= db.s.clock {
			continue
		}
		if !fn(k, e.v) {
			return
		}
	}
}

// RandomKey returns any live key, or "".
func (db *DB) RandomKey() string {
	for k := range db.keys {
		if db.lookup(k) != nil {
			return k
		}
	}
	return ""
}

// Move takes key out of db and puts it into dst. It fails if dst has the key.
func (db *DB) Move(key string, dst *DB) bool {
	e := db.lookup(key)
	if e == nil || dst.Exists(key) {
		return false
	}
	at, hasTTL := db.expires[key]
	db.remove(key)
	dst.keys[key] = &entry{v: e.v, access: db.s.clock}
	if hasTTL {
		dst.expires[key] = at
	}
	dst.Touched(key)
	return true
}

// Load adds a key read from disk.
func (db *DB) Load(key string, v *Value, expireAt int64) {
	v.Resize()
	db.keys[key] = &entry{v: v, access: db.s.clock}
	if expireAt > 0 {
		db.expires[key] = expireAt
	}
	db.Touched(key)
}

// Match reports whether s matches the glob pattern (*, ?, [abc], [a-z],
// [^a] and \ escapes), as used by KEYS and SCAN.
func Match(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			for len(pattern) > 1 && pattern[1] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 1 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if Match(pattern[1:], s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(s) == 0 {
				return false
			}
			s = s[1:]
			pattern = pattern[1:]
		case '[':
			if len(s) == 0 {
				return false
			}
			end := 1
			for end < len(pattern) && pattern[end] != ']' {
				if pattern[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(pattern) {
				return pattern == s // unterminated class: compare literally
			}
			class := pattern[1:end]
			negate := len(class) > 0 && class[0] == '^'
			if negate {
				class = class[1:]
			}
			found := false
			for i := 0; i < len(class); i++ {
				c := class[i]
				if c == '\\' && i+1 < len(class) {
					i++
					c = class[i]
				}
				if i+2 < len(class) && class[i+1] == '-' {
					lo, hi := c, class[i+2]
					if lo > hi {
						lo, hi = hi, lo
					}
					if s[0] >= lo && s[0] <= hi {
						found = true
					}
					i += 2
				} else if s[0] == c {
					found = true
				}
			}
			if found == negate {
				return false
			}
			s = s[1:]
			pattern = pattern[end+1:]
		case '\\':
			if len(pattern) > 1 {
				pattern = pattern[1:]
			}
			fallthrough
		default:
			if len(s) == 0 || s[0] != pattern[0] {
				return false
			}
			s = s[1:]
			pattern = pattern[1:]
		}
	}
	return len(s) == 0
}

// Swap exchanges the contents of two databases (SWAPDB).
func (s *Store) Swap(a, b int) {
	da, db := s.DBs[a], s.DBs[b]
	for k := range da.keys {
		da.changed(k)
	}
	for k := range db.keys {
		db.changed(k)
	}
	da.keys, db.keys = db.keys, da.keys
	da.expires, db.expires = db.expires, da.expires
	for k := range da.keys {
		da.changed(k)
	}
	for k := range db.keys {
		db.changed(k)
	}
	s.Dirty++
}
