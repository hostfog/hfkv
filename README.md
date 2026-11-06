<p align="center">
  <img src="docs/logo.png" width="88" alt="hfkv logo">
</p>

<h1 align="center">hfkv</h1>

<p align="center">
  A fast key-value and cache server that speaks the Redis protocol.<br>
  One small binary, pure Go, no dependencies.
</p>

<p align="center">
  <a href="https://github.com/hostfog/hfkv/releases/latest"><img src="https://img.shields.io/github/v/release/hostfog/hfkv?color=d87858&label=release" alt="Latest release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/licence-MIT-d87858" alt="MIT licence"></a>
  <img src="https://img.shields.io/badge/go-1.22+-00ADD8" alt="Go 1.22+">
  <img src="https://img.shields.io/badge/runs%20on-Linux%20%7C%20macOS%20%7C%20Windows%20%7C%20Docker-555" alt="Platforms">
</p>

By [HostFog Ltd](https://hostfog.com). hfkv stores strings, lists, hashes,
sets and sorted sets in memory, with expiry, transactions, pub/sub, blocking
queues and snapshots to disk. Any Redis client library or tool works with it
unchanged: point it at hfkv's address.

```
$ hfkv
2026/11/06 18:00:00 hfkv 1.0.0 ready on 127.0.0.1:6379

$ hfkv-cli set greeting "hello world"
OK
$ hfkv-cli get greeting
"hello world"
```

## Contents

- [Install](#install)
- [Run it](#run-it)
- [Configuration](#configuration)
- [Use it from your code](#use-it-from-your-code)
- [Commands](#commands)
- [Persistence](#persistence)
- [Memory limit and eviction](#memory-limit-and-eviction)
- [Security](#security)
- [Performance](#performance)
- [Building from source](#building-from-source)

## Install

### Download

Get the archive for your system from the
[latest release](https://github.com/hostfog/hfkv/releases/latest). Each one
contains `hfkv` (the server) and `hfkv-cli` (the command line client).

| System | File |
| --- | --- |
| Linux (x86-64 / ARM) | `hfkv-<version>-linux-amd64.tar.gz` / `-linux-arm64.tar.gz` |
| macOS (Intel / Apple silicon) | `hfkv-<version>-darwin-amd64.tar.gz` / `-darwin-arm64.tar.gz` |
| Windows (x86-64 / ARM) | `hfkv-<version>-windows-amd64.zip` / `-windows-arm64.zip` |
| FreeBSD | `hfkv-<version>-freebsd-amd64.tar.gz` |

**Linux and macOS**

```bash
tar -xzf hfkv-*-linux-amd64.tar.gz
sudo mv hfkv-*/hfkv hfkv-*/hfkv-cli /usr/local/bin/
hfkv
```

On macOS, if you downloaded with a browser, run
`xattr -d com.apple.quarantine /usr/local/bin/hfkv*` once.

**Windows**

1. Unzip `hfkv-<version>-windows-amd64.zip`, for example into `C:\Program Files\hfkv`.
2. Add that folder to your `PATH` (*Settings → System → About → Advanced
   system settings → Environment Variables*).
3. Open a new terminal and run `hfkv`. Allow it through the firewall only if
   other computers need to connect.

Each release has a `SHA256SUMS` file to check your download.

### Docker

```bash
docker run -d --name hfkv -p 6379:6379 -v hfkv-data:/data \
  -e HFKV_PASSWORD=change-me ghcr.io/hostfog/hfkv
```

The container listens on all interfaces, so always set `HFKV_PASSWORD`
when the port is reachable from outside.

### With Go

```bash
go install github.com/hostfog/hfkv/cmd/hfkv@latest
go install github.com/hostfog/hfkv/cmd/hfkv-cli@latest
```

## Run it

### In a terminal

```bash
hfkv                                   # 127.0.0.1:6379, snapshots in the current folder
hfkv --port 6380 --dir ~/hfkv-data     # another port and data folder
hfkv --save 0 --maxmemory 512mb --maxmemory-policy allkeys-lru   # a pure cache
```

Stop it with Ctrl+C; it saves a final snapshot first.

### As a service on Linux (systemd)

```bash
sudo useradd --system --home /var/lib/hfkv --create-home hfkv
sudo cp hfkv hfkv-cli /usr/local/bin/
sudo curl -fLo /etc/systemd/system/hfkv.service \
  https://raw.githubusercontent.com/hostfog/hfkv/main/deploy/hfkv.service
sudo systemctl enable --now hfkv
hfkv-cli ping
```

Logs: `journalctl -u hfkv -f`. To set a password or listen on all
interfaces, edit the `Environment=` lines in the unit file and run
`sudo systemctl daemon-reload && sudo systemctl restart hfkv`.

### Uninstall

Stop it, then delete the binaries and the data folder. For the service:
`sudo systemctl disable --now hfkv`, then delete
`/etc/systemd/system/hfkv.service`, `/usr/local/bin/hfkv*` and
`/var/lib/hfkv`.

## Configuration

Every option can be given as a flag; the common ones also as an
environment variable.

| Flag | Environment | Default | |
| --- | --- | --- | --- |
| `--bind` | `HFKV_BIND` | `127.0.0.1` | address to listen on; `0.0.0.0` for all interfaces |
| `--port` | `HFKV_PORT` | `6379` | TCP port |
| `--requirepass` | `HFKV_PASSWORD` | | password clients must send with `AUTH` |
| `--dir` | `HFKV_DIR` | `.` | folder for the snapshot file |
| `--dbfilename` | | `dump.hfkv` | snapshot file name |
| `--save` | `HFKV_SAVE` | `60` | seconds between snapshots when data changed; `0` turns saving off |
| `--maxmemory` | `HFKV_MAXMEMORY` | `0` | memory limit for data, such as `256mb` or `2gb`; `0` is no limit |
| `--maxmemory-policy` | `HFKV_MAXMEMORY_POLICY` | `noeviction` | what happens at the limit (see below) |
| `--databases` | | `16` | number of databases (`SELECT 0` to `15`) |
| `--maxclients` | | `10000` | maximum connected clients |

`maxmemory` and `maxmemory-policy` can also be changed while running with
`CONFIG SET`.

## Use it from your code

Use your language's usual Redis client and connect to hfkv's address.

**Node.js** (`npm install redis`)

```js
import { createClient } from 'redis';

const db = await createClient({ url: 'redis://localhost:6379' }).connect();
await db.set('visits', 0);
await db.incr('visits');
```

**Python** (`pip install redis`)

```python
import redis

db = redis.Redis(host="localhost", port=6379, decode_responses=True)
db.hset("user:1", mapping={"name": "Param", "city": "London"})
print(db.hgetall("user:1"))
```

**Go** (`go get github.com/redis/go-redis/v9`)

```go
db := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
db.Set(ctx, "session:42", "data", time.Hour)
```

**PHP** (`composer require predis/predis`)

```php
$db = new Predis\Client('tcp://127.0.0.1:6379');
$db->lpush('jobs', 'send-email');
```

Both RESP2 and RESP3 are supported, so new and old client versions work.
Caching and sessions in frameworks such as Laravel, Django, Rails and Express
only need the host and port changed. Job queues built on Lua scripts, such as
BullMQ and Laravel queues, need `EVAL`, which isn't supported yet.

## Commands

| Group | Commands |
| --- | --- |
| Keys | `DEL` `UNLINK` `EXISTS` `TYPE` `KEYS` `SCAN` `RANDOMKEY` `RENAME` `RENAMENX` `MOVE` `COPY` `TOUCH` `DBSIZE` |
| Expiry | `EXPIRE` `PEXPIRE` `EXPIREAT` `PEXPIREAT` (with `NX` `XX` `GT` `LT`) `TTL` `PTTL` `EXPIRETIME` `PEXPIRETIME` `PERSIST` |
| Strings | `GET` `SET` (with `NX` `XX` `GET` `EX` `PX` `EXAT` `PXAT` `KEEPTTL`) `SETNX` `SETEX` `PSETEX` `GETSET` `GETDEL` `GETEX` `MGET` `MSET` `MSETNX` `APPEND` `STRLEN` `GETRANGE` `SETRANGE` `INCR` `DECR` `INCRBY` `DECRBY` `INCRBYFLOAT` |
| Lists | `LPUSH` `RPUSH` `LPUSHX` `RPUSHX` `LPOP` `RPOP` `LLEN` `LRANGE` `LINDEX` `LSET` `LREM` `LTRIM` `LINSERT` `LPOS` `LMOVE` `RPOPLPUSH` `BLPOP` `BRPOP` `BLMOVE` `BRPOPLPUSH` |
| Hashes | `HSET` `HMSET` `HSETNX` `HGET` `HMGET` `HDEL` `HLEN` `HSTRLEN` `HEXISTS` `HGETALL` `HKEYS` `HVALS` `HINCRBY` `HINCRBYFLOAT` `HSCAN` `HRANDFIELD` |
| Sets | `SADD` `SREM` `SMEMBERS` `SISMEMBER` `SMISMEMBER` `SCARD` `SPOP` `SRANDMEMBER` `SMOVE` `SINTER` `SUNION` `SDIFF` `SINTERSTORE` `SUNIONSTORE` `SDIFFSTORE` `SINTERCARD` `SSCAN` |
| Sorted sets | `ZADD` (with `NX` `XX` `GT` `LT` `CH` `INCR`) `ZINCRBY` `ZREM` `ZCARD` `ZSCORE` `ZMSCORE` `ZRANK` `ZREVRANK` `ZCOUNT` `ZRANGE` (with `BYSCORE` `REV` `LIMIT` `WITHSCORES`) `ZREVRANGE` `ZRANGEBYSCORE` `ZREVRANGEBYSCORE` `ZREMRANGEBYRANK` `ZREMRANGEBYSCORE` `ZPOPMIN` `ZPOPMAX` `ZSCAN` |
| Transactions | `MULTI` `EXEC` `DISCARD` `WATCH` `UNWATCH` |
| Pub/sub | `SUBSCRIBE` `UNSUBSCRIBE` `PSUBSCRIBE` `PUNSUBSCRIBE` `PUBLISH` `PUBSUB` |
| Connection | `PING` `ECHO` `AUTH` `HELLO` `SELECT` `CLIENT` `RESET` `QUIT` |
| Server | `INFO` `CONFIG GET/SET` `SAVE` `BGSAVE` `LASTSAVE` `FLUSHDB` `FLUSHALL` `SWAPDB` `TIME` `COMMAND` `SHUTDOWN` |

Every command runs on its own, so no other client sees it half done. Not
included yet: streams, Lua scripting, replication and clustering.

## Persistence

hfkv keeps all data in memory and writes a snapshot of it to
`<dir>/dump.hfkv`:

- every `--save` seconds (60 by default) when something has changed,
- when you run `SAVE` or `BGSAVE`,
- when it shuts down.

On start it loads the snapshot back. A snapshot is written to a temporary
file and renamed into place, so a crash mid-save never damages the last good
one, and a checksum stops a damaged file from loading. Changes made after the
last snapshot are lost if the machine itself crashes; lower `--save` if that
matters to you, or use `--save 0` for a cache that doesn't need to survive
restarts.

Back up by copying `dump.hfkv` while hfkv runs.

## Memory limit and eviction

Set `--maxmemory` to cap the memory used by data. When it's reached:

| Policy | Behaviour |
| --- | --- |
| `noeviction` | writes fail with an `OOM` error; reads still work |
| `allkeys-lru` | removes the least recently used keys (the usual choice for a cache) |
| `volatile-lru` | the same, but only among keys with an expiry |
| `allkeys-random` | removes random keys |
| `volatile-random` | removes random keys that have an expiry |
| `volatile-ttl` | removes the keys closest to expiring |

Eviction samples a few keys at a time, so it's fast and close to exact LRU.
`INFO memory` shows `used_memory` and `INFO stats` shows `evicted_keys`.

## Security

- hfkv listens on `127.0.0.1` by default, so only programs on the same
  machine can connect.
- Before listening on other interfaces (`--bind 0.0.0.0`), set a password
  with `--requirepass` or `HFKV_PASSWORD`. hfkv prints a warning if you don't.
- Traffic isn't encrypted. Across the internet, use a private network, a VPN
  or an SSH tunnel.
- Report security problems to [security@hostfog.com](mailto:security@hostfog.com).

## Performance

All commands share one lock, which keeps each command atomic, while every
client is served by its own goroutine. On a 2-core cloud server,
`redis-benchmark` measures about 100,000 requests per second for single
commands (`SET`, `GET`, `INCR`, `LPUSH`, `HSET`, `ZADD`) and about 600,000
per second for `SET` with pipelining (`-P 16`). Check your own hardware with:

```bash
redis-benchmark -q -n 100000 -t set,get,incr,lpush,hset
```

## Building from source

Requires Go 1.22 or newer.

```bash
git clone https://github.com/hostfog/hfkv
cd hfkv
go build ./cmd/hfkv
go build ./cmd/hfkv-cli
go test -race ./...
```

```
cmd/hfkv          the server
cmd/hfkv-cli      the command line client
internal/resp     the wire protocol (RESP2 and RESP3)
internal/store    data types, expiry, eviction and snapshots
internal/server   connections, commands, transactions, pub/sub
deploy/           systemd unit
```

Issues and pull requests are welcome.

## Licence

MIT © 2026 HostFog Ltd. hfkv is an independent project and isn't affiliated
with Redis Ltd; Redis is a trademark of Redis Ltd.
