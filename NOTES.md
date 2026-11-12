# Phantasy Star Universe (PS2, USA) revival — bearings

Game files come from the owner's own disc and must never be committed or published
(`disc/` and `psu_us_strings.txt` are derived from it).

## Existing work: this is largely done already

**[PSChuze/prettystellaruniverse](https://github.com/PSChuze/prettystellaruniverse)** (GPL-3.0, Python asyncio
+ SQLite, first public commit 2026-10-23, v0.1.1). It's a full PS2 server for PSU (SLUS-21194) and AotI
(SLUS-21631), with a disc data extractor (`extract/`: CVM, NBL, PRS, ERX, item/enemy/drop/mission/level
tables), a setup program and a scripted test player. It claims lobbies, parties, trade, missions with
EXP/drops/ranks, levels/types/PAs, shops, synthesis, My Room/My Shop, GM tools and events. Known gaps:
Ep1 story was offline-only, AotI Ep3 online-only story isn't on disc, mixed-disc parties are limited,
real PS2 is less tested.
- Ports: patch 11110 (plaintext), login 12110 / game 12061 over **SSL 3.0 RC4-SHA** (stunnel) → 22110 / 22061.
- Framing: `u32le size | u16 BE command | u8 channel (C 2, S 3) | u8 0 | body`; addresses given as
  `PSU_US.erx` image offsets (RAM = image + 0x248000).
- Answers "who computes EXP/drops": the server does (they reimplement it from disc tables + PSUPedia).
- **DNAS: they use a 4-instruction client bypass** (pnach / Cheat Device / patched ISO), claiming the
  replies are Sony-signed and can't be served. Our TSFP work says otherwise for i-connect (symmetric only,
  replays). **PSU's DNAS game key `a63bbca3e99ade9c` (at `PSU_US.ERX+0x547000`) is in DNASrep's US store**
  (one packet, `a63bbca3e99ade9c_01100000`; TSFP's was `_01080000`). Unmodified-disc online looks
  feasible. That's the most obvious contribution from our TSFP stack.
  Plan: don't run our own DNAS. A hosted community DNAS-replay service covers most games, so point
  `gate1.us.dnas.playstation.org` (+ the `playstaion` alias) at it in the resolver. Their compose uses
  OpenLobby's DNS (`dns` service: answers the four game names, forwards everything else), so this is a
  DNS record override plus testing an **unpatched** disc in PCSX2.

**[essen/egs](https://github.com/essen/egs)** (AGPL-3.0, Erlang, Loïc Hoguin, 2010–2019). It's the open
server for the **PC** version and the source of the protocol field names prettystellaruniverse started from.

## prettystellaruniverse review (cloned 2026-11-11 at `f905648`, v0.1.1)

Cloned into `prettystellaruniverse/`. About 31k lines of stdlib Python: `server/psu/` (37 modules),
`extract/` (disc → `data/`), `tools/` (testplay bots, packet digest checks), `setup.py` (2.7k-line host wizard).
The author is not expected to collaborate, so treat this as a GPL-3.0 fork we host and maintain.

**Run results (2026-11-11, PSU disc only, Python 3.14):**
- `python3 -m extract --psu "<iso>" --out ../data --work ../extract-cache` builds cleanly: `data/` is 8 MB,
  the cache 103 MB, a few minutes.
- **Bug fixed (local, uncommitted):** with only the PSU disc, `extract/gamedata.py` `apply_type_overrides`
  wrote 10-entry unlock requirements for the overridden PSU types, but `progression.types_table` packs
  `<16b`, so the 0x0113 types packet raised `struct.error`. Now padded to the disc row width (16).
- Setup unit tests: 90/90 OK.
- `tools/xgame_check.py`: not meaningful without the AotI disc (fixed digests and AotI item ids assume a
  two-disc build; KeyError on an AotI id). Rerun once an AotI ISO is available.
- `tools/testplay.py --game psu --scenario all`: **20/21 pass** (login, zones, shops, invsync, grind,
  synthesis, My Room, mission, levelup, party, technique, chat, trade, GM, PP, board). `story`/`ep3` SKIP (AotI-only).
  `xparty` fails, expected without AotI data: its AotI bot gets PSU's tables. Recheck with AotI.

**Load (2026-11-11, one PC, one game process, bots on the same machine).** Driver: a throwaway
Python script, since superseded by `psubot load`.
Lobby bots all stand in ONE lobby zone (the worst case, N² relay) and send a 0x0102 move at 4 Hz plus chat
every 10 s. Mission bots loop the testplay mission flow.

| players | traffic log | game RSS | CPU (1 core) | logs | result |
| --- | --- | --- | --- | --- | --- |
| idle (patch+login+game) | on | 21+28+34 MB | ~0 | | |
| 20 + 2 missions | on | 45 MB | 18% avg | 6.7 GB/h | all OK, 20 missions cleared |
| 100 + 4 | on | 134 MB | 63% avg / 94% peak | 16.6 GB/h | 35 of 100 couldn't enter (timeouts) |
| 100 + 4 | **off** | 57 MB | 46% / 63% | 0.14 GB/h | all OK, 28 missions cleared |
| 200 + 4 | on | 2.0 GB | 91% / 100% | | 148 of 200 couldn't enter |
| 200 + 4 | off | 1.5 GB | 79% / 100% | | 80 of 200 couldn't enter |

Takeaways: the per-packet logging (hexdump into every session log with a flush, plus an `events.jsonl`
open/close and a stdout print for every send/recv) costs about half the CPU and makes the logs a
disk-filler. With it off, ~100 players crowded into one zone fit on one core in 57 MB. 200 in one zone saturates the single asyncio
thread and memory grows from unbounded write buffers (no backpressure on relays). Real play spreads
players over zones/missions, and the bot harness itself shares the CPU, so this is pessimistic.
Added locally: `PSU_TRAFFIC_LOG=0` (observe.py `event` skips recv/send; game.py `record` skips hexdumps).

**What's missing for a public host** (their design is "host it at home for friends"):
1. **No public DNS.** Their resolver (OpenLobby `dns`) only answers private ranges; internet players are
   told to run `tools/devdns.py` on their own LAN. A public host needs a resolver on a public IP that
   answers the four game names (`loginps2/patchps2` × `psuus/ipsuus.segaonline.jp`) and the DNAS names (→
   hosted DNAS), and refuses everything else (no open resolver / amplification). TSFP's
   `dns_server.py` is the template.
2. **DNAS needs a client patch.** Players need a pnach/Cheat Device/patched ISO. Forwarding DNAS to the hosted
   DNAS should let stock discs connect (key `a63bbca3e99ade9c` is in DNASrep). Untested.
3. **Third-party images on `:latest`.** `ghcr.io/prettyopenlobby/openlobby-ssl3` (the SSLv3 stunnel) and
   `openlobby` (DNS) come from the same author, unpinned. Source is AGPL in PrettyOpenLobby/OpenLobby
   (`services/Dockerfile.ssl3`, `sslterm/`), so build and pin our own.
4. **Unbounded pre-auth reads.** `game.py:529` / `login.py:156` frame on a client-supplied `u32` size with
   no upper bound, and `size < 8` just `break`s, so a client can grow `buf` forever before logging in.
   Needs a max packet size + drop. Also `buf += data` is quadratic.
5. **Logs grow forever.** Every session writes a full hexdump (`logs/sessions/*.log`, incl. chat) plus
   `events.jsonl`, with no rotation or retention. That fills a VPS disk and holds player chat indefinitely.
6. **Tests only cover `setup.py`** (`tests/test_setup_*`); CI never exercises the server (testplay needs
   disc data). We'd want a self-hosted CI job with our data.
7. Code shape: `game.session` is one ~1400-line closure with 51 `elif cmd ==` branches. It works, but it's
   the main cleanup target if we maintain this long term.
8. Real PS2s are less tested ("mission loads on real hardware may still stall"). That needs hardware testing.
9. Content gaps (theirs): no AotI Ep3 online story (not on disc), mixed PSU/AotI parties split at mission start.

## Disc: Phantasy Star Universe (USA), `SLUS_211.94`

ISO 9660, volume `PSU`, built 2006-08-24/25. Source: the owner's own retail disc image.

| File | Size | What it is |
| --- | --- | --- |
| `SLUS_211.94` | 848 KB | Boot ELF. Metrowerks `MW MIPS C Compiler (2.4.1.01)`, symbol table empty. Loader for the ERX modules. |
| `ERX/PSU_US.GZ` → `PSU_US.ERX` | 2.6 MB → 7.8 MB | **The actual game.** gzip'd Sony ERX relocatable module (`.eemod`, `.erx.lib/.erx.stub`), built 2006-08-24. 5.0 MB `.text`, 1.1 MB `.data`, full `.rel.*` relocations, no symbols. Same MW 2.4.1.01 compiler. |
| `ERX/DNAS_NET.GZ`, `DNAS_MC.GZ` | | Sony libdnas2 as ERX modules. Version string `PsIIdnas2no 300ADnAsusfermat20040315201839` (US DNAS 3.00). |
| `MODULES/DNAS300.IMG` | 276 KB | DNAS IOP image. |
| `CDPSU0.AFS`, `CDPSU1.AFS` | 480 MB, 398 MB | CRI AFS archives (plain `AFS\0` header, 0x11 and 0x14 entries at top level). Likely audio (`xa_*.adx`) / streamed data. |
| `PSUROFS.CVM` | 3.1 GB | CRI CVM (`CVMH`, `ROFSROFSBLD Ver.1.52 2003-06-09`). Holds the main ROFS filesystem (the `*.nbl` files). **Inner TOC is scrambled** — needs CVM password/descrambling before extraction. |
| `NETCNF/`, `MODULES/*.IRX` | | Stock Sony network config (Your Network Configuration file) and IOP modules (SMAP, PPP, DEV9, USB keyboard). |

Compiler note: unlike TSFP (SN/GCC 3.2) this is **Metrowerks CodeWarrior**, so a byte-matching
decomp would need a different toolchain (mwccps2). That's not the goal here: Ghidra on
`PSU_US.ERX` (load as relocatable at a chosen base, apply `.rel.*`) is for reading the protocol and game rules.

## Network endpoints found in `PSU_US.ERX`

- `loginps2.psuus.segaonline.jp`: login server (NA PS2)
- `patchps2.psuus.segaonline.jp`: patch server (NA PS2)
- DNAS: US gateway (libdnas2 v3.00), so the TSFP DNS + DNAS-replay stack should carry over,
  provided DNASrep (or a capture) has PSU's game-key packet.

Ports, packet framing and crypto: TODO (Ghidra: xrefs from these strings; `TNetworkAdapter_PS2`, `TPsuClient`).

## Client/server architecture (from C++ class names in the ERX)

The client has an internal **client/server split**: `TPsuServer`/`TPsuServerBase` vs `TPsuClient`,
`TServerActor{Player,Enemy,Object,Vehicle,...}`, `TBoss*Server` (with `...ServerOn`/`...ServerOff`
variants), `TQuestControl` vs `TQuestControlClient`, `TQuestJudge` vs `TQuestJudgeClient`,
`TConditionWorkServer/Client`, `TStatusFlagServer/Client`, `TVehicleManagerServer/Client`,
`TItemParamManagerClient`, `TQuestContractConditionCalc(Client)`, `TQuestScriptVM`, `TQuestPacketBase`.

Working hypothesis: offline story mode runs the "server" half in-process. Online, either the
real server or the party host runs it. If so, the mission, enemy, boss and quest logic the server
needs is on the disc. That means reimplementing or porting `TPsuServer` code paths rather than
guessing behaviour. **Verify in Ghidra:** what does `TPsuServer` do online vs offline, and which
packets does `TPsuClient` wait on (drops, EXP, rewards)?

Other online-only classes: `TOnlineLoginCtrlExPS2`, `TPatchClient`, `TSelectServerCounter`,
`TWindowServerName`, `TPartyClient`, `TGameRoomClient`, `TMyRoomManageClient`/`TMyRoomSender`
(personal room/shop), `TGuildCard*`, `TWindowMail*`, `TLobbyCounter*`, `TDnasNet`.

## Data files referenced (inside the CVM, not yet extracted)

`ga_ItemParam.nbl`, `ga_tbl.nbl`, `ga_statuschange_table.nbl`, `ga_QuestCommon.nbl`,
`xf_ItemWeaponAll.afs`, `xf_ItemTechAll.afs`, `xf_ItemShopData.afs`, per-enemy `Param*.snr`,
`AtkDat*.snr`, `DamageData*.snr`, `ParamServer<Boss>.snr`, `su_en_*.nbl` (enemies), `xf_DC_map_*.nbl`.
Item params and shop data look client-side. Whether EXP and drop tables are too: TODO.

## Next steps

1. Descramble/unpack `PSUROFS.CVM` and unpack NBL (see research below).
2. Ghidra project for `PSU_US.ERX` (relocatable load; class names → vtables → methods).
3. DNAS: check DNASrep for PSU's US game key; else capture on a real PS2 (no, servers are gone) →
   check whether the retail i-connect response replays like TSFP's.
4. PCSX2 + our DNS → observe the first packets to `loginps2...` (port, TLS or not).
5. Compare against Clementine's behaviour / PC captures for the shared protocol.

## Research

### Network / community (web research, 2026-11-11)

- **Clementine** ([psu-clementine.com](https://psu-clementine.com), Discord teamclementine) is PC-only
  (JP AotI PC client); its FAQ says no PS2 or 360. Closed source, active (Update 107, Oct 2026). Its
  update notes are a reference for server-side behaviour: EXP, drops per party size, meseta, synthesis.
- PC and PS2 shared servers (cross-play); 080e carries a `Platform` byte (0 = PS2, 1 = PC). 360 was separate.
  Shutdowns: NA/PAL PC+PS2 2010-03-31, JP PS2 2011-04-14, 360 2012-09-07, JP PC 2012-09-28.
- Header per EGS: `u32le size | u8 category | u8 sub | u8 channel (1 broadcast, 2 C>S, 3 S>C) | pad`
  (same shape as PSO2). Fragments 0b03 / ack 0b05. Login/game over SSLv3; PC patch server uses a
  PSO-v2-style lagged-subtractive stream cipher (PS2's patch port is plaintext per prettystellaruniverse).
- Server-authoritative online: character storage, EXP/levels, damage results (0e00 → 0e07), drops,
  meseta/rewards, shops, synthesis, PA levels, instancing. Position/chat/emotes are client-relayed.
- Other tools: gasetools Erlang MITM proxies (login/patch), HybridEidolon/psuseed (Clementine hook),
  HikikoMarmy/PSUEx, DanielCoffey1/psu-decomp (360 static recomp, offline only).
- Contacts: PSChuze is pseudonymous, no site/Discord. The only channel is GitHub issues on the repo. Possibly
  linked to [PrettyOpenLobby](https://github.com/PrettyOpenLobby/OpenLobby) (public server play.openlobby.fyi);
  PSChuze mentioned talking to PS Rewired about DNS routing (CosmicScale/PSBBN-Definitive-Project#672).
  No public PS2 PSU server found.

### Data / assets (web research, 2026-11-11)

- Regions: PSU NA SLUS-21194 (CRC 29B44F89), JP SLPM-66031 / SLPM-74244, EU SLES-54308; AotI NA
  SLUS-21631 (CRC 5EA50CB0), JP SLPM-66663, EU SLES-54892. NA/PAL servers closed 2010-03-31, JP PS2 2011-04-14.
- Online is a thin client: EGS shows the server sending quest/zone files (020e/020f), counters (0c05/0c06),
  shops (010a), EXP/level/meseta/type levels (0115), final stats, type info (0113) and rewards (0c09).
- **CVM**: CRI ROFS; only the TOC is encrypted, using an 8-byte key derived from an ASCII password.
  [JayFoxRox/cvm_tool](https://github.com/JayFoxRox/cvm_tool) `split -p <pw>`. PSU's password isn't public
  (prettystellaruniverse's `extract/cvm.py` handles it).
- **NBL**: `NMLL` (+ `TMLL` textures), PRS compressed, Blowfish with a per-file key in the header.
  [Tenora Works/PSULib](https://github.com/Agrathejagged/tenora-works) (C#) parses item, drop
  (`itemEnemyDrop`, `itemObjectDrop`), enemy (`EnemyLevelBaseParam`, `Think*`), layout (`set_r*.rel`) and text.
  [essen/gasetools](https://github.com/essen/gasetools) (C, GPL-3): nbl/afs tools + `docs/psufiles.ods` file list.
- Models: Shadowth117's PSO2-Aqua-Library (SegaNN, PSU NOM) and PSU-Model-Tools.
- No public decomp, symbol leak or PS2 prototype. Clementine is PC-only (JP AotI PC client), closed source.
- Validation: PSUPedia / re-psupedia.info, JP wiki spoiler.sakura.ne.jp/rr/psu, Clementine Discord.
