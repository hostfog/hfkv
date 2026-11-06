package server

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hostfog/hfkv/internal/resp"
)

func start(t *testing.T, cfg Config) string {
	t.Helper()
	cfg.Log = log.New(io.Discard, "", 0)
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	s := New(cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	return ln.Addr().String()
}

// conn is a minimal client that returns replies as strings: simple and bulk
// strings as-is, integers as numbers, nil as "<nil>", errors as "ERR: msg"
// and arrays as "[a b c]".
type conn struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
	w *resp.Writer
}

func dial(t *testing.T, addr string) *conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &conn{t: t, c: c, r: bufio.NewReader(c), w: resp.NewWriter(c)}
}

func (c *conn) do(args ...string) string {
	c.t.Helper()
	c.w.Command(args...)
	c.w.Flush()
	c.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	return c.read()
}

func (c *conn) read() string {
	c.t.Helper()
	line, err := c.r.ReadString('\n')
	if err != nil {
		c.t.Fatal(err)
	}
	line = strings.TrimSuffix(line, "\r\n")
	switch line[0] {
	case '+', ':', ',':
		return line[1:]
	case '-':
		return "ERR: " + line[1:]
	case '_':
		return "<nil>"
	case '$':
		n, _ := strconv.Atoi(line[1:])
		if n < 0 {
			return "<nil>"
		}
		buf := make([]byte, n+2)
		io.ReadFull(c.r, buf)
		return string(buf[:n])
	case '*', '%', '~', '>':
		n, _ := strconv.Atoi(line[1:])
		if n < 0 {
			return "<nil>"
		}
		if line[0] == '%' {
			n *= 2
		}
		parts := make([]string, n)
		for i := range parts {
			parts[i] = c.read()
		}
		return "[" + strings.Join(parts, " ") + "]"
	}
	c.t.Fatalf("unexpected reply %q", line)
	return ""
}

func expect(t *testing.T, c *conn, want string, args ...string) {
	t.Helper()
	if got := c.do(args...); got != want {
		t.Errorf("%v = %q, want %q", args, got, want)
	}
}

func TestCommands(t *testing.T) {
	c := dial(t, start(t, Config{}))
	steps := []struct {
		want string
		args []string
	}{
		{"PONG", []string{"PING"}},
		{"OK", []string{"SET", "k", "v"}},
		{"v", []string{"GET", "k"}},
		{"<nil>", []string{"SET", "k", "x", "NX"}},
		{"v", []string{"SET", "k", "w", "GET"}},
		{"5", []string{"INCRBY", "n", "5"}},
		{"ERR: ERR value is not an integer or out of range", []string{"INCR", "k"}},
		{"3", []string{"RPUSH", "l", "a", "b", "c"}},
		{"[b c]", []string{"LRANGE", "l", "1", "-1"}},
		{"ERR: WRONGTYPE Operation against a key holding the wrong kind of value", []string{"GET", "l"}},
		{"2", []string{"HSET", "h", "f1", "1", "f2", "2"}},
		{"[1 <nil>]", []string{"HMGET", "h", "f1", "nope"}},
		{"2", []string{"SADD", "s", "a", "b"}},
		{"1", []string{"SISMEMBER", "s", "a"}},
		{"2", []string{"ZADD", "z", "1", "one", "2", "two"}},
		{"[one 1 two 2]", []string{"ZRANGE", "z", "0", "-1", "WITHSCORES"}},
		{"[two]", []string{"ZRANGEBYSCORE", "z", "(1", "+inf"}},
		{"1", []string{"EXPIRE", "k", "100"}},
		{"100", []string{"TTL", "k"}},
		{"string", []string{"TYPE", "k"}},
		{"6", []string{"DBSIZE"}},
		{"OK", []string{"SELECT", "1"}},
		{"0", []string{"DBSIZE"}},
		{"ERR: ERR unknown command 'NOPE', with args beginning with: ", []string{"NOPE"}},
	}
	for _, s := range steps {
		expect(t, c, s.want, s.args...)
	}
}

func TestTransactions(t *testing.T) {
	addr := start(t, Config{})
	c, other := dial(t, addr), dial(t, addr)
	expect(t, c, "OK", "MULTI")
	expect(t, c, "QUEUED", "SET", "a", "1")
	expect(t, c, "QUEUED", "INCR", "a")
	expect(t, c, "[OK 2]", "EXEC")

	expect(t, c, "OK", "WATCH", "a")
	expect(t, other, "3", "INCR", "a")
	expect(t, c, "OK", "MULTI")
	expect(t, c, "QUEUED", "SET", "a", "x")
	expect(t, c, "<nil>", "EXEC")
	expect(t, c, "3", "GET", "a")
}

func TestPubSub(t *testing.T) {
	addr := start(t, Config{})
	sub, pub := dial(t, addr), dial(t, addr)
	expect(t, sub, "[subscribe news 1]", "SUBSCRIBE", "news")
	expect(t, sub, "[psubscribe n* 2]", "PSUBSCRIBE", "n*")
	expect(t, pub, "2", "PUBLISH", "news", "hello")
	if got := sub.read(); got != "[message news hello]" {
		t.Errorf("got %q", got)
	}
	if got := sub.read(); got != "[pmessage n* news hello]" {
		t.Errorf("got %q", got)
	}
	if got := sub.do("GET", "x"); !strings.HasPrefix(got, "ERR: ERR Can't execute 'get'") {
		t.Errorf("GET while subscribed = %q", got)
	}
}

func TestBlockingPop(t *testing.T) {
	addr := start(t, Config{})
	waiter, pusher := dial(t, addr), dial(t, addr)
	done := make(chan string)
	go func() { done <- waiter.do("BLPOP", "jobs", "5") }()
	time.Sleep(100 * time.Millisecond)
	expect(t, pusher, "1", "RPUSH", "jobs", "j1")
	select {
	case got := <-done:
		if got != "[jobs j1]" {
			t.Errorf("BLPOP = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("BLPOP did not wake up")
	}
	expect(t, waiter, "<nil>", "BLPOP", "jobs", "0.1")
}

func TestAuth(t *testing.T) {
	c := dial(t, start(t, Config{Password: "secret"}))
	expect(t, c, "ERR: NOAUTH Authentication required.", "GET", "k")
	expect(t, c, "ERR: WRONGPASS invalid username-password pair or user is disabled.", "AUTH", "wrong")
	expect(t, c, "OK", "AUTH", "secret")
	expect(t, c, "<nil>", "GET", "k")
}

func TestRESP3(t *testing.T) {
	c := dial(t, start(t, Config{}))
	if got := c.do("HELLO", "3"); !strings.Contains(got, "proto 3") {
		t.Fatalf("HELLO 3 = %q", got)
	}
	expect(t, c, "<nil>", "GET", "missing")
	expect(t, c, "1", "HSET", "h", "f", "v")
	expect(t, c, "[f v]", "HGETALL", "h")
	expect(t, c, "1", "ZADD", "z", "1.5", "m")
	expect(t, c, "1.5", "ZSCORE", "z", "m")
	expect(t, c, "[[m 1.5]]", "ZRANGE", "z", "0", "-1", "WITHSCORES")
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Dir: dir, DBFile: "dump.hfkv", SaveEvery: time.Hour, Log: log.New(io.Discard, "", 0)}

	s := New(cfg)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go s.Serve(ln)
	c := dial(t, ln.Addr().String())
	expect(t, c, "OK", "SET", "kept", "yes")
	expect(t, c, "1", "RPUSH", "list", "a")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := New(cfg)
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	go s2.Serve(ln2)
	defer s2.Close()
	c2 := dial(t, ln2.Addr().String())
	expect(t, c2, "yes", "GET", "kept")
	expect(t, c2, "[a]", "LRANGE", "list", "0", "-1")
}

func TestPipelining(t *testing.T) {
	c := dial(t, start(t, Config{}))
	for i := 0; i < 1000; i++ {
		c.w.Command("INCR", "n")
	}
	c.w.Flush()
	var last string
	for i := 0; i < 1000; i++ {
		last = c.read()
	}
	if last != "1000" {
		t.Errorf("last reply %q", last)
	}
	expect(t, c, fmt.Sprint(1000), "GET", "n")
}
