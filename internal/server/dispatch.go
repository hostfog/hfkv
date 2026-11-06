package server

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/hostfog/hfkv/internal/resp"
	"github.com/hostfog/hfkv/internal/store"
)

type flags uint8

const (
	fDenyOOM flags = 1 << iota // refused when over maxmemory and nothing can be evicted
	fNoLock                    // runs without the store lock
	fNoAuth                    // allowed before AUTH
	fPubSub                    // allowed while subscribed
	fNoQueue                   // runs immediately inside MULTI
)

type command struct {
	arity int // exact number of args including the name; -n means at least n
	flags flags
	run   func(*ctx)
}

// ctx is one command being run.
type ctx struct {
	s    *Server
	c    *client
	args [][]byte
	w    *resp.Writer
	db   *store.DB

	inExec bool // running inside EXEC: never block
}

var commands map[string]*command

func register(name string, arity int, f flags, run func(*ctx)) {
	if commands == nil {
		commands = map[string]*command{}
	}
	commands[name] = &command{arity: arity, flags: f, run: run}
}

// dispatch runs one command and reports whether the client asked to quit.
func (s *Server) dispatch(c *client, args [][]byte) bool {
	name := strings.ToLower(string(args[0]))
	c.lastCmd = name
	cmd := commands[name]
	w := c.w

	if cmd == nil {
		if c.inMulti {
			c.multiErr = true
		}
		w.Error(fmt.Sprintf("unknown command '%s', with args beginning with: %s", args[0], quoteArgs(args[1:])))
		return false
	}
	if (cmd.arity > 0 && len(args) != cmd.arity) || (cmd.arity < 0 && len(args) < -cmd.arity) {
		if c.inMulti {
			c.multiErr = true
		}
		w.Error(fmt.Sprintf("wrong number of arguments for '%s' command", name))
		return false
	}
	if s.cfg.Password != "" && !c.authed && cmd.flags&fNoAuth == 0 {
		w.Error("NOAUTH Authentication required.")
		return false
	}
	if w.Proto == 2 && cmd.flags&fPubSub == 0 && s.pubsub.count(c) > 0 {
		w.Error(fmt.Sprintf("Can't execute '%s': only (P|S)SUBSCRIBE / (P|S)UNSUBSCRIBE / PING / QUIT / RESET are allowed in this context", name))
		return false
	}
	if c.inMulti && cmd.flags&fNoQueue == 0 {
		c.queued = append(c.queued, args)
		w.Simple("QUEUED")
		return false
	}
	if name == "quit" {
		w.OK()
		return true
	}

	x := &ctx{s: s, c: c, args: args, w: w}
	if cmd.flags&fNoLock != 0 {
		cmd.run(x)
		return false
	}
	s.store.Mu.Lock()
	s.exec(x, cmd)
	s.store.Mu.Unlock()
	return false
}

// exec runs a data command; the store lock must be held.
func (s *Server) exec(x *ctx, cmd *command) {
	x.db = s.store.DBs[x.c.db]
	if cmd.flags&fDenyOOM != 0 {
		if err := s.store.Reserve(); err != nil {
			x.w.Error(err.Error())
			return
		}
	}
	cmd.run(x)
}

func quoteArgs(args [][]byte) string {
	var b strings.Builder
	for i, a := range args {
		if i == 10 {
			break
		}
		fmt.Fprintf(&b, "'%s' ", a)
	}
	return b.String()
}

func auth(s *Server, password string) bool {
	return subtle.ConstantTimeCompare([]byte(password), []byte(s.cfg.Password)) == 1
}

// Argument helpers. Each writes the error reply itself and returns ok=false.

var (
	errSyntax   = errors.New("syntax error")
	errInt      = errors.New("value is not an integer or out of range")
	errFloat    = errors.New("value is not a valid float")
	errNotFlt   = errors.New("min or max is not a float")
	errIndex    = errors.New("index out of range")
	errNoKey    = errors.New("no such key")
	errOverflow = errors.New("increment or decrement would overflow")
)

func (x *ctx) str(i int) string { return string(x.args[i]) }

func (x *ctx) key() string { return string(x.args[1]) }

func (x *ctx) fail(err error) { x.w.Error(err.Error()) }

func (x *ctx) int(i int) (int64, bool) {
	n, err := strconv.ParseInt(string(x.args[i]), 10, 64)
	if err != nil {
		x.fail(errInt)
		return 0, false
	}
	return n, true
}

func (x *ctx) float(i int) (float64, bool) {
	f, ok := parseFloat(string(x.args[i]))
	if !ok {
		x.fail(errFloat)
	}
	return f, ok
}

func parseFloat(s string) (float64, bool) {
	switch strings.ToLower(s) {
	case "inf", "+inf":
		return math.Inf(1), true
	case "-inf":
		return math.Inf(-1), true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || strings.ContainsAny(s, " \t") {
		return 0, false
	}
	return f, true
}

func formatFloat(f float64) string { return resp.FormatFloat(f) }

// is reports whether arg i equals word, ignoring case.
func (x *ctx) is(i int, word string) bool {
	return strings.EqualFold(string(x.args[i]), word)
}

// span turns Redis start/stop indexes (negative counts from the end) into
// a half-open range [lo, hi) within n items.
func span(start, stop int64, n int) (int, int) {
	if start < 0 {
		start += int64(n)
	}
	if stop < 0 {
		stop += int64(n)
	}
	if start < 0 {
		start = 0
	}
	if stop >= int64(n) {
		stop = int64(n) - 1
	}
	if start > stop || start >= int64(n) {
		return 0, 0
	}
	return int(start), int(stop) + 1
}
