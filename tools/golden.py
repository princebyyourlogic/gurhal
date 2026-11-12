"""Golden vectors from prettystellaruniverse for psud's rules tests.

  python3 tools/golden.py <prettystellaruniverse> <data dir> <out.json>

Runs the reference Python on characters made from testplay's sample 0x0D02 body (every race, gender and
a few classes) and writes what the Go side must reproduce. The output is derived from disc data: keep it
under work/ (never committed).
"""
import json, os, struct, sys

repo, data, out = sys.argv[1:4]
os.environ.update(PSU_GAMEDATA=os.path.join(data, "gamedata"), PSU_NBL_DIR=os.path.join(data, "nbl"),
                  PSU_CAPTURE_DIR=os.path.join(data, "captures"), PSU_LOG_DIR=os.path.join(os.path.dirname(out), "pylogs"),
                  PSU_STATE_DIR=os.path.join(os.path.dirname(out), "pystate"))
sys.path[:0] = [os.path.join(repo, "server"), os.path.join(repo, "tools")]
from testplay import RAW_0D02                      # noqa: E402
from psu import area, charload, items, progression  # noqa: E402

vectors = []
for race in range(4):
    for gender in range(2):
        for cls in (0, 3, 7, 9, 12):
            raw = bytearray(RAW_0D02)
            raw[0x28:0x68] = f"G{race}{gender}{cls}".encode("utf-16-le").ljust(64, b"\0")
            raw[0x68], raw[0x69], raw[0x6A] = race, gender, cls
            raw = bytes(raw)
            st = progression.new_state(raw)
            st["inv"], st["pas"], st["next_uuid"] = items.starter(raw, 1)
            gid = 0x01020304 + race * 16 + gender * 4 + cls
            vectors.append({
                "raw": raw.hex(), "gid": gid, "state": st,
                "char_bin": charload.char_bin(raw, st).hex(),
                "slot_record": charload.slot_record(raw, st).hex(),
                "status_block": progression.status_block(raw, st).hex(),
                "c0a0a": items.c0a0a_state(gid, st).hex(),
                "user_bin": area.user_bin(gid, 3, raw, (1100000, 0, 4), 2, pos=(1.5, -2.0, 30.25, 90.0), st=st).hex(),
                "c0d01": charload.c0d01_character(gid, raw, None, st=st).hex(),
            })
json.dump(vectors, open(out, "w"), indent=0)
print(f"{len(vectors)} vectors -> {out}")
