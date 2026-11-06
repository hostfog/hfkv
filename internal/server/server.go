// Package server runs the hfkv network server: connections, command
// dispatch, transactions, pub/sub, blocking pops and snapshots.
package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hostfog/hfkv/internal/resp"
	"github.com/hostfog/hfkv/internal/store"
)

const Version = "1.0.0"

type Config struct {
	Addr       string
	Password   string
	Dir        string
	DBFile     string
	SaveEvery  time.Duration // 0 disables automatic snapshots
	Databases  int
	MaxMemory  int64
	Policy     store.Policy
	MaxClients int
	Log        *log.Logger
}

type Server struct {
	cfg   Config
	store *store.Store
	log   *log.Logger

	ln      net.Listener
	mu      sync.Mutex // guards clients
	clients map[int64]*client
	nextID  atomic.Int64
	closing atomic.Bool
	done    chan struct{}
	wg      sync.WaitGroup

	pubsub  *hub
	waiters map[dbKey][]*waiter // clients blocked in BLPOP and friends, guarded by store.Mu

	started     time.Time
	commands    atomic.Int64
	connections atomic.Int64
	rejected    atomic.Int64

	saving   atomic.Bool
	saveMu   sync.Mutex // one snapshot write at a time
	lastSave atomic.Int64
	saveErr  atomic.Value // string
}

type dbKey struct {
	db  int
	key string
}

func New(cfg Config) *Server {
	if cfg.Databases <= 0 {
		cfg.Databases = 16
	}
	if cfg.MaxClients <= 0 {
		cfg.MaxClients = 10000
	}
	if cfg.Log == nil {
		cfg.Log = log.New(os.Stderr, "", log.LstdFlags)
	}
	st := store.New(cfg.Databases)
	st.MaxMemory = cfg.MaxMemory
	st.Policy = cfg.Policy
	s := &Server{
		cfg:     cfg,
		store:   st,
		log:     cfg.Log,
		clients: map[int64]*client{},
		done:    make(chan struct{}),
		pubsub:  newHub(),
		waiters: map[dbKey][]*waiter{},
		started: time.Now(),
	}
	s.lastSave.Store(time.Now().Unix())
	return s
}

func (s *Server) dbPath() string { return filepath.Join(s.cfg.Dir, s.cfg.DBFile) }

// Load reads the snapshot file, if there is one.
func (s *Server) Load() error {
	if s.cfg.DBFile == "" {
		return nil
	}
	data, err := os.ReadFile(s.dbPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	start := time.Now()
	s.store.Mu.Lock()
	defer s.store.Mu.Unlock()
	if err := s.store.Restore(data); err != nil {
		return fmt.Errorf("loading %s: %w", s.dbPath(), err)
	}
	s.log.Printf("loaded %d keys from %s in %s", s.store.Keys(), s.dbPath(), time.Since(start).Round(time.Millisecond))
	return nil
}

// ListenAndServe accepts connections until Close is called.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	s.wg.Add(1)
	go s.cron()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.closing.Load() {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return err
		}
		s.connections.Add(1)
		c := newClient(s.nextID.Add(1), conn)
		s.mu.Lock()
		if len(s.clients) >= s.cfg.MaxClients {
			s.mu.Unlock()
			s.rejected.Add(1)
			c.w.Error("max number of clients reached")
			c.w.Flush()
			conn.Close()
			continue
		}
		s.clients[c.id] = c
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveClient(c)
		}()
	}
}

// Close stops accepting connections, disconnects clients and writes a
// final snapshot.
func (s *Server) Close() error {
	if !s.closing.CompareAndSwap(false, true) {
		return nil
	}
	close(s.done)
	s.mu.Lock()
	if s.ln != nil {
		s.ln.Close()
	}
	for _, c := range s.clients {
		c.conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	if s.cfg.DBFile != "" && s.cfg.SaveEvery > 0 {
		return s.save()
	}
	return nil
}

// Addr is the address the server is listening on.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ln.Addr()
}

// cron expires keys and takes automatic snapshots.
func (s *Server) cron() {
	defer s.wg.Done()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-tick.C:
		}
		s.store.Mu.Lock()
		s.store.Tick()
		dirty := s.store.Dirty
		s.store.Mu.Unlock()

		if s.cfg.SaveEvery > 0 && dirty > 0 && s.cfg.DBFile != "" &&
			time.Since(time.Unix(s.lastSave.Load(), 0)) >= s.cfg.SaveEvery {
			s.bgsave()
		}
	}
}

// snapshot encodes the data set while holding the lock, so the copy is
// consistent; writing it to disk happens afterwards.
func (s *Server) snapshot() ([]byte, int64) {
	var buf bytes.Buffer
	s.store.Mu.Lock()
	defer s.store.Mu.Unlock()
	s.store.Snapshot(&buf)
	return buf.Bytes(), s.store.Dirty
}

func (s *Server) save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	data, dirty := s.snapshot()
	if err := s.store.SaveFile(s.dbPath(), data); err != nil {
		s.saveErr.Store(err.Error())
		s.log.Printf("saving %s: %v", s.dbPath(), err)
		return err
	}
	s.saveErr.Store("")
	s.store.Mu.Lock()
	s.store.Dirty -= dirty
	s.store.Mu.Unlock()
	s.lastSave.Store(time.Now().Unix())
	return nil
}

// bgsave saves in the background; it reports false if a save is running.
func (s *Server) bgsave() bool {
	if !s.saving.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer s.saving.Store(false)
		start := time.Now()
		if s.save() == nil {
			s.log.Printf("saved %s in %s", s.dbPath(), time.Since(start).Round(time.Millisecond))
		}
	}()
	return true
}

func (s *Server) serveClient(c *client) {
	defer func() {
		s.pubsub.unsubscribeAll(c)
		s.unwatch(c)
		s.mu.Lock()
		delete(s.clients, c.id)
		s.mu.Unlock()
		c.conn.Close()
	}()

	for {
		args, err := c.r.ReadCommand()
		if err != nil {
			if errors.Is(err, resp.ErrProtocol) {
				c.lock()
				c.w.Error("Protocol error: " + strings.TrimPrefix(err.Error(), "protocol error: "))
				c.w.Flush()
				c.unlock()
			} else if !errors.Is(err, io.EOF) && !s.closing.Load() && !isClosed(err) {
				s.log.Printf("client %s: %v", c.conn.RemoteAddr(), err)
			}
			return
		}
		if len(args) == 0 {
			continue
		}
		s.commands.Add(1)
		c.lock()
		quit := s.dispatch(c, args)
		if c.r.Buffered() == 0 || quit {
			if err := c.w.Flush(); err != nil {
				c.unlock()
				return
			}
		}
		c.unlock()
		if quit {
			return
		}
	}
}

func isClosed(err error) bool {
	return errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "connection reset")
}
