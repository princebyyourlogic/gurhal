# Deploying psud

psud is one static binary that serves patch, login and game for both US discs. It runs next to
PostgreSQL, either as the Docker Compose stack in this repository (recommended) or as a systemd
service. DNS and DNAS are not part of it.

## What you need

- A Linux host with a public IPv4 address. 1 CPU and 512 MB of RAM are plenty: 200 players use
  about 110 MB and a bit more than one core.
- The game data, built once from **your own discs** (`make import`, see below). Game files are
  never part of the image and must not be published anywhere.
- Inbound TCP on these ports, and no others:

  | Port | Service |
  |---|---|
  | 11110 / 11140 | patch, PSU / AotI (plain TCP) |
  | 12110 / 12140 | login, PSU / AotI (SSLv3) |
  | 12061 | game, both discs (SSLv3) |

  The admin API (22190) listens on loopback only, and PostgreSQL is not published at all.

## 1. Build the game data

On any machine with Docker and the ISOs:

```bash
make import PSU=/path/to/psu.iso AOTI=/path/to/aoti.iso
```

AOTI is optional. This writes `./data`. Copy it to the server privately, for example with
`rsync -a data/ server:gurhal/data/`. Never put it in a public repository, image or download.

## 2. Configure

```bash
make env
```

This creates `.env` from `.env.example`. Set at least:

- `PSU_GAME_ADDR`: the server's public IPv4. Login hands it to the console for the game
  connection, so it must be reachable from the internet.
- `POSTGRES_PASSWORD`: a long random string.
- `PSU_ADMINS`: the account names (in-game user IDs) allowed the `!` GM chat commands. These
  names never auto-register, so nobody can claim one first. Make each one after starting with
  `make ctl ARGS="create NAME"`. That prints a one-time code: log in with the code followed at
  once by your password.

The rest have working defaults:

- `PSU_MOTD` is the login notice when there is no news.
- `PSU_UNIVERSES` lists the universes.
- `PSU_MAX_PER_IP` and `PSU_CONN_PER_MIN` cap one address (one household router) across all
  ports. `PSU_MAX_CONNS` caps everyone together. `PSU_REGISTER_PER_HOUR` caps the new accounts
  one address may auto-register.
- `PSU_SEASON` sets the lobby season.
- `PSU_NEWS_TICKER_SECS` sets how often the news ticker rotates.
- `PSU_BOARD_SECS` sets how often the public board file is rewritten.

## 3. Start

```bash
make up
```

```bash
make status
```

```bash
make logs
```

`make status` should show both containers healthy. The Docker restart policy keeps them running
across reboots, so you don't need a systemd unit on this path. Logs go to Docker's json-file
driver, capped at 3 × 10 MB.

Open the five ports in the host firewall, for example with ufw:

```bash
for p in 11110 11140 12110 12140 12061; do sudo ufw allow $p/tcp; done
```

## 4. DNS and DNAS

The discs connect to fixed host names, so players need a resolver that sends them to you:

- **Game names:** `loginps2.psuus.segaonline.jp`, `patchps2.psuus.segaonline.jp`,
  `loginps2.ipsuus.segaonline.jp` and `patchps2.ipsuus.segaonline.jp` all resolve to
  `PSU_GAME_ADDR`. revive-dns does this.
- **DNAS:** `gate1.us.dnas.playstation.org` (and the misspelled alias `gate1.us.dnas.playstaion.org`)
  resolve to the hosted community DNAS-replay service, which carries PSU's US game key.
- **Everything else:** forwarded or refused. Never run an open resolver: answer only these names
  for the public, or other names only for your own clients.

Players set that resolver as the primary DNS in the network settings: the PS2 network
configuration, or PCSX2's DEV9 DNS settings.

Connecting with an unpatched disc through the hosted DNAS is not tested yet. If it fails, players
need the usual DNAS-skip patch (pnach, Cheat Device or a patched ISO).

## 5. Run the server

Admin commands go through `psuctl`, inside the container:

```bash
make ctl ARGS="who"
```

| Command | What it does |
|---|---|
| `status`, `who` | the server's state and everyone online |
| `say [-scope uni:2] [-kind dialog] <text>` | a ticker or dialog to everyone, a universe, an area or a player |
| `kick <name> [reason]`, `mute <name> [minutes] [reason]`, `unmute <name>` | moderation (name: a character or an account) |
| `accounts [pattern]`, `audit [n]` | accounts with their characters and bans, and the latest changes |
| `reset <account>` | a one-time 6-character code. The player logs in with the code alone, or the code followed at once by a new password. |
| `ban <account> [hours] [reason]`, `unban <account>` | 0 hours means 10 years. An online player is dropped. |
| `rename <account> <new>`, `delete <account> -yes [-force]` | delete removes everything under the account (back up first) |
| `news`, `news add <text>`, `news rm <id>` | notices for the login box, the ticker and `!news` |
| `events`, `events set <file.json>`, `events rm <key>` | the event calendar (format below) |
| `help`, `help show <id>`, `help reply [-close] <id> <text>`, `help close <id>` | player requests sent with `!gm` |
| `season`, `season <name> <season or none or off>` | the lobby season, and a preview for one character |

An event spec looks like this. It's one JSON object per event; times are UTC, and days run 0
(Monday) to 6:

```json
{"key": "halloween-2026", "name": "Halloween Festival",
 "start": "2026-10-28T00:00Z", "end": "2026-11-08T00:00Z",
 "season": "halloween", "exp": 1.5, "drops": 2, "event_drop": 20,
 "missions": [110000], "notice": "Pumpkins are dropping all over Gurhal!"}
```

Use `"weekly": {"dow": [5, 6], "from": "00:00", "hours": 48}` instead of `start`/`end` for a
recurring event. Add `"game": "psu"` or `"aoti"` for one disc only. `events set` checks the spec
before saving it, and running events take effect within 30 seconds.

In game, the accounts in `PSU_ADMINS` have the same tools as chat commands. Type `!help` in chat
for the list.

### Files psud writes

These go to the `state` volume (`/state` in the container, `/var/lib/psud` under systemd):

| File | What it is |
|---|---|
| `ssl3.pem` | the SSL certificate the discs accept. Keep it, so it survives rebuilds. |
| `admin.token` | the admin API's token (unless `PSU_ADMIN_TOKEN` is set) |
| `status.json` | players online per universe and area, uptime and zones, every 30 s, for monitoring |
| `board/board.json` | the public board: online players, levels, mission rankings, recent clears, events. It has character names only. |

A board website should read only `board/board.json`, for example by bind-mounting the state
directory read-only into it.

### Backups and upgrades

```bash
make backup
```

This writes a `pg_dump` to `backups/`. Run it from cron daily, and before any `delete`. To
restore, feed the dump to `make psql`. To upgrade, pull the new source and run `make up` again;
database migrations apply themselves at start.

## Without Docker (systemd)

1. Build both binaries with the pinned toolchain:

   ```bash
   CGO_ENABLED=0 scripts/go build -o work/ ./cmd/psud ./cmd/psuctl
   ```

2. Install PostgreSQL 16 or later. Create a `psu` role and database, and set
   `PSU_DATABASE_URL=postgres://psu:...@127.0.0.1:5432/psu` in `/etc/psud.env`. That file has the
   same variables as `.env.example`.
3. Create the user with `useradd --system --home /var/lib/psud psud`. Install the binaries to
   `/usr/local/bin` and the data to `/opt/psud/data`, then install
   [deploy/psud.service](deploy/psud.service) and run `systemctl enable --now psud`.
4. Logs go to journald (`journalctl -u psud`). Run `psuctl` on the host as the service user:

   ```bash
   sudo -u psud PSU_STATE_DIR=/var/lib/psud psuctl who
   ```

The unit runs psud unprivileged, with no capabilities and a read-only system. All of its ports
are above 1024.

## What psud does against abuse

- **One address:** a shared budget across all ports, covering open connections and new
  connections a minute; past it, connections are closed at once.
- **Before signing in:** patch and the game's key exchange get 30 s of silence, login 2 minutes
  (players type their ID and password); the SSL handshake gets 20 s.
- **Logins:** failed logins are throttled per name and per address, and password hashing runs at
  most one per CPU at a time. A login flood queues instead of starving the game.
- **Accounts:** at most `PSU_REGISTER_PER_HOUR` new accounts per address an hour, and GM names
  are never auto-registered.
- **In game:** chat, mail, partner-card offers and trade requests are rate-limited per player.
- **Crashes:** a packet that crashes a handler drops only that player. One that crashes a zone
  is logged, and the zone carries on with the character locks it held released.
- **Containers:** read-only root filesystem, no capabilities, no-new-privileges, and memory,
  process and open-file limits. PostgreSQL is never published, and the admin API listens on
  loopback only.
