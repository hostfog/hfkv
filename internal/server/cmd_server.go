package server

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hostfog/hfkv/internal/store"
)

func init() {
	register("ping", -1, fNoLock|fPubSub, cmdPing)
	register("echo", 2, fNoLock, func(x *ctx) { x.w.Bulk(x.args[1]) })
	register("quit", -1, fNoLock|fNoAuth|fPubSub|fNoQueue, func(x *ctx) {})
	register("auth", -2, fNoLock|fNoAuth|fNoQueue, cmdAuth)
	register("hello", -1, fNoLock|fNoAuth|fNoQueue, cmdHello)
	register("select", 2, fNoLock, cmdSelect)
	register("swapdb", 3, 0, cmdSwapDB)
	register("flushdb", -1, 0, func(x *ctx) { x.db.Flush(); x.s.store.Dirty++; x.w.OK() })
	register("flushall", -1, 0, func(x *ctx) { x.s.store.Flush(); x.s.store.Dirty++; x.w.OK() })
	register("time", 1, fNoLock, cmdTime)
	register("lastsave", 1, fNoLock, func(x *ctx) { x.w.Int(x.s.lastSave.Load()) })
	register("save", 1, fNoLock|fNoQueue, cmdSave)
	register("bgsave", -1, fNoLock|fNoQueue, cmdBGSave)
	register("info", -1, fNoLock, cmdInfo)
	register("config", -2, fNoLock, cmdConfig)
	register("client", -2, fNoLock, cmdClient)
	register("command", -1, fNoLock, cmdCommand)
	register("reset", 1, fNoLock|fNoAuth|fPubSub|fNoQueue, cmdReset)
	register("shutdown", -1, fNoLock|fNoQueue, cmdShutdown)

	register("multi", 1, fNoLock|fNoQueue, cmdMulti)
	register("exec", 1, fNoLock|fNoQueue, cmdExec)
	register("discard", 1, fNoLock|fNoQueue, cmdDiscard)
	register("watch", -2, fNoLock|fNoQueue, cmdWatch)
	register("unwatch", 1, fNoLock|fNoQueue, func(x *ctx) { x.s.unwatch(x.c); x.w.OK() })
}

func cmdPing(x *ctx) {
	if x.w.Proto == 2 && x.s.pubsub.count(x.c) > 0 {
		x.w.Array(2)
		x.w.BulkString("pong")
		if len(x.args) > 1 {
			x.w.Bulk(x.args[1])
		} else {
			x.w.BulkString("")
		}
		return
	}
	switch len(x.args) {
	case 1:
		x.w.Simple("PONG")
	case 2:
		x.w.Bulk(x.args[1])
	default:
		x.w.Error("wrong number of arguments for 'ping' command")
	}
}

// AUTH password, or AUTH default password.
func cmdAuth(x *ctx) {
	if len(x.args) > 3 {
		x.fail(errSyntax)
		return
	}
	if x.s.cfg.Password == "" {
		x.w.Error("AUTH <password> called without any password configured for the default user. Are you sure your configuration is correct?")
		return
	}
	password := x.str(len(x.args) - 1)
	if (len(x.args) == 3 && x.str(1) != "default") || !auth(x.s, password) {
		x.w.Error("WRONGPASS invalid username-password pair or user is disabled.")
		return
	}
	x.c.authed = true
	x.w.OK()
}

// HELLO [protover [AUTH user pass] [SETNAME name]] switches between RESP2
// and RESP3.
func cmdHello(x *ctx) {
	if len(x.args) > 1 {
		v, err := strconv.Atoi(x.str(1))
		if err != nil {
			x.w.Error("Protocol version is not an integer or out of range")
			return
		}
		if v != 2 && v != 3 {
			x.w.Error("NOPROTO unsupported protocol version")
			return
		}
		for i := 2; i < len(x.args); i++ {
			switch {
			case x.is(i, "auth") && i+2 < len(x.args):
				if (x.str(i+1) != "default") || x.s.cfg.Password == "" || !auth(x.s, x.str(i+2)) {
					x.w.Error("WRONGPASS invalid username-password pair or user is disabled.")
					return
				}
				x.c.authed = true
				i += 2
			case x.is(i, "setname") && i+1 < len(x.args):
				x.c.name = x.str(i + 1)
				i++
			default:
				x.fail(errSyntax)
				return
			}
		}
	}
	if x.s.cfg.Password != "" && !x.c.authed {
		x.w.Error("NOAUTH HELLO must be called with the client already authenticated, otherwise the HELLO <proto> AUTH <user> <pass> option can be used to authenticate the client and select the RESP protocol version at the same time")
		return
	}
	if len(x.args) > 1 {
		x.w.Proto, _ = strconv.Atoi(x.str(1))
	}
	x.w.Map(7)
	x.w.BulkString("server")
	x.w.BulkString("hfkv")
	x.w.BulkString("version")
	x.w.BulkString(Version)
	x.w.BulkString("proto")
	x.w.Int(int64(x.w.Proto))
	x.w.BulkString("id")
	x.w.Int(x.c.id)
	x.w.BulkString("mode")
	x.w.BulkString("standalone")
	x.w.BulkString("role")
	x.w.BulkString("master")
	x.w.BulkString("modules")
	x.w.Array(0)
}

func cmdSelect(x *ctx) {
	n, err := strconv.Atoi(x.str(1))
	if err != nil {
		x.fail(errInt)
		return
	}
	if n < 0 || n >= len(x.s.store.DBs) {
		x.w.Error("DB index is out of range")
		return
	}
	x.c.db = n
	x.w.OK()
}

func cmdSwapDB(x *ctx) {
	a, ok1 := x.int(1)
	b, ok2 := x.int(2)
	if !ok1 || !ok2 {
		return
	}
	dbs := x.s.store.DBs
	if a < 0 || b < 0 || a >= int64(len(dbs)) || b >= int64(len(dbs)) {
		x.w.Error("DB index is out of range")
		return
	}
	x.s.store.Swap(int(a), int(b))
	x.w.OK()
}

func cmdTime(x *ctx) {
	now := time.Now()
	x.w.Strings([]string{strconv.FormatInt(now.Unix(), 10), strconv.Itoa(now.Nanosecond() / 1000)})
}

func cmdSave(x *ctx) {
	if x.s.cfg.DBFile == "" {
		x.w.Error("saving is disabled")
		return
	}
	if err := x.s.save(); err != nil {
		x.w.Error(err.Error())
		return
	}
	x.w.OK()
}

func cmdBGSave(x *ctx) {
	if x.s.cfg.DBFile == "" {
		x.w.Error("saving is disabled")
		return
	}
	if !x.s.bgsave() {
		x.w.Error("Background save already in progress")
		return
	}
	x.w.Simple("Background saving started")
}

func cmdShutdown(x *ctx) {
	x.w.OK()
	x.w.Flush()
	x.s.log.Print("shutdown requested by client")
	go func() {
		x.s.Close()
		os.Exit(0)
	}()
}

func cmdReset(x *ctx) {
	x.s.pubsub.unsubscribeAll(x.c)
	x.s.unwatch(x.c)
	x.c.inMulti, x.c.queued, x.c.multiErr = false, nil, false
	x.c.db, x.c.name = 0, ""
	x.c.authed = false
	x.w.Proto = 2
	x.w.Simple("RESET")
}

func cmdMulti(x *ctx) {
	if x.c.inMulti {
		x.w.Error("MULTI calls can not be nested")
		return
	}
	x.c.inMulti, x.c.queued, x.c.multiErr = true, nil, false
	x.w.OK()
}

func cmdDiscard(x *ctx) {
	if !x.c.inMulti {
		x.w.Error("DISCARD without MULTI")
		return
	}
	x.c.inMulti, x.c.queued = false, nil
	x.s.unwatch(x.c)
	x.w.OK()
}

// cmdExec runs the queued commands with the store locked, so no other
// client sees a half-finished transaction.
func cmdExec(x *ctx) {
	c, s := x.c, x.s
	if !c.inMulti {
		x.w.Error("EXEC without MULTI")
		return
	}
	queued := c.queued
	c.inMulti, c.queued = false, nil
	if c.multiErr {
		s.unwatch(c)
		x.w.Error("EXECABORT Transaction discarded because of previous errors.")
		return
	}

	s.store.Mu.Lock()
	if c.watch != nil && c.watch.Dirty {
		s.store.Mu.Unlock()
		s.unwatch(c)
		x.w.NullArray()
		return
	}
	x.w.Array(len(queued))
	for _, args := range queued {
		cmd := commands[strings.ToLower(string(args[0]))]
		qx := &ctx{s: s, c: c, args: args, w: x.w, inExec: true}
		switch {
		case cmd.flags&fBlocking != 0:
			cmd.run(qx) // runs once, without waiting
		case cmd.flags&fNoLock != 0:
			s.store.Mu.Unlock()
			cmd.run(qx)
			s.store.Mu.Lock()
		default:
			s.exec(qx, cmd)
		}
	}
	s.store.Mu.Unlock()
	s.unwatch(c)
}

func cmdWatch(x *ctx) {
	if x.c.inMulti {
		x.w.Error("WATCH inside MULTI is not allowed")
		return
	}
	s := x.s
	s.store.Mu.Lock()
	if x.c.watch == nil {
		x.c.watch = &store.Watch{}
	}
	for _, a := range x.args[1:] {
		k := dbKey{x.c.db, string(a)}
		s.store.AddWatch(k.db, k.key, x.c.watch)
		x.c.watched = append(x.c.watched, k)
	}
	s.store.Mu.Unlock()
	x.w.OK()
}

func (s *Server) unwatch(c *client) {
	if c.watch == nil {
		return
	}
	s.store.Mu.Lock()
	for _, k := range c.watched {
		s.store.RemoveWatch(k.db, k.key, c.watch)
	}
	s.store.Mu.Unlock()
	c.watch, c.watched = nil, nil
}

// cmdConfig supports CONFIG GET and SET for the settings clients commonly
// read, and RESETSTAT/REWRITE as no-ops.
func cmdConfig(x *ctx) {
	s := x.s
	switch {
	case x.is(1, "get") && len(x.args) >= 3:
		s.store.Mu.Lock()
		params := map[string]string{
			"bind":             host(s.cfg.Addr),
			"port":             port(s.cfg.Addr),
			"databases":        strconv.Itoa(len(s.store.DBs)),
			"maxmemory":        strconv.FormatInt(s.store.MaxMemory, 10),
			"maxmemory-policy": s.store.Policy.String(),
			"maxclients":       strconv.Itoa(s.cfg.MaxClients),
			"dir":              s.cfg.Dir,
			"dbfilename":       s.cfg.DBFile,
			"save":             saveSetting(s.cfg.SaveEvery),
			"appendonly":       "no",
			"requirepass":      "",
		}
		s.store.Mu.Unlock()
		var names []string
		for n := range params {
			for _, a := range x.args[2:] {
				if store.Match(strings.ToLower(string(a)), n) {
					names = append(names, n)
					break
				}
			}
		}
		sort.Strings(names)
		x.w.Map(len(names))
		for _, n := range names {
			x.w.BulkString(n)
			x.w.BulkString(params[n])
		}
	case x.is(1, "set") && len(x.args) >= 4 && len(x.args)%2 == 0:
		s.store.Mu.Lock()
		defer s.store.Mu.Unlock()
		for i := 2; i < len(x.args); i += 2 {
			val := x.str(i + 1)
			switch strings.ToLower(x.str(i)) {
			case "maxmemory":
				n, err := ParseSize(val)
				if err != nil {
					x.w.Error("CONFIG SET failed (possibly related to argument 'maxmemory') - " + err.Error())
					return
				}
				s.store.MaxMemory = n
			case "maxmemory-policy":
				p, ok := store.ParsePolicy(strings.ToLower(val))
				if !ok {
					x.w.Error("CONFIG SET failed (possibly related to argument 'maxmemory-policy') - argument(s) must be one of the following: " + strings.Join(policyNames(), ", "))
					return
				}
				s.store.Policy = p
			default:
				x.w.Error("Unknown option or number of arguments for CONFIG SET - '" + x.str(i) + "'")
				return
			}
		}
		x.w.OK()
	case x.is(1, "resetstat") || x.is(1, "rewrite"):
		x.w.OK()
	default:
		x.w.Error("unknown subcommand '" + x.str(1) + "'. Try CONFIG HELP.")
	}
}

func policyNames() []string {
	var out []string
	for p := store.NoEviction; p <= store.VolatileTTL; p++ {
		out = append(out, p.String())
	}
	return out
}

func saveSetting(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return fmt.Sprintf("%d 1", int(d.Seconds()))
}

func host(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		return addr[:i]
	}
	return addr
}

func port(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

// ParseSize reads sizes such as 0, 1048576, 512mb or 2gb.
func ParseSize(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		n      int64
	}{{"kb", 1 << 10}, {"mb", 1 << 20}, {"gb", 1 << 30}, {"k", 1000}, {"m", 1000 * 1000}, {"g", 1000 * 1000 * 1000}, {"b", 1}} {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSuffix(s, u.suffix), u.n
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}

// CLIENT ID | GETNAME | SETNAME name | LIST | INFO | KILL ID id | SETINFO ...
func cmdClient(x *ctx) {
	switch {
	case x.is(1, "id"):
		x.w.Int(x.c.id)
	case x.is(1, "getname"):
		if x.c.name == "" {
			x.w.Null()
		} else {
			x.w.BulkString(x.c.name)
		}
	case x.is(1, "setname") && len(x.args) == 3:
		if strings.ContainsAny(x.str(2), " \n") {
			x.w.Error("Client names cannot contain spaces, newlines or special characters.")
			return
		}
		x.c.name = x.str(2)
		x.w.OK()
	case x.is(1, "setinfo"):
		x.w.OK()
	case x.is(1, "info"):
		x.w.BulkString(x.s.clientLine(x.c))
	case x.is(1, "list"):
		x.s.mu.Lock()
		ids := make([]int64, 0, len(x.s.clients))
		for id := range x.s.clients {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		var b strings.Builder
		for _, id := range ids {
			b.WriteString(x.s.clientLine(x.s.clients[id]))
		}
		x.s.mu.Unlock()
		x.w.BulkString(b.String())
	case x.is(1, "kill") && len(x.args) == 4 && x.is(2, "id"):
		id, ok := x.int(3)
		if !ok {
			return
		}
		x.s.mu.Lock()
		target := x.s.clients[id]
		x.s.mu.Unlock()
		if target == nil {
			x.w.Int(0)
			return
		}
		target.conn.Close()
		x.w.Int(1)
	default:
		x.w.Error("unknown subcommand '" + x.str(1) + "'. Try CLIENT HELP.")
	}
}

// clientLine reads only fields that don't change after connecting, plus
// ones that are safe to read approximately.
func (s *Server) clientLine(c *client) string {
	return fmt.Sprintf("id=%d addr=%s laddr=%s name=%s age=%d db=%d cmd=%s\n",
		c.id, c.conn.RemoteAddr(), c.conn.LocalAddr(), c.name,
		int(time.Since(c.created).Seconds()), c.db, c.lastCmd)
}

// COMMAND, COMMAND COUNT and COMMAND DOCS. Command details aren't
// published; clients only use these to probe.
func cmdCommand(x *ctx) {
	switch {
	case len(x.args) == 1:
		x.w.Array(0)
	case x.is(1, "count"):
		x.w.Int(int64(len(commands)))
	case x.is(1, "docs"), x.is(1, "info"):
		x.w.Array(0)
	default:
		x.w.Error("unknown subcommand '" + x.str(1) + "'. Try COMMAND HELP.")
	}
}

func cmdInfo(x *ctx) {
	s := x.s
	section := "default"
	if len(x.args) > 1 {
		section = strings.ToLower(x.str(1))
	}
	want := func(name string) bool {
		return section == "default" || section == "all" || section == "everything" || section == name
	}

	var b strings.Builder
	line := func(k string, v any) { fmt.Fprintf(&b, "%s:%v\r\n", k, v) }
	head := func(name string) {
		if b.Len() > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString("# " + name + "\r\n")
	}

	if want("server") {
		head("Server")
		line("hfkv_version", Version)
		line("redis_version", "7.0.0") // what clients check for feature support
		line("redis_mode", "standalone")
		line("os", runtime.GOOS+" "+runtime.GOARCH)
		line("arch_bits", strconv.IntSize)
		line("go_version", runtime.Version())
		line("process_id", os.Getpid())
		line("tcp_port", port(s.cfg.Addr))
		line("uptime_in_seconds", int(time.Since(s.started).Seconds()))
		line("uptime_in_days", int(time.Since(s.started).Hours()/24))
	}
	if want("clients") {
		s.mu.Lock()
		n := len(s.clients)
		s.mu.Unlock()
		s.store.Mu.Lock()
		blocked := 0
		seen := map[*waiter]bool{}
		for _, ws := range s.waiters {
			for _, w := range ws {
				if !seen[w] {
					seen[w] = true
					blocked++
				}
			}
		}
		s.store.Mu.Unlock()
		head("Clients")
		line("connected_clients", n)
		line("maxclients", s.cfg.MaxClients)
		line("blocked_clients", blocked)
	}
	s.store.Mu.Lock()
	used, maxMem, policy := s.store.UsedMemory(), s.store.MaxMemory, s.store.Policy
	dirty, evicted, expired := s.store.Dirty, s.store.Evicted, s.store.Expired
	type dbInfo struct{ keys, expires int }
	var dbs []dbInfo
	for _, db := range s.store.DBs {
		dbs = append(dbs, dbInfo{db.Len(), db.Expiring()})
	}
	s.store.Mu.Unlock()

	if want("memory") {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		head("Memory")
		line("used_memory", used)
		line("used_memory_human", human(used))
		line("used_memory_rss", ms.Sys)
		line("used_memory_rss_human", human(int64(ms.Sys)))
		line("maxmemory", maxMem)
		line("maxmemory_human", human(maxMem))
		line("maxmemory_policy", policy)
	}
	if want("persistence") {
		head("Persistence")
		line("loading", 0)
		line("rdb_changes_since_last_save", dirty)
		line("rdb_bgsave_in_progress", b2i(s.saving.Load()))
		line("rdb_last_save_time", s.lastSave.Load())
		status := "ok"
		if e, _ := s.saveErr.Load().(string); e != "" {
			status = "err"
		}
		line("rdb_last_bgsave_status", status)
		line("aof_enabled", 0)
	}
	if want("stats") {
		head("Stats")
		line("total_connections_received", s.connections.Load())
		line("total_commands_processed", s.commands.Load())
		line("rejected_connections", s.rejected.Load())
		line("expired_keys", expired)
		line("evicted_keys", evicted)
		s.pubsub.mu.RLock()
		line("pubsub_channels", len(s.pubsub.channels))
		line("pubsub_patterns", len(s.pubsub.patterns))
		s.pubsub.mu.RUnlock()
	}
	if want("replication") {
		head("Replication")
		line("role", "master")
		line("connected_slaves", 0)
	}
	if want("keyspace") {
		head("Keyspace")
		for i, d := range dbs {
			if d.keys > 0 {
				fmt.Fprintf(&b, "db%d:keys=%d,expires=%d,avg_ttl=0\r\n", i, d.keys, d.expires)
			}
		}
	}
	x.w.BulkString(b.String())
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	f, suffix := float64(n), "KMGT"
	i := -1
	for f >= unit && i < len(suffix)-1 {
		f /= unit
		i++
	}
	return fmt.Sprintf("%.2f%c", f, suffix[i])
}
