# PSU PS2 server: Go rewrite plan

A single static Go binary that replaces prettystellaruniverse's three Python services and the OpenLobby
SSLv3 stunnel image. It ships as a Docker Compose stack with PostgreSQL, and there's no DNS
(revive-dns does that).

It uses two third-party Go modules, both justified:
- `github.com/driftnet-io/insecure-tls`: SSLv3 server, see below.
- `github.com/jackc/pgx/v5`: Postgres, same as angelware.

Everything else is stdlib.

## What we are replacing

| Python (prettystellaruniverse v0.1.1) | Lines | Go |
|---|---|---|
| `server/psu/*.py` (patch, login, game and 35 modules) | 11.9k | `cmd/psud` + `internal/*` |
| `openlobby-ssl3` image (OpenSSL 1.1.1 + stunnel, `:latest`) | external | `internal/ssl3` (in-process) |
| `openlobby` dns image | external | dropped (revive-dns) |
| SQLite (`state/psu.db`) | — | `internal/store` on PostgreSQL (pgx) |
| `olport.py` / `adminport.py` / `gamelink.py` (OpenLobby panel + regapi) | 1.2k | small loopback admin API + `psuctl` |
| `tools/testplay.py`, `tools/bot.py`, my `loadtest.py` | 2.4k | `cmd/psubot` (load + scenario), testplay kept as an acceptance test |
| `extract/` (disc → `data/`) | 9k | **kept in Python for now** (phase 6 is optional) |
| `setup.py`, `hostsetup/` (UPnP, wizard) | 3.5k | dropped |

The server only reads the built `data/` directory: `nbl/*.nbl` (served as is), `gamedata/*.json` (15
tables) and `captures/*.bin`. That directory is the boundary between the extractor and the server, so the
server can be rewritten without touching extraction.

## Conventions (same as angelware / ps2-ue2-dedicated)

- `module github.com/princebyyourlogic/gurhal`, `go 1.27.4`, with the toolchain pinned the way
  `ps2-ue2-dedicated/python/tools/gr2_go.py` does it. A cached copy already exists at
  `ps2-ue2-dedicated/work/.go-toolchains/go1.27.4-linux-amd64`. Go is not on this machine's PATH.
- `cmd/<name>/main.go` only parses config and wires services. Everything else goes in `internal/`.
- Config comes from env (`PSU_*`), loaded by `env(key, def)` + `loadConfig()` that returns an error.
- Logging is `log/slog` JSON to stdout, with `log.With("svc", ...)` per service.
- Shutdown: `signal.NotifyContext`, and every `Serve(ctx, ln)` closes its listener when ctx ends.
- Dockerfile: `CGO_ENABLED=0 -trimpath -ldflags="-s -w"`, `FROM scratch`, `USER 65534`.
- `SPDX-License-Identifier: GPL-3.0-or-later` on every file. This is a port of GPL-3.0 code, so it stays
  GPL. EGS (AGPL) stays out of the server.
- `internal/archtest` enforces the layering below, like UE2's `layers_test.go`.

## Process and ports

One process, `psud`, with all listeners sharing one in-memory world:

| Listener | Port | Transport |
|---|---|---|
| patch PSU / AotI | 11110 / 11140 | plain TCP |
| login PSU / AotI | 12110 / 12140 | SSLv3 RC4-SHA (`internal/ssl3`) |
| game | 12061 | SSLv3 RC4-SHA |
| plain login / game (tests only, off by default) | 22110 / 22140 / 22061 | plain TCP, loopback |
| admin API | 22190 | HTTP, loopback, token |

Benefits of one process:
- Login hands the client a key and the game address in 0x0216, and the game redeems the key from the
  same in-memory registry (angelware's `session.Registry` pattern). There is no cross-process database
  handoff.
- One binary to run, one systemd unit, one log stream.

The plain-port option exists so `tools/testplay.py` and the load test can run against `psud` unchanged.

## Package layout

```
cmd/psud/              config, listeners, wiring
cmd/psuctl/            accounts (create, reset, ban), news, events: talks to the admin API
cmd/psubot/            protocol bot: load test (lobby movers, mission loops) and scenario checks

internal/wire/         frame (u32 size | u16 BE cmd | u8 channel | u8 0), Reader/Writer helpers,
                       the shared 0x24-byte body header, 0x0B03/0x0B05 fragmentation
internal/ssl3/         thin wrapper over insecure-tls: SSLv3-only RC4-SHA listener config, cert minting
internal/netx/         accept loop, per-connection Conn (reader + writer goroutine, bounded queue),
                       idle timeouts, max frame size, per-IP limits, traffic recorder
internal/disc/         game profiles (psu, aoti): ports, digests, per-disc table differences
internal/gamedata/     loads data/ once into immutable typed tables + NBL index; shared read-only
internal/rules/        pure game logic: growth/EXP, stats, combat math, drops, econ, synthesis,
                       grind, items, PA levels. No I/O, no wire, no goroutines.
internal/store/        accounts, characters, cards, mail, blacklist, bans, clears, help threads
internal/patch/        patch service
internal/login/        login service, sign-in throttling
internal/game/         session state machine and handlers, world registry, zone actors,
                       parties, missions/instances, shops, social, board, gm, season, events
internal/admin/        loopback admin HTTP API + status.json
internal/archtest/     layering test
```

Layering, enforced by archtest:
- `wire`, `ssl3`, `netx`, `gamedata` and `rules` never import `game`, `login` or `store`.
- `rules` imports only `gamedata` and stdlib. That keeps every formula unit-testable and
  diff-testable against the Python.
- Services (`patch`, `login`, `game`) never import each other. They share `store` and a small `auth`
  key registry.

## Concurrency model

The Python server runs everything on one asyncio loop. My load test showed it tops out at about 200
players in one zone, and memory grows without bound because relays queue to slow clients. The Go design
splits the work along the game's own boundaries:

**Per connection, two goroutines** (angelware's `lobby.Conn` pattern):
- The reader goroutine runs the session: it decodes frames and dispatches handlers.
- The writer goroutine owns the socket write side and the fragment sender. A large payload goes out as
  0x0B03 chunks, each waiting for its 0x0B05 ack. Acks come in on the reader and are forwarded to the
  writer over a channel, so a download never blocks the session or anyone relaying to it. Python needs
  `relay()` fire-and-forget tasks for this; in Go it falls out of the structure.
- `Send(pkt)` (replies) blocks until there's room in the queue, with a deadline. `Relay(pkt)` (other
  players' movement and chat) is a non-blocking try. If a client's bounded queue (default 256) stays
  full, the client is **disconnected**, not silently dropped, because missing state packets would
  desync it. This is the backpressure the Python lacks.

**One goroutine per zone** (an actor): each `(universe, quest, zone)` lobby, and each mission instance
(`(quest, zone, party owner)`).
- It owns everything in that zone: members and LIDs, positions, enemies and their HP and statuses,
  ground drops, instance flags/object states, and a timer heap for buffs, status ticks and PP regen.
  That one heap replaces Python's task-per-player `watch_buffs` and task-per-enemy `watch_enemy`.
- Sessions talk to it through a buffered inbox (`Enter`, `Leave`, `Move`, `Chat`, `Attack`,
  `Pickup`, ...). The zone answers by calling `Send`/`Relay` on member Conns.
- No locks on zone state. The goroutine starts on first enter and exits when the zone is empty, which
  replaces `combat.clear_zone`, `drops.clear` and `instance.clear`.
- Fan-out builds the packet once when the bytes are the same for every recipient. That covers movement
  (0x0102 ch1) and chat, the hot path at about 4 Hz per player. It builds per recipient only where the
  body really differs (`user_bin` per recipient disc).
- Zones are independent, so they spread across cores. The single-core ceiling only applies within one
  crowded zone, plus a per-lobby player cap (`PSU_ZONE_CAP`).

**Shared registries** are small, low-contention maps behind a `sync.RWMutex`: `world` (gid → player,
name lookup, universe population), `parties` and `auth` keys. Nothing hot goes through them.

**Persistence** goes through a save queue goroutine in front of the pgx pool. Sessions hand it an
immutable character snapshot. It coalesces writes per gid (the last one wins within ~1 s), so a burst
of item pickups is one `UPDATE`, and the game never waits on the database. Logout and shutdown flush
synchronously. Reads (login, character list) go straight to the pool.

**Background jobs** each get a goroutine under the root ctx: status.json, board, news ticker, events
calendar, log rotation.

**Traffic logging** is off by default (`PSU_TRAFFIC_LOG=1` turns it on per session or globally). An async
writer gets a bounded channel; when it's full, records are dropped and counted, so it never blocks the
game. Files rotate by size and age. In Python this was half the CPU and up to 16 GB/hour.

## Store (PostgreSQL)

This follows angelware's `internal/store`:
- A `Store` interface plus two implementations: `Memory` for tests and quick local runs, and
  `Postgres` on a `pgxpool`.
- The schema is `//go:embed schema.sql`, applied idempotently at startup.
- Unique violations (`23505`) map to `ErrExists`.

Instead of a migration library, a `schema_migrations` table plus numbered embedded files
(`migrations/0001_init.sql`, ...) are applied in one transaction at startup. That's about 40 lines, and
it covers schema changes after the server goes live.

Tables, carried over from the Python schema:

| Table | Columns | Notes |
|---|---|---|
| `accounts` | `gid` PK, `username` unique (citext-style lower index), `pw_hash`, `created`, `last_login`, `flags` | |
| `characters` | `(gid, slot)` PK, `name`, `raw bytea` (the client's record), `state jsonb` (progression, inventory, storage), `created`, `updated` | |
| `bans`, `account_resets` | | |
| `cards`, `mail`, `blacklist` | | social |
| `clears` | | mission clear times and ranks, indexed `(quest, secs)` for the board |
| `help_threads`, `help_messages` | | |
| `news`, `events` | | operator content, editable via `psuctl` instead of files on disk |

- **Login keys** stay in memory, since login and game share the process.
- **Passwords** use PBKDF2-SHA256 (`crypto/pbkdf2`, stdlib since Go 1.24) with a per-account salt.
- **Trades and player-shop purchases** touch two characters, so each is one transaction with
  `SELECT ... FOR UPDATE` on both rows in gid order.
- **Postgres outages** don't kill sessions: the save queue keeps the latest snapshot and retries. Login
  fails cleanly while the database is down.
- **No SQLite import**, since nobody hosts the Python version.

## Docker

Like angelware and ps2-ue2-dedicated: a `Makefile` front end and `compose.yaml` with `.env`.

```
compose.yaml
  postgres   postgres:18, named volume, NOT published (internal network only), pg_isready healthcheck
  psud       built from Dockerfile (golang:1.27.4-alpine → scratch, USER 65534),
             depends_on postgres healthy, publishes 11110 11140 12110 12140 12061,
             mounts ./data:/data:ro (built game data) and a small volume for the SSL cert,
             healthcheck: `psud health` (scratch has no shell, so the binary checks itself)
  extract    profile "extract": the existing Python extractor image (+ erlang for the EGS step),
             `make import ISO=...` writes ./data once
Makefile
  make import ISO=/path/psu.iso [AOTI=/path/aoti.iso]   build ./data from the discs
  make up / down / logs / status / psql / backup        backup = pg_dump to ./backups
  make test / vet / fmt                                  via the pinned toolchain
.env.example
  PSU_GAME_ADDR (public IPv4 handed out in 0x0216), POSTGRES_PASSWORD, PSU_ADMIN_TOKEN, PSU_MOTD,
  PSU_UNIVERSES, PSU_ZONE_CAP, PSU_TRAFFIC_LOG, PSU_LOG_LEVEL
```

- **Networking:** a default bridge network with published ports, unlike angelware's host networking.
  PSU has no UDP or STUN, and Docker on Linux keeps external clients' source addresses, so per-IP limits
  and bans still work. Postgres is reachable only by `psud`.
- **The admin API** stays inside the network. `psuctl` runs as `docker compose exec psud psuctl ...`,
  since it's built into the same image.
- **Logs** go to stdout through the json-file driver, capped at `max-size: 10m`, `max-file: 3`.

## DRY: what the Python repeats

- `u32()`, `hdr(d)`, `ACC` constants and `packet(cmd, body)` are redefined in world, instance, combat,
  login and others. In Go they become one `wire.Writer` (LE/BE put helpers, fixed-offset `PutAt`) and
  one `wire.Header(to *Player)` for the common 0x24-byte body prefix.
- The 61 `elif cmd ==` branches in one 1400-line closure become a dispatch table
  `map[uint16]handler` registered per domain file (`handlers_lobby.go`, `handlers_mission.go`,
  `handlers_item.go`, ...). Each handler is `func(s *Session, r *wire.Reader) error`, with the size check
  done once before dispatch.
- The `cNNNN(d, ...)` builders become methods grouped by domain. Each packet's layout comment from the
  Python docstrings sits next to its builder.
- `games.current() == "aoti"` checks scattered through modules become one `disc.Profile` value on the
  session.
- `fragment_list` / `fragments` become one implementation in `wire`.

## SSLv3: use an existing fork, don't reimplement

Go's `crypto/tls` dropped SSLv3 in 1.14. Surveyed November 2026:

| Package | SSLv3 server | Deps | Activity | Verdict |
|---|---|---|---|---|
| [driftnet-io/insecure-tls](https://github.com/driftnet-io/insecure-tls) (BSD-3) | **yes, its stated purpose**: minimal-diff fork of stdlib `crypto/tls` (go1.22.3) with SSLv3 server and client re-added, RC4-SHA in `InsecureCipherSuites()`. Has `ssl30MAC`, the SSLv3 PRF and `finishedSum30` | `x/crypto` only | last commit 2024-06, 3 stars, one company maintainer | **use this** |
| [runZeroInc/excrypto](https://github.com/runZeroInc/excrypto) `crypto/ssl3/tls` (BSD-3) | yes: the old ZCrypto stack, server negotiates down to 0x0300, has `prf30` / `ssl30MAC` | its own forked crypto/x509 tree, go-logging, publicsuffix, x/crypto | active (commits Sept–Oct 2026) | fallback: maintained, but a heavy research-scanner stack |
| [zmap/zcrypto](https://github.com/zmap/zcrypto) `tls` | **no**: defines `VersionSSL30` but has no SSLv3 MAC/PRF/Finished code left | logrus, circl, publicsuffix, zcertificate | active | ruled out |

Why insecure-tls:
- Its API is exactly `crypto/tls` (`import tls "github.com/driftnet-io/insecure-tls"`), so
  `internal/ssl3` is about 50 lines: `tls.NewListener(ln, &tls.Config{MinVersion: VersionSSL30,
  MaxVersion: VersionSSL30, CipherSuites: []uint16{TLS_RSA_WITH_RC4_128_SHA}, Certificates: ...})`.
- Login and game see a plain `net.Conn`, and swapping implementations later only touches that file.
- Being stale is acceptable here. SSLv3 + RC4 are frozen protocols and we're claiming no security from
  them, so the risk is only "does it build on Go 1.27". Pin it in go.mod/go.sum.
- If it doesn't build or misbehaves with the PS2, swap in excrypto behind the same wrapper. Our own
  ~500-line implementation is the last resort.

**Tested 2026-11-12.** The probe is in the session scratchpad (`ssl3test/`), with both forks pinned to
local clones:
- Both forks build on Go 1.27.4 with no changes.
- An insecure-tls client against an excrypto server, and the reverse, both complete a full SSLv3
  handshake and echo application data: negotiated version 0x0300, suite 0x0005, RSA-1024 self-signed
  cert. These are two independent SSLv3 implementations agreeing.
- A hand-built PS2-style ClientHello (SSLv3 record, client_version 3.0, suite 0x0005 only, null
  compression, no extensions) gets a correct ServerHello (0x0300 / 0x0005) from both servers.

Still to do:
- One `openssl s_client -ssl3` interop run from the openlobby-ssl3 image. Pulling that image was blocked
  here, so it needs your OK.
- A real PCSX2 handshake.

Cert: RSA-1024 self-signed via stdlib `crypto/x509`, minted on first start into the cert volume. The
client doesn't check it.

## Correctness strategy

The Python is the reference until the Go version passes everything:
1. **Golden tests for `rules`.** A small Python script calls the reference functions (growth, combat
   damage, drops tables with a fixed seed, synthesis, econ, item records) on generated inputs and writes
   JSON vectors. Go table tests must match them byte for byte. The vectors are generated from local
   disc data, so they're kept out of git like `data/`.
2. **Wire tests.** Build each S>C packet in both implementations from the same inputs and compare. The
   Python's `testplay.json` captures give real C>S inputs.
3. **Acceptance.** Run `tools/testplay.py` (21 scenarios; 20 pass on Python with PSU only) against
   `psud`'s plain ports. It needs an "external server" option, about a 10-line change to testplay.
4. **Load.** Run `psubot` at the same 20/100/200 profiles as the Python numbers in NOTES.md.
5. **Real client.** PCSX2 with the retail disc, DNAS forwarded to the hosted DNAS, and names from
   revive-dns.

## Phases

| # | Scope | Done when |
|---|---|---|
| 0 | Scaffold, toolchain pin, `wire`, `netx`, `ssl3` (insecure-tls wrapper, cross-tested), `patch`, archtest, Dockerfile + compose + Makefile | `ssl3` cross-tests and openssl interop pass; `make up` brings up psud + postgres; PCSX2 gets past the patch server |
| 1 | `store` (Memory + Postgres, migrations), `login`, `auth` registry, `gamedata` loader | store tests pass against a throwaway postgres container; testplay login scenarios pass; PCSX2 reaches character select |
| 2 | `game` core: session, dispatch, character create/load/save, `world`, zone actors, lobby enter/move/chat/area change, universes | two clients see each other in the Colony; psubot 100-in-one-lobby beats the Python numbers |
| 3 | `rules` + missions: counter, accept, instances, enemies, combat, statuses, drops, EXP/levels, clear/rank, parties | golden vectors match; testplay mission scenarios pass |
| 4 | Items and social: inventory, shops, synthesis, grind, player shop, my room, cards, mail, blacklist, board, helpdesk, gm commands, season, events, news | full testplay passes (AotI cross-play once an AotI ISO exists) |
| 5 | Ops: admin API + `psuctl`, status.json, log rotation, per-IP limits, systemd unit, deploy docs | runs on the VPS behind revive-dns, with DNAS forwarded |
| 6 | Optional: port `extract/` to Go (`psuctl extract`) | `data/` builds identically; the EGS/Erlang step is the hard part (see below) |

**Phase 0 status (2026-11-12): done, except for the PCSX2 check.**
- About 1,300 lines of Go. The `wire`, `netx`, `ssl3`, `patch` and `archtest` tests pass.
- The patch bytes match prettystellaruniverse's exactly, and a Python client completes both discs'
  patch flows.
- An SSLv3 client completes handshakes on 12110, 12140 and 12061.
- `docker compose up` gives two healthy containers. The psud image is 8.7 MB and uses about 3 MB of RAM.
- Login and game currently accept the SSL handshake, log it and close (`pending` in cmd/psud).

**Phase 1 status (2026-11-12): done, except testplay and PCSX2.**
- About 2,900 lines of Go.
- `store`:
  - A `Store` interface with Memory and Postgres implementations, and embedded numbered migrations
    applied once each under an advisory lock.
  - One contract test runs against both, Postgres in a throwaway container (`make test-db`).
  - Tables: accounts (case-insensitive unique names), bans, news and the audit trail. Characters
    and social tables come as migrations in phase 2 and later.
- `auth`:
  - PBKDF2-SHA256 at 200k rounds, the same as the Python.
  - Auto-register, the admin reset-code flow, bans, and throttling: 8 failures per name and 30 per
    address in 10 minutes.
  - The single-use, 2-minute login keys the game service will redeem.
- `login`:
  - Every packet matches login.py byte for byte (hello, auth ok/error, game server, MOTD pages,
    textwrap wrapping).
  - An SSLv3 client ran the whole flow against the Docker stack: hello, MOTD, register, wrong
    password refused, login, 0x0216. The same account then logged in after a psud restart, with
    the name in a different case.
- `gamedata`:
  - Loads the extractor's data dir into memory, with per-disc overlays.
  - Names missing files, and refuses discs the data wasn't built for.
  - Served discs default to the built ones. Real data: 85 archives, 16 tables, 2,069 items.
- Not done:
  - `tools/testplay.py` doesn't run against psud yet. It needs plain-TCP test ports, which come with
    the game service in phase 2.
  - Login from a local client shows the Docker bridge address (172.x) as the peer. External players
    keep their own addresses.

**Phase 2 status (2026-11-12): done, except PCSX2.**
- About 6,500 lines of Go (tests included).
- `rules` (pure, imports only gamedata):
  - New state with the starter pack, stats, level and status blocks, character and slot records,
    the 712-byte user record, the 0x0A0A inventory, and the cross-disc appearance, type and PA views.
  - All 40 golden vectors match the Python byte for byte: 4 races × 2 genders × 5 types.
    `tools/golden.py` generates them from your data into `work/` (never committed).
- `store`: migration 2 adds characters (raw 0x0D02 + jsonb state).
- `game`:
  - The session handles a dispatch table of handlers, one file per domain.
  - Covered: key auth, character list, create, select, the load sequence, lobby, map/zone/quest
    change, native zone numbers, the Spaceport counter work, 0x0D04 work writes and story flags,
    chat, casts, and the Uni-cube.
  - One goroutine per zone instance owns its members, with no locks. The world registry stops a
    zone when its last member leaves.
  - A background saver coalesces state writes.
  - The same account logging in twice drops the older session.
- `netx` additions:
  - `After(f)`: a callback once queued writes are out. A newcomer only gets casts and chat after
    its zone load is written.
  - Write batching, a 1024-packet queue, and `SetIdle(0)` once authenticated (keepalive every 5 s).
- `psubot`:
  - `diff` runs one scenario on the Python server and psud and compares every packet: two
    players, map/zone/quest changes, a bounce, spawn, chat, move, Uni-cube, leave.
    **All 14 steps match byte for byte**; only the hello's temporary GID and the clock are
    excluded.
  - `load` runs N lobby movers, over plain TCP or `-ssl`.
- Load, everyone in one lobby, bots on the same machine, 4 moves/s each plus chat:

  | players | psud CPU (one core = 100%) | psud RSS | Python (logging off) |
  |---|---|---|---|
  | 20 | 5% | 33 MB | 18%, 45 MB (logging on) |
  | 100 | 44% avg (peaks use several cores) | 55 MB | 46%, 57 MB |
  | 200 | 118% across cores, all but 1 entered, none dropped | 84 MB | saturated, 80 couldn't enter, 1.5 GB |

- Tests: in-process game tests (meet/chat/move/part, save, a 30-player crowd) pass under the race
  detector (`make race`). The Docker stack with Postgres ran 8 SSLv3 bots through the real ports.
- Not yet: parties, missions, combat and everything in phases 3–4. My Room requests bounce to
  entry 0. Chat "!" commands are relayed as plain chat until GM commands exist.

**Phase 3 status (2026-11-12): done for PSU's free missions, except PCSX2.**
- About 12,500 lines of Go in total (tests included).
- `rules`, all golden-tested against the Python with a scripted dice stream both sides read in the
  same order (`tools/golden_combat.py` → `work/golden/combat.json`):
  - The full 0x0E00 resolve: player and enemy hits, misses, crits, deaths, support techniques,
    healing and revive items, cure and status items, Jellen/Zalure/Zoldeel, Dizas/Rentis and
    breakable boxes.
  - The status system (ailments, buffs, debuffs, barriers, ticks, expiry, hit-ended statuses),
    elements, equipment stats and immunities.
  - Enemy stats from the disc tables, kill EXP sharing, level-ups, type EXP, the types table, PA
    growth and links.
  - PP costs and regeneration, inventory events, drop rolls (enemy, box, area tables), and the
    judge: RPN start/clear rules, rank sections, rewards, the result block.
- `game`:
  - A mission zone works like the client's own room. Its goroutine owns enemies, flags, objects,
    boxes, ground items, wave clears, kills/EXP/drops and the judge.
  - Mission runs are shared across a mission's zones.
  - Characters have a mutex: a zone resolving hits locks its members in GID order; a session locks
    only its own. Rules are never evaluated while holding the run lock, and a zone starts without
    re-taking the world lock. Two deadlocks of exactly those kinds were found and fixed during
    testing.
  - Covered: counters, accept, depart, move between mission zones, leave, cancel, death and Scape
    Dolls, return after death, type change, equip/use/PP items/palette, PP spending on casts and
    regeneration, pickups, and parties (invite, join, leave, kick, leader, HP HUD, member
    locations, a member following the leader into the leader's mission instance).
- Python bug fixed rather than copied: judge runs and gateway wave clears were never reset, so
  repeating a mission with the same owner would never start or clear it again. Accepting now starts
  a fresh run, and wave clears reset with the zone.
- `psubot diff` now also runs a party and a mission. Party windows, counter, accept, departure, the
  join snapshot and 72 enemy status images are byte-identical to Python; the random fight passes
  the same checks on both servers (kills, EXP, wave clear, drop pickup, clear with results, leave).
- In-process `TestPartyMission`: two players clear together with concurrent hits, under the race
  detector.
- Load, 60 s, bots and server on the same machine:

  | lobby players | mission bots | psud CPU | psud RSS | missions cleared | Python |
  |---|---|---|---|---|---|
  | 100 | 4 | 58% avg | 65 MB | 168 | 46% / 57 MB, 28 cleared |
  | 200 | 4 | 130% across cores, all in | 104 MB | 128 | saturated, 80 couldn't enter |

- Not yet (phase 4 or AotI):
  - Grinding and PA disk learning (0x0105 events still go through the plain item path), shops,
    synthesis, My Room, chat GM commands.
  - Seasonal and event drops; recording clears for the board (no clears table yet).
  - AotI's Episode 2/3 story missions, bases and gates, and the cross-disc ground-item filter.

**Phase 4 status (2026-11-12): done for PSU, except PCSX2 and AotI cross-play.**
- About 19,800 lines of Go in total (tests included).
- `rules`, golden-tested against the Python (`tools/golden_econ.py` → `work/golden/econ.json`):
  - NPC shop listings, buying and selling, grinding (chance and outcome), PA disk learning,
    Lumilass and the Probo, and synthesis boards (the AotI element roll draws every time, like
    the Python's `_cum1000`).
  - My Room: furniture, the item stock, Probo gifts, and the room record with the shop and the
    visitor log.
  - My Shop: put, take back, price, open/close, buy, the visitor log, search.
  - Partner cards and trades (offers, the swap).
- `store`: migration 3 adds cards, mail and the blacklist; `OpenShops` feeds the shop search.
- `game`:
  - Shops and NPC dialogs, grinding, PA disks, Lumilass, the PP cube, chairs, My Room (own and
    visits), synthesis, partner cards with online flags and the holder window, mail, blacklist,
    chat shortcuts, view stats, and trades.
  - My Shop: a guest buys from an owner online (both characters locked in GID order) or offline
    (the stored state, under one mutex).
  - The load sequence carries the stored cards, blacklist, shortcuts and mail.
  - Card presence uses an in-memory index of who holds whose card (built at load), so a login
    or logout touches only the holders.
- Bugs found by the diff and fixed:
  - Trade replies went out after the trade lock was released, so the partner's next step could
    overtake them. They are now queued under the lock (Relay never blocks).
  - The post-trade 0x0115 carried playtime from before banking.
  - The shop search missed online owners whose save was still queued; it now reads their live state.
- Python bugs fixed rather than copied: the Lumilass commit no longer resets the character's
  state, and a visited room's owner byte comes from the owner, not the visitor.
- Kept different on purpose: a player's position resets when they join a zone. The Python
  carries over the last zone's position.
- `psubot diff`: 81 steps, all byte-identical to Python. New steps cover cards, card flags, mail,
  NPC shop talk/list/buy/sell, a meseta trade, grind quote, view stats, the own room, and My Shop
  (stock, put, open, search, visit, shop menu, guest buy, visitor log). Excluded: playtime and
  the visitor log's dates (wall clock).
- In-process tests: cards, mail and trade, and an offline My Shop purchase (search, visit, buy,
  meseta and visitor saved for the owner). They pass under the race detector.
- Operator side:
  - `calendar` (pure; new package): the event calendar (one-off and weekly windows, per-disc
    events, EXP/meseta multipliers, extra drop rolls, season item rates, featured missions)
    and the seasons (by date with Easter, aliases, the client's ids, holiday items). Tested
    against values computed by `psu/events.py` and `psu/season.py`.
  - `store` migration 4: `clears`, `events` (JSON specs, for `psuctl` in phase 5; the Python
    read a file), `mutes`, and the help desk's threads and messages. The contract test covers
    both backends.
  - `gamedata` loads the staged season archives (`nbl/season-staging`, 1.1 MB) into memory.
  - Seasons: the lobby load carries the season's archives, the id in 0x020F and the seasonal
    flag in 0x0205. A running event's season wins over `PSU_SEASON`; a GM's `!season` preview
    wins for that GM. Season items drop at 1 in `PSU_SEASON_DROP` from kills that rolled nothing.
  - Events: EXP and meseta multipliers on kills, drops and clear rewards; extra drop rolls; a
    ticker to everyone when one starts or ends (checked every 30 s); their lines join the news
    in the login MOTD and in `!news`.
  - News: the store's notices, a welcome ticker 6 s after the first lobby load, and the rotation
    every `PSU_NEWS_TICKER_SECS`.
  - Help desk: `!gm <message>` opens or adds to a request (5 per 10 minutes); staff answers
    (`Server.HelpReply`/`HelpClose`, for the admin API) show at once, or after the next lobby load.
  - GM commands for accounts in `PSU_ADMINS`: announce/uni/zone/notice, online, stats, spawns,
    kick, ban/unban, mute/unmute (kept in the store), summon/goto, give, meseta, level, warp,
    season, clear. Everyone has help, who, news and gm. Mutes stop chat from being relayed.
  - Mission clears are recorded; the public board (`board.json` v1, the Python's layout) is
    rewritten every `PSU_BOARD_SECS`: online players, levels, types, missions with the best
    distinct parties, recent clears, the week, events with featured results, the season, totals.
  - The session now reads in its own goroutine and runs queued jobs between packets
    (`player.job`), so a GM can move another player on that player's own goroutine. Fragment
    acks reach paced writes directly.
- `psubot diff`: 87 steps, all byte-identical to Python, also with `PSU_SEASON=snow` on both
  servers (the seasonal lobby archives and flags). New steps: `!help`, `!who`, `!gm` (status,
  request, status), `!stats`. Also excluded: the play time in 0x0201's level block.
- In-process tests: GM commands, help desk, mute, summon, ban; event notices; a seasonal lobby
  and a GM preview; clears and the board after the party mission. All pass under the race
  detector.
- Load, 60 s, bots and server on the same machine, with the snow season, a 20 s ticker and the
  board rewritten every 10 s:

  | lobby players | mission bots | psud CPU | psud RSS | missions cleared | phase 3 |
  |---|---|---|---|---|---|
  | 100 | 4 | 53% avg | 76 MB | 165 | 58% / 65 MB, 168 |
  | 200 | 4 | 131% across cores, all in | 111 MB | 126 | 130% / 104 MB, 128 |

- Not yet: AotI cross-play and AotI's story missions (they need a US AotI disc, or an undub
  built on one), and per-character story records on the board. The help desk's inbox and event
  editing wait for the phase 5 admin API.

**Phase 5 status (2026-11-12): done, except running on the VPS behind revive-dns.**
- About 21,800 lines of Go in total (tests included).
- `internal/admin`: the loopback admin API. It uses HTTP + JSON with a bearer token
  (`PSU_ADMIN_TOKEN`, else `<state>/admin.token`, made 0600 on first start) on
  `PSU_ADMIN_ADDR` (loopback only, default 127.0.0.1:22190). It covers status, players,
  announce, kick, mute, season previews; accounts (search, reset code, ban, unban, rename,
  delete), audit; news; events (specs checked with `calendar.Parse`); and the help desk
  (list, show, reply, close). It reaches the game through an `admin.Game` interface that
  `game.Server` satisfies, so no service imports another (archtest enforces it). It also writes
  `status.json` every 30 s.
- `cmd/psuctl`: the client, built into the same image (`make ctl ARGS="who"`, or
  `docker compose exec psud psuctl ...`). It shows tables for who, accounts, audit, news and
  help, and sends `X-PSU-By` from `$SUDO_USER`/`$USER` for the audit trail. Deleting needs `-yes`.
- `store`: account search with characters and bans, rename, delete (cascading), notice ids,
  the help inbox, and the audit tail, on both backends.
- `netx.Limiter`: one per-address budget across every public port, with open connections
  (`PSU_MAX_PER_IP`, now 16 since it is shared) and a refilling accept rate
  (`PSU_CONN_PER_MIN`, 60). Idle addresses are forgotten. The loopback test ports have no limit.
- `deploy/psud.service`: a hardened systemd unit for running without Docker (unprivileged, no
  capabilities, read-only system, `StateDirectory`). [DEPLOY.md](DEPLOY.md) covers data,
  configuration, firewall, DNS and DNAS, `psuctl`, events, the files psud writes, backups and
  upgrades. Logs: Docker's json-file driver (3 × 10 MB) or journald.
- Tests:
  - Admin API tests with a fake game: token, accounts (reset, ban, rename, delete with
    online check and force), news, event validation, announce, strict request bodies, token file.
  - Limiter tests (open cap, rate, refill, pruning).
  - A live run of psuctl against psud with bots online: who, say, news, events, mute, reset,
    ban (the player dropped at once), unban, audit, refusals.
  - The Compose stack built and run with PostgreSQL: both containers healthy, psuctl inside
    the container, migration 4 applied. The image is 28 MB.
  - `psubot diff` still matches all 87 steps; tests and the race detector pass.
- Hardening before going public:
  - Zone goroutines recover panics. The character locks a zone closure held are tracked and
    released, so one malformed packet no longer takes psud down. A handler panic drops that
    player and frees a character lock it left held.
  - GM names in `PSU_ADMINS` never auto-register (`psuctl create` makes them with a reset code).
  - Auto-registration is capped per address (`PSU_REGISTER_PER_HOUR`; loopback exempt).
  - PBKDF2 runs at most GOMAXPROCS at a time.
  - Idle limits before sign-in: 30 s for patch and game, 2 minutes for login.
  - A global connection cap (`PSU_MAX_CONNS`).
  - Per-player rate limits on chat, mail, card offers and trade requests.
  - The compose stack runs read-only, with `cap_drop: ALL`, no-new-privileges, and memory,
    process and file limits.
- Not yet: the VPS deployment itself (DNS through revive-dns, DNAS forwarding, an unpatched disc
  in PCSX2).

Phases 0–2 are the risky part (SSL, framing, the world model). Phases 3–4 are mostly mechanical porting
against golden tests.

## Open decisions

1. **Extractor.** I recommend keeping it in Python for now. It's stdlib-only and runs once per install.
   Its one heavy dependency is EGS's Erlang quest compiler, which builds the Colony lobby quest NBLs and
   the season decorations. Porting extract to Go means either porting EGS's script compiler or building
   the Colony quest from the native disc files. That's a project in its own right, so it's phase 6.
2. **OpenLobby panel/regapi integration** (`olport`, `adminport`, `gamelink`). I recommend dropping it
   for a small loopback admin API + `psuctl`, unless you plan to run an OpenLobby panel.
3. **PC (AotI PC client on 11230/12230) and Xbox 360 patch.** Both are experimental in the Python and
   never run end to end. I recommend leaving them out.
