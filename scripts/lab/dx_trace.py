"""递归反汇编 dx（含函数体/嵌套指令列表），标出访问路径。用法: python dx_trace.py <pairs.json> <idx> <dx|collector_dx|snapshot_dx>"""
import base64, json, sys

pairs = json.load(open(sys.argv[1]))
pair = pairs[int(sys.argv[2])]
which = sys.argv[3]


def xor(s, k):
    return "".join(chr(ord(c) ^ ord(k[i % len(k)])) for i, c in enumerate(s))


prog = json.loads(xor(base64.b64decode(pair[which] + "=" * (-len(pair[which]) % 4)).decode("latin1"), pair["p"]))
BASE = {0: "RUN", 1: "XOR", 2: "SET", 3: "RESOLVE", 4: "REJECT", 5: "APPEND", 6: "GET", 7: "CALL", 8: "COPY", 9: "QUEUE", 10: "WINDOW",
        11: "SCRIPT_MATCH", 12: "REGS", 13: "CALL_TRY", 14: "JSON_PARSE", 15: "JSON_STR", 16: "KEY", 17: "CALL_STORE", 18: "ATOB", 19: "BTOA",
        20: "IF_EQ", 21: "IF_DIFF", 22: "RUN_LIST", 23: "IF_DEF", 24: "BIND", 25: "NOP", 26: "NOP", 27: "SUB", 28: "NOP", 29: "LT",
        30: "DEFFN", 33: "MUL", 34: "AWAIT", 35: "DIV"}
alias, vals, paths = {}, {}, {10: "window"}


def opname(o):
    if isinstance(o, (int, float)) and float(o).is_integer() and int(o) in BASE and o not in alias:
        return BASE[int(o)]
    return alias.get(o, "?%s" % o)


def is_prog(x):
    return isinstance(x, list) and x and all(isinstance(i, list) and i for i in x)


def walk(lst, depth):
    for ins in lst:
        name = opname(ins[0])
        args = ins[1:]
        pad = "  " * depth
        note = ""
        if name == "COPY" and len(args) == 2:
            dst, src = args
            if isinstance(src, (int, float)) and float(src).is_integer() and int(src) in BASE:
                alias[dst] = BASE[int(src)]
            elif src in alias:
                alias[dst] = alias[src]
            elif src in paths:
                paths[dst] = paths[src]
            elif src in vals:
                vals[dst] = vals[src]
            note = "alias/copy"
        elif name == "SET" and len(args) == 2 and not is_prog(args[1]):
            vals[args[0]] = args[1]
            note = "%s := %s" % (args[0], json.dumps(args[1], ensure_ascii=False)[:70])
        elif name in ("GET", "BIND") and len(args) == 3:
            dst, obj, key = args
            paths[dst] = "%s.%s" % (paths.get(obj, "<%s>" % obj), vals.get(key, "?"))
            note = "%s = %s" % (dst, paths[dst])
        elif name in ("CALL_STORE",) and len(args) >= 2:
            dst, fn = args[0], args[1]
            argp = ",".join(json.dumps(vals[a], ensure_ascii=False)[:30] if a in vals else str(paths.get(a, a)) for a in args[2:])
            paths[dst] = "%s(%s)" % (paths.get(fn, "<%s>" % fn), argp)
            note = "%s = %s" % (dst, paths[dst])
        elif name in ("CALL", "CALL_TRY") and args:
            note = "call %s" % paths.get(args[-1] if name == "CALL_TRY" else args[0], args)
        flat = [a if not is_prog(a) else "[nested %d]" % len(a) for a in args]
        if name not in ("COPY",) or True:
            print("%s%-10s %s  %s" % (pad, name, json.dumps(flat, ensure_ascii=False)[:90], note))
        for a in args:
            if is_prog(a):
                walk(a, depth + 1)
            elif isinstance(a, list) and a and all(isinstance(i, list) for i in a) is False:
                pass


walk(prog, 0)
