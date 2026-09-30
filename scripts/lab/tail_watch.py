#!/usr/bin/env python3
import glob, re, subprocess, sys, time

pat = sys.argv[1] if len(sys.argv) > 1 else "/tmp/watch_*.log"
n = int(sys.argv[2]) if len(sys.argv) > 2 else 14
skip = re.compile("捕获页面|Sentinel 页面水合|Sentinel 已加载|Sentinel 已改用")
for f in sorted(glob.glob(pat)):
    print("=====", f)
    try:
        lines = open(f, errors="replace").read().splitlines()
    except Exception as e:
        print("read fail", e)
        continue
    lines = [l for l in lines if not skip.search(l)]
    for l in lines[-n:]:
        l = re.sub(r"([A-Za-z0-9._%+-]{2})[A-Za-z0-9._%+-]*(@(hotmail|outlook)\.com)", r"\1***\2", l)
        l = re.sub(r"did=[0-9a-f-]{8,}", "did=***", l)
        print(l[:260])
print("now", time.strftime("%H:%M:%S", time.gmtime()), "UTC")
