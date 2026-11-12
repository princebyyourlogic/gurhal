"""Economy golden vectors (shops, grinding, PA disks, Lumilass, Probo, synthesis) for psud's rules tests.

  python3 tools/golden_econ.py <prettystellaruniverse> <data dir> <out.json>

Same scripted dice stream as golden_combat.py. Derived from disc data: keep it under work/.
"""
import json, os, random, struct, sys

repo, data, out = sys.argv[1:4]
os.environ.update(PSU_GAMEDATA=os.path.join(data, "gamedata"), PSU_NBL_DIR=os.path.join(data, "nbl"),
                  PSU_LOG_DIR=os.path.join(os.path.dirname(out), "pylogs"), PSU_STATE_DIR=os.path.join(os.path.dirname(out), "pystate"))
os.environ.pop("PSU_SHOP_TIER", None)
sys.path[:0] = [os.path.join(repo, "server"), os.path.join(repo, "tools")]
from testplay import RAW_0D02  # noqa: E402
from psu import econ, growth, items, progression, shop, synthesis  # noqa: E402

k = [0]
def nxt():
    v = (k[0] * 7919 + 13) % 1000003
    k[0] += 1
    return v
random.randrange = lambda n: nxt() % n
random.randint = lambda a, b: a + nxt() % (b - a + 1)
random.random = lambda: (nxt() % 1000000) / 1e6

def char(race=0, gender=0, cls=0):
    raw = bytearray(RAW_0D02); raw[0x68], raw[0x69], raw[0x6A] = race, gender, cls; raw = bytes(raw)
    st = progression.new_state(raw)
    st["inv"], st["pas"], st["next_uuid"] = items.starter(raw, 1)
    return raw, st

db = items.db()
doc = {"start": k[0]}
# shops
doc["shops"] = []
for npc in range(len(shop.data()["npcs"])):
    for lvl in (1, 20, 60, 100):
        lst, cat = shop.listing(npc, lvl)
        doc["shops"].append([npc, lvl, cat, [[key, b.hex()] for key, b in (lst or [])],
                             shop.c010a_list(type("D", (), {"lid": 0, "gid": 1})(), lst or [], cat or 0).hex()])
# buy / sell
raw, st = char()
st["meseta"] = 50000
ops = []
for key in [e[0] for e in shop.listing(2, 20)[0]][:4] + [e[0] for e in shop.listing(0, 20)[0]][:2]:
    for q in (1, 3):
        slot, err = shop.buy(st, key, q)
        ops.append(["buy", key, q, slot and slot["uuid"], err, st["meseta"], len(st["inv"])])
for x in list(st["inv"])[-3:]:
    n, gain = shop.sell(st, x["uuid"], 2)
    ops.append(["sell", x["uuid"], 2, n, gain, st["meseta"], len(st["inv"])])
doc["trade"] = {"ops": ops, "inv": st["inv"]}
# grinding
g = econ.data("grind")
grinders = list(g["grinders"])[:3]
weapons = [w for w in g["weapons"] if w in db][:6]
doc["grind"] = []
for w in weapons:
    for gr in grinders:
        ws = {"uuid": 2, "id": w, "qty": 1, "active": 0, "slots": 0, "grind": 0, "pa": [0, 0]}
        doc["grind"].append([w, gr, econ.grind_chance({"id": gr}, ws)])
k0 = k[0]
commits = []
for w in weapons[:3]:
    st = {"inv": [{"uuid": 1, "id": grinders[-1], "qty": 99, "active": 0, "slots": 0, "grind": 0, "pa": [0, 0]},
                  {"uuid": 2, "id": w, "qty": 1, "active": 0, "slots": 0, "grind": 0, "pa": [0, 0]}]}
    for _ in range(12):
        _gr, wp, ok, gi, err = econ.grind(st, 1, 1)
        commits.append([w, ok, err, dict(st["inv"][1])])
doc["grind_commits"] = {"start": k0, "rows": commits}
# PA disks
raw, st = char()
disks = [i for i, v in db.items() if v["cat"] in (4, 5, 6)][:8]
for n, d in enumerate(disks):
    st["inv"].append({"uuid": 100 + n, "id": d, "qty": 1, "active": 0, "slots": 0, "grind": 0, "pa": [0, 0]})
learns = []
for n in range(len(disks) + 1):
    req = bytes(0x2C) + bytes([7 + n % 3, 7, 0x24, 0]) + struct.pack("<I", 100 + n)
    pos, entry, cat, name, err = growth.learn_disk(st, req)
    learns.append([req.hex(), pos, entry, cat, err])
doc["learn"] = {"disks": disks, "rows": learns, "pas": st["pas"], "inv": [x["id"] for x in st["inv"]]}
# Lumilass
doc["lumilass"] = []
for race in range(4):
    for gender in range(2):
        raw, _ = char(race, gender)
        body = bytes(0x28) + struct.pack("<I", 2) + bytes(4) + bytes(range(0x24)) + bytes([9, 8, 7, 6, 5])
        doc["lumilass"].append([raw.hex(), econ.hair_list(raw), econ.apply_lumilass(raw, body).hex(), body.hex()])
# Probo
raw, st = char()
gifts = [i for i, v in db.items() if v["cat"] in (1, 2, 3, 7, 8, 9, 15)][::37][:40]
probo = []
for i in gifts:
    slot = {"uuid": 1, "id": i, "qty": 1, "active": 0, "slots": 0, "grind": 0, "pa": [0, 0]}
    for _ in range(3):
        ev, t, stats, deltas = econ.probo_gift(st, slot)
        probo.append([i, ev, t, list(stats), list(deltas)])
doc["probo"] = probo
# synthesis
k0 = k[0]
bs = synthesis.data()["boards"]
synth = []
raw, st = char()
st["probo"] = {"type": 3, "stats": [250, 120, 400, 30, 0]}
for bid in list(bs)[::7][:30]:
    b = synthesis.lookup(bid)
    e = {"board": bid, "running": 0, "fail": 0, "fail_id": -1, "used": 0, "variant": 0, "slots": synthesis.recipe(b, 0),
         "result": None}
    st["room_stock"] = [{"uuid": 900 + n, "id": m["id"], "qty": 20, "active": 0, "slots": 0, "grind": 0, "pa": [0, 0]}
                        for n, m in enumerate(b["materials"][k2][0] for k2 in range(4)) if m["id"] in db]
    row = [bid, synthesis.rate(st, e, b, 0), synthesis.output_max(st, e, b)]
    res = synthesis.roll(st, e, b)
    synthesis.consume(st, e, b, 0, 1)
    row += [res, [[x["id"], x["qty"]] for x in st["room_stock"]]]
    synth.append(row)
doc["synth"] = {"start": k0, "rows": synth}
json.dump(doc, open(out, "w"))
print(f"{len(doc['shops'])} shop lists, {len(ops)} trades, {len(commits)} grinds, {len(learns)} learns, "
      f"{len(probo)} gifts, {len(synth)} boards -> {out}")
