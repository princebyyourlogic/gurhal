# gurhal

A Phantasy Star Universe server for the US PlayStation 2 discs (PSU, SLUS-21194, and
Ambition of the Illuminus, SLUS-21631). It's a Go rewrite of
[prettystellaruniverse](https://github.com/PSChuze/prettystellaruniverse): one static binary,
`psud`, running in Docker next to PostgreSQL. [GO_REWRITE.md](GO_REWRITE.md) has the design and
phases.

**Status: phase 5 (PSU).** Patch, login, the lobby, parties and PSU's free missions work:
counters, combat with enemies, statuses and techniques, EXP and levels, drops, wave clears,
mission clears with rank and rewards, alone or as a party. Also: NPC shops, grinding, PA disks,
synthesis, My Room and My Shop, partner cards, mail, blacklist and trades; seasonal lobbies, an
event calendar, news, a help desk, GM chat commands, and a public board file. AotI's story
missions wait for an AotI disc. Operators use `psuctl` (`make ctl ARGS="who"`) through a
loopback admin API; [DEPLOY.md](DEPLOY.md) covers running it on a server.

## Run it

```sh
make env        # creates .env; set POSTGRES_PASSWORD and PSU_GAME_ADDR
make up         # builds the image and starts psud + postgres
make status     # both should be healthy
make logs
```

psud needs the game data, built once from your own discs:

```sh
make import PSU=/path/to/psu.iso [AOTI=/path/to/aoti.iso]
```

Two things are not part of this stack:
- **DNS:** point consoles at a resolver (revive-dns) that answers `*.psuus.segaonline.jp` /
  `*.ipsuus.segaonline.jp` with this host.
- **DNAS:** forwarded to the hosted revival DNAS.

| Port | Service | Transport |
|---|---|---|
| 11110 / 11140 | patch, PSU / AotI | plain TCP |
| 12110 / 12140 | login, PSU / AotI | SSLv3 RC4-SHA |
| 12061 | game, both discs | SSLv3 RC4-SHA |

## Develop

`scripts/go` runs the pinned Go 1.27.4, downloading it once into `work/` with a checksum check.
`make test`, `make vet`, `make fmt` and `make run` (psud on the host without Docker, accounts in
memory, plain test ports on 22110/22140/22061) all use it. `make test-db` also runs the store
tests against a throwaway PostgreSQL container; `make test-data` adds the tests that need
`./data` and the golden vectors (`make golden`: character rules and combat, both from the Python
reference on your data); `make race` runs the concurrent packages under the race detector.

`psubot` (`scripts/go run ./cmd/psubot`) plays the client against plain ports: `diff` compares
psud with the Python server packet by packet, `load` runs a crowd.

Game files from the discs (`disc/`, `data/`, `extract-cache/`) are never committed or published.
