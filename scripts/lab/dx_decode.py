"""解码真实抓包里 sentinel/req 的 so.collector_dx / so.snapshot_dx / turnstile.dx 程序并反汇编。"""
import base64, json, os, re, sys

tag = sys.argv[1] if len(sys.argv) > 1 else ""
which = int(sys.argv[2]) if len(sys.argv) > 2 else 0  # 第几个 sentinel/req
p = os.path.join(os.environ["TEMP"], "ctl_net%s.jsonl" % tag)
recs = [json.loads(l) for l in open(p, encoding="utf-8")]
reqs = {r["id"]: r for r in recs if r["k"] == "req"}
bodies = [r for r in recs if r["k"] == "body" and "sentinel/req" in r["url"]]
b = bodies[which]
rq = reqs[b["id"]]
preq = json.loads(rq["body"])
key = preq["p"]
resp = json.loads(b["body"])
print("flow:", preq.get("flow"), "resp keys:", list(resp.keys()))
print("so keys:", list((resp.get("so") or {}).keys()), "required:", (resp.get("so") or {}).get("required"))
print("turnstile keys:", list((resp.get("turnstile") or {}).keys()))
print("pow:", {k: (str(v)[:30]) for k, v in (resp.get("proofofwork") or {}).items()})


def xor(s, k):
    return "".join(chr(ord(c) ^ ord(k[i % len(k)])) for i, c in enumerate(s))


def dec(dx, key):
    raw = base64.b64decode(dx + "=" * (-len(dx) % 4)).decode("latin1")
    return json.loads(xor(raw, key))


OPS = {0: "RUN", 1: "XOR", 2: "SET", 3: "RESOLVE", 4: "REJECT", 5: "APPEND", 6: "GET", 7: "CALL", 8: "COPY", 9: "QUEUE", 10: "WINDOW",
       11: "SCRIPT_MATCH", 12: "REGS", 13: "CALL_TRY", 14: "JSON_PARSE", 15: "JSON_STR", 16: "KEY", 17: "CALL_STORE", 18: "ATOB", 19: "BTOA",
       20: "IF_EQ", 21: "IF_DIFF", 22: "RUN_LIST", 23: "IF_DEF", 24: "BIND", 25: "NOP", 26: "NOP", 27: "SUB/REMOVE", 28: "NOP", 29: "LT", 30: "DEFFN",
       33: "MUL", 34: "AWAIT", 35: "DIV"}


def show(name, dx):
    try:
        prog = dec(dx, key)
    except Exception as e:
        print(name, "decode failed:", e)
        return
    print("\n=====", name, "instructions:", len(prog))
    for i, ins in enumerate(prog):
        op = ins[0]
        print("%3d %-12s %s" % (i, OPS.get(op, op), json.dumps(ins[1:], ensure_ascii=False)[:200]))


so = resp.get("so") or {}
if so.get("collector_dx"):
    show("collector_dx", so["collector_dx"])
if so.get("snapshot_dx"):
    show("snapshot_dx", so["snapshot_dx"])
if len(sys.argv) > 3 and sys.argv[3] == "t" and (resp.get("turnstile") or {}).get("dx"):
    show("turnstile.dx", resp["turnstile"]["dx"])

