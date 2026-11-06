// Command hfkv is a key-value and cache server that speaks the Redis
// protocol.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/hostfog/hfkv/internal/server"
	"github.com/hostfog/hfkv/internal/store"
)

func main() {
	log.SetFlags(log.LstdFlags)
	var (
		bind      = flag.String("bind", envOr("HFKV_BIND", "127.0.0.1"), "address to listen on (0.0.0.0 for all)")
		port      = flag.Int("port", envInt("HFKV_PORT", 6379), "TCP port")
		password  = flag.String("requirepass", os.Getenv("HFKV_PASSWORD"), "password clients must send with AUTH")
		dir       = flag.String("dir", envOr("HFKV_DIR", "."), "folder for the snapshot file")
		dbfile    = flag.String("dbfilename", "dump.hfkv", "snapshot file name")
		save      = flag.Int("save", envInt("HFKV_SAVE", 60), "seconds between snapshots when data changed (0 = never save)")
		databases = flag.Int("databases", 16, "number of databases")
		maxmem    = flag.String("maxmemory", envOr("HFKV_MAXMEMORY", "0"), "memory limit for data, e.g. 256mb or 2gb (0 = no limit)")
		policy    = flag.String("maxmemory-policy", envOr("HFKV_MAXMEMORY_POLICY", "noeviction"), "what to do at the limit: noeviction, allkeys-lru, volatile-lru, allkeys-random, volatile-random, volatile-ttl")
		maxconns  = flag.Int("maxclients", 10000, "maximum connected clients")
		version   = flag.Bool("version", false, "print the version and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: hfkv [options]\n\nA key-value and cache server that speaks the Redis protocol.\n\nOptions:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *version {
		fmt.Println("hfkv", server.Version)
		return
	}
	limit, err := server.ParseSize(*maxmem)
	if err != nil {
		log.Fatalf("maxmemory: %v", err)
	}
	pol, ok := store.ParsePolicy(*policy)
	if !ok {
		log.Fatalf("unknown maxmemory-policy %q", *policy)
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		log.Fatal(err)
	}
	dbFile := *dbfile
	if *save <= 0 {
		dbFile = ""
	}

	addr := net.JoinHostPort(*bind, strconv.Itoa(*port))
	srv := server.New(server.Config{
		Addr:       addr,
		Password:   *password,
		Dir:        *dir,
		DBFile:     dbFile,
		SaveEvery:  time.Duration(*save) * time.Second,
		Databases:  *databases,
		MaxMemory:  limit,
		Policy:     pol,
		MaxClients: *maxconns,
	})
	if err := srv.Load(); err != nil {
		log.Fatal(err)
	}
	if *password == "" && !isLoopback(*bind) {
		log.Printf("WARNING: listening on %s without a password. Anyone who can reach this port can read and change your data. Set --requirepass.", *bind)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		log.Print("shutting down")
		if err := srv.Close(); err != nil {
			log.Printf("final save failed: %v", err)
			os.Exit(1)
		}
		os.Exit(0)
	}()

	log.Printf("hfkv %s ready on %s", server.Version, addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
	select {} // Close is running; it exits the process
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}
