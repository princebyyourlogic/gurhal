"""Combat, drops, EXP and judge golden vectors from prettystellaruniverse for psud's rules tests.

  python3 tools/golden_combat.py <prettystellaruniverse> <data dir> <out.json>

Randomness is a scripted stream both sides read in the same order: value k is (k * 7919 + 13) % 1000003;
randint(a, b) = a + v % (b - a + 1), randrange(n) = v % n, random() = (v % 1000000) / 1e6. The output is derived
from disc data: keep it under work/ (never committed).
"""
import json, os, random, struct, sys

repo, data, out = sys.argv[1:4]
os.environ.update(PSU_GAMEDATA=os.path.join(data, "gamedata"), PSU_NBL_DIR=os.path.join(data, "nbl"),
                  PSU_CAPTURE_DIR=os.path.join(data, "captures"), PSU_LOG_DIR=os.path.join(os.path.dirname(out), "pylogs"),
                  PSU_STATE_DIR=os.path.join(os.path.dirname(out), "pystate"))
sys.path[:0] = [os.path.join(repo, "server"), os.path.join(repo, "tools")]
from testplay import RAW_0D02                      # noqa: E402
from psu import combat, drops, items, judge, missions, progression  # noqa: E402


class Stream:
    def __init__(self):
        self.k = 0

    def next(self):
        v = (self.k * 7919 + 13) % 1000003
        self.k += 1
        return v


S = Stream()
random.randint = lambda a, b: a + S.next() % (b - a + 1)
random.randrange = lambda n: S.next() % n
random.random = lambda: (S.next() % 1000000) / 1e6


class P:
    def __init__(self, gid, lid, raw, st):
        self.gid, self.lid, self.raw, self.state, self.game = gid, lid, raw, st, "psu"
        self.zone = None


def u32(v):
    return struct.pack("<I", v & 0xFFFFFFFF)


def hit(pos, src, dst, w, D):
    B = bytes.fromhex("0101bda5956ab5c38c9b804109941044")
    C = bytes.fromhex("010189249235c2c30c00604150370b44")
    return struct.pack("<fff", *pos) + struct.pack("<IIII", src, dst, w, 0) + B + C + D


def body(lid, seq, hits):
    return bytes(0x24) + u32(len(hits)) + u32(lid) + u32(seq) + b"".join(hits)


def make_char(race, gender, cls):
    raw = bytearray(RAW_0D02)
    raw[0x68], raw[0x69], raw[0x6A] = race, gender, cls
    raw = bytes(raw)
    st = progression.new_state(raw)
    st["inv"], st["pas"], st["next_uuid"] = items.starter(raw, 1)
    return raw, st


out_doc = {"battles": [], "enemies": [], "exp": [], "drops": [], "judge": [], "counters": []}

# --- battles: a player hits the first mission's enemies with the starter weapons; enemies hit back ---
d = missions.data()
qid = next(q for q in d["counters"]["0"]["quests"] if missions.mission(q))
mm = missions.mission(qid)
zone = mm["enter"][0]
for race, gender, cls, weapon_i in ((0, 0, 0, 0), (1, 1, 3, 1), (3, 0, 7, 0)):
    raw, st = make_char(race, gender, cls)
    st["inv"][weapon_i]["active"] = 1
    p = P(0x01000000 + race, 0, raw, st)
    p.zone = (1, qid, (zone, p.gid))
    combat.fights.clear()
    wpn = st["inv"][weapon_i]["uuid"]
    roster = sorted(combat.roster(p.zone))[:6]
    k0 = S.k
    batches = []
    ENEMY_D = bytes.fromhex("04120000") + bytes(16)
    POISON_D = struct.pack("<IIhhhhBBBB", 0x3204, 0x2, 0, 0, 0, 100, 0, 0, 1, 0)
    plan = []
    for rnd in range(25):
        plan.append([hit((1.0, 0.0, 2.0), 0, e, wpn, bytes(20)) for e in roster])
        if rnd % 5 == 2:
            plan.append([hit((0.0, 0.0, 0.0), roster[0], 0, 0, ENEMY_D), hit((0.0, 0.0, 0.0), roster[1], 0, 0, POISON_D)])
        if rnd == 10:
            mono = next(x for x in st["inv"] if x["id"] == "03010000")
            plan.append([hit((0.0, 0.0, 0.0), 0, 0, mono["uuid"], bytes(20))])
    for i, hits in enumerate(plan):
        b = body(0, i, hits)
        entries, kills, hurt, grown = combat.resolve(p, [p], b, max(1, st["level"]))
        batches.append({"body": b.hex(), "entries": [e.hex() for e in entries], "kills": [e.id for e in kills],
                        "hp": st["hp"], "se2": combat.se2(p), "enemy_hp": {str(e.id): e.hp for e in combat.zone_enemies(p.zone).values()},
                        "pas": st["pas"], "grown": [[g[1][0], g[1][1], g[2]] for g in grown]})
    out_doc["battles"].append({"quest": qid, "zone": zone, "raw": raw.hex(), "state": st | {"inv": st["inv"]},
                               "start": k0, "batches": batches})
    # the state at the start: the test rebuilds it, so record the weapon slot instead of the mutated state
    out_doc["battles"][-1]["weapon"] = weapon_i

# --- enemies: stats for every roster spec of the first counter's missions ---
for q in d["counters"]["0"]["quests"][:6]:
    m = missions.mission(q)
    if not m:
        continue
    for z, gates in m["gateways"].items():
        for g in gates:
            for e in g["enemies"][:3]:
                en = combat.Enemy(e["id"], e["level"], dict(e, gateway=g["id"], map=g["map"]))
                out_doc["enemies"].append({"spec": e, "hp": en.max_hp, "stats": en.stats, "exp": en.exp,
                                           "element": en.element, "pct": en.element_pct})
for lvl in (1, 5, 50, 199, 300):
    en = combat.Enemy(9999, lvl, None)
    out_doc["enemies"].append({"generic": lvl, "hp": en.max_hp, "stats": en.stats, "exp": en.exp})

# --- EXP and type EXP ---
for race, gender, cls in ((0, 0, 0), (2, 1, 5), (3, 1, 9)):
    raw, st = make_char(race, gender, cls)
    p = P(1, 0, raw, st)
    steps = []
    for amount in (10, 100, 1000, 5000, 50000, 10 ** 9):
        up = combat.add_exp(p, amount)
        old, new = judge.add_type_exp(raw, st, amount // 10)
        steps.append({"amount": amount, "up": up, "level": st["level"], "exp": st["exp"], "hp": st["hp"],
                      "tl": [old, new], "texp": st["type_exp"][st["cls"]],
                      "types": progression.types_table(raw, st).hex(), "status": progression.status_block(raw, st).hex()})
    out_doc["exp"].append({"raw": raw.hex(), "steps": steps})

# --- drops: enemy and box rolls on the scripted stream ---
t = drops.tables()
k0 = S.k
rolls = []
for variant in list(t["enemy"])[:40]:
    for lvl in (1, 30, 90):
        for area in (6, 12, 21):
            rolls.append(["enemy", int(variant), lvl, area, drops.roll_enemy(int(variant), lvl, area, rnd=random.randrange)])
for dtype in range(5):
    for idx in range(min(8, len(t["object"][dtype]))):
        rolls.append(["object", dtype, idx, 20, 9, drops.roll_object(dtype, idx, 20, 9, rnd=random.randrange)])
out_doc["drops"] = {"start": k0, "rolls": rolls,
                    "zones": [[int(q), int(z), *drops.zone_area(int(q), int(z))] for q in list(t["zones"])[:10] for z in t["zones"][q]]}

# --- judge: rank, rewards and the result block for every quest of the first counter ---
for q in d["counters"]["0"]["quests"]:
    m = missions.mission(q)
    if not m or not m.get("quest"):
        continue
    quest = m["quest"]
    for elapsed, kills, nflags in ((100, 72, 99), (480, 40, 1), (2000, 0, 0)):
        set_flags = set()
        clear = quest.get("clear") or []
        i = 0
        while i < len(clear):
            if clear[i] == 120:
                set_flags.add(tuple(clear[i + 1:i + 4]))
                i += 4
            elif clear[i] == 100:
                i += 2
            else:
                i += 1
        on = set(list(sorted(set_flags))[:nflags])
        flag = lambda z, mp, ev: (z, mp, ev) in on
        run = {"kills": kills, "deaths": 1, "apc_downs": {}}
        rank, total, secs = judge.rank_of(quest, elapsed, run, flag)
        got = judge.rewards(quest, rank, flag)
        slots = [dict(uuid=7 + n, id=r["item"], qty=max(1, r["qty"]), active=0, slots=0, grind=0, pa=[0, 0])
                 for n, r in enumerate(got) if r["item"] in items.db()]
        out_doc["judge"].append({"quest": q, "elapsed": elapsed, "kills": kills, "flags": sorted(on),
                                 "start": judge.evaluate(quest.get("start"), flag),
                                 "clear": judge.evaluate(quest.get("clear"), flag, at=(q, 0, 0)),
                                 "rank": rank, "total": total, "rewards": got,
                                 "block": judge.result_block(rank, total, secs, 1234, slots).hex()})
for table in d["counters"]:
    out_doc["counters"].append({"table": int(table), "options": missions.options(table).hex()})

# --- statuses on a scripted clock: ailments, buffs, ticks, cures, element, technique weapons ---
CLOCK = [1000.0]
combat.time.monotonic = lambda: CLOCK[0]
raw, st = make_char(1, 0, 4)
rod = next(k for k, v in items.db().items() if v["cat"] == 1 and bytes.fromhex(v["consts"])[0xE] == 3)
st["inv"].append({"uuid": 50, "id": rod, "qty": 1, "active": 1, "slots": 0, "grind": 0, "pa": [0, 0],
                  "techs": [[41, 12], [1, 3], [0, 0], [0, 0]]})          # Resta Lv13, Foie Lv4
st["pas"] += [[5, "06050012"], [0, "06010003"]]
p = P(0x02000000, 0, raw, st)
k0 = S.k
log = []
def note(what, *vals):
    log.append([what, CLOCK[0]] + [v.hex() if isinstance(v, bytes) else v for v in vals])
note("inflict poison", combat.inflict(p, 0x2, 1, 100, 10, False, 0), combat.se2(p))
note("inflict burn sure", combat.inflict(p, 0x1, 2, 100, 10, True, 0), combat.se2(p))
note("inflict freeze sure", combat.inflict(p, 0x20, 0, 100, 10, True, 0), combat.se2(p))
note("buffs", combat.apply_buffs(p, 0x800000, 2), combat.se2(p))
mx = progression.derived(raw, st)["max_hp"]
note("buffed stats", combat.buffed_stats(p, [100, 200, 300, 400, 500, 600, 700, 800]))
for t in (1001.0, 1002.5, 1003.0, 1004.0, 1006.0, 1012.0, 1030.0):
    CLOCK[0] = t
    ev, hp = combat.status_step(p, st["hp"], mx)
    st["hp"] = hp
    note("step", [list(e) for e in ev], hp, combat.se2(p))
CLOCK[0] = 1031.0
note("inflict freeze again", combat.inflict(p, 0x20, 1, 100, 10, True, 0), combat.se2(p))
for _ in range(4):
    combat.break_on_hit(p)
    note("break", combat.se2(p))
note("cure", combat.cure(p, 0x3C3FF), combat.se2(p))
D = struct.pack("<IIhhhhBBBB", 0x1204, 0, 50, 0, 400, 0, 1, 50, 0, 0)
note("damage frozen", *combat.damage(D, [10] * 8, 500, None, p))
for codes in ((1, 50, 9, 30), (1, 100, 1, 100), (2, 10, 10, 0), (4, 20, 1, 0), (12, 60, 4, 80)):
    note("element", *codes, combat.element_mult(*codes))
w_resta, w_foie = 50, 50 | (1 << 29)
sup = combat.support_attack(p, w_resta, bytes(20))
note("support", sup[0] if sup else None, *(sup[1:] if sup else []))
st["hp"] = 10
note("apply support", *(combat.apply_support(p, *sup[1:]) or [None]), st["hp"])
note("foie D", combat.player_attack(p, w_foie, bytes(20)))
note("pa hit", *[[g[0], g[1]] if isinstance(g, list) else g for g in (combat.growth_hit(p, w_resta) or (None, None, None))[1:]])
out_doc["status"] = {"raw": raw.hex(), "rod": rod, "start": k0, "log": log}

json.dump(out_doc, open(out, "w"))
print(f"{len(out_doc['battles'])} battles, {len(out_doc['enemies'])} enemies, {len(rolls)} drop rolls, "
      f"{len(out_doc['judge'])} judge cases -> {out}")
