package server

import (
	"sort"
	"sync"

	"github.com/hostfog/hfkv/internal/store"
)

// hub tracks subscriptions. Messages are written straight to subscriber
// connections under each client's write lock.
type hub struct {
	mu       sync.RWMutex
	channels map[string]map[*client]struct{}
	patterns map[string]map[*client]struct{}
}

func newHub() *hub {
	return &hub{channels: map[string]map[*client]struct{}{}, patterns: map[string]map[*client]struct{}{}}
}

func (h *hub) count(c *client) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return c.subscribed()
}

func init() {
	register("subscribe", -2, fNoLock|fPubSub|fNoQueue, subscribeCmd(false))
	register("psubscribe", -2, fNoLock|fPubSub|fNoQueue, subscribeCmd(true))
	register("unsubscribe", -1, fNoLock|fPubSub|fNoQueue, unsubscribeCmd(false))
	register("punsubscribe", -1, fNoLock|fPubSub|fNoQueue, unsubscribeCmd(true))
	register("publish", 3, fNoLock, cmdPublish)
	register("pubsub", -2, fNoLock, cmdPubSub)
}

func subscribeCmd(pattern bool) func(*ctx) {
	kind := "subscribe"
	if pattern {
		kind = "psubscribe"
	}
	return func(x *ctx) {
		h := x.s.pubsub
		for _, a := range x.args[1:] {
			name := string(a)
			h.mu.Lock()
			subs, mine := h.channels, x.c.channels
			if pattern {
				subs, mine = h.patterns, x.c.patterns
			}
			if subs[name] == nil {
				subs[name] = map[*client]struct{}{}
			}
			subs[name][x.c] = struct{}{}
			mine[name] = true
			n := x.c.subscribed()
			h.mu.Unlock()

			x.w.Push(3)
			x.w.BulkString(kind)
			x.w.BulkString(name)
			x.w.Int(int64(n))
		}
	}
}

func unsubscribeCmd(pattern bool) func(*ctx) {
	kind := "unsubscribe"
	if pattern {
		kind = "punsubscribe"
	}
	return func(x *ctx) {
		h := x.s.pubsub
		h.mu.Lock()
		mine := x.c.channels
		if pattern {
			mine = x.c.patterns
		}
		var names []string
		if len(x.args) > 1 {
			for _, a := range x.args[1:] {
				names = append(names, string(a))
			}
		} else {
			for n := range mine {
				names = append(names, n)
			}
			sort.Strings(names)
		}
		h.mu.Unlock()

		if len(names) == 0 {
			x.w.Push(3)
			x.w.BulkString(kind)
			x.w.Null()
			x.w.Int(int64(x.s.pubsub.count(x.c)))
			return
		}
		for _, name := range names {
			h.mu.Lock()
			h.remove(x.c, name, pattern)
			n := x.c.subscribed()
			h.mu.Unlock()
			x.w.Push(3)
			x.w.BulkString(kind)
			x.w.BulkString(name)
			x.w.Int(int64(n))
		}
	}
}

// remove drops one subscription; h.mu must be held.
func (h *hub) remove(c *client, name string, pattern bool) {
	subs, mine := h.channels, c.channels
	if pattern {
		subs, mine = h.patterns, c.patterns
	}
	delete(mine, name)
	delete(subs[name], c)
	if len(subs[name]) == 0 {
		delete(subs, name)
	}
}

func (h *hub) unsubscribeAll(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for n := range c.channels {
		h.remove(c, n, false)
	}
	for n := range c.patterns {
		h.remove(c, n, true)
	}
}

// publish delivers msg and returns how many clients received it.
func (h *hub) publish(from *client, channel, msg string) int {
	type delivery struct {
		c       *client
		pattern string
	}
	var out []delivery
	h.mu.RLock()
	for c := range h.channels[channel] {
		out = append(out, delivery{c: c})
	}
	for p, subs := range h.patterns {
		if store.Match(p, channel) {
			for c := range subs {
				out = append(out, delivery{c, p})
			}
		}
	}
	h.mu.RUnlock()

	for _, d := range out {
		// The publisher already holds its own write lock.
		if d.c != from {
			d.c.lock()
		}
		w := d.c.w
		if d.pattern == "" {
			w.Push(3)
			w.BulkString("message")
		} else {
			w.Push(4)
			w.BulkString("pmessage")
			w.BulkString(d.pattern)
		}
		w.BulkString(channel)
		w.BulkString(msg)
		if d.c != from {
			w.Flush()
			d.c.unlock()
		}
	}
	return len(out)
}

func cmdPublish(x *ctx) {
	x.w.Int(int64(x.s.pubsub.publish(x.c, x.str(1), x.str(2))))
}

// PUBSUB CHANNELS [pattern] | NUMSUB [channel ...] | NUMPAT
func cmdPubSub(x *ctx) {
	h := x.s.pubsub
	h.mu.RLock()
	defer h.mu.RUnlock()
	switch {
	case x.is(1, "channels") && len(x.args) <= 3:
		var names []string
		for n := range h.channels {
			if len(x.args) == 2 || store.Match(x.str(2), n) {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		x.w.Strings(names)
	case x.is(1, "numsub"):
		x.w.Array((len(x.args) - 2) * 2)
		for _, a := range x.args[2:] {
			x.w.Bulk(a)
			x.w.Int(int64(len(h.channels[string(a)])))
		}
	case x.is(1, "numpat") && len(x.args) == 2:
		x.w.Int(int64(len(h.patterns)))
	default:
		x.w.Error("unknown subcommand '" + x.str(1) + "'. Try PUBSUB HELP.")
	}
}
