package server

import (
	"net"
	"sync"
	"time"

	"github.com/hostfog/hfkv/internal/resp"
	"github.com/hostfog/hfkv/internal/store"
)

type client struct {
	id      int64
	conn    net.Conn
	r       *resp.Reader
	w       *resp.Writer
	wmu     sync.Mutex // the writer is shared with pub/sub deliveries
	created time.Time

	// Only touched by the client's own goroutine.
	db       int
	authed   bool
	name     string
	lastCmd  string
	inMulti  bool
	queued   [][][]byte
	multiErr bool
	watch    *store.Watch
	watched  []dbKey

	// Guarded by the hub's lock.
	channels map[string]bool
	patterns map[string]bool
}

func newClient(id int64, conn net.Conn) *client {
	return &client{
		id:       id,
		conn:     conn,
		r:        resp.NewReader(conn),
		w:        resp.NewWriter(conn),
		created:  time.Now(),
		channels: map[string]bool{},
		patterns: map[string]bool{},
	}
}

func (c *client) lock()   { c.wmu.Lock() }
func (c *client) unlock() { c.wmu.Unlock() }

func (c *client) subscribed() int { return len(c.channels) + len(c.patterns) }
