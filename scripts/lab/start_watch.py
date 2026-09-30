#!/usr/bin/env python3
"""在服务器上错峰启动多组 gptlive watch 实验，日志写 /tmp/watch_<label>.log。
用法: python3 start_watch.py <bin> <label1,label2,...> [first] [every] [total] [gap_sec]
"""
import os
import subprocess
import sys
import time

BIN = sys.argv[1] if len(sys.argv) > 1 else "/opt/chatgpt-register/gptlive3"
labels = (sys.argv[2] if len(sys.argv) > 2 else "N1,N2").split(",")
first = sys.argv[3] if len(sys.argv) > 3 else "60s"
every = sys.argv[4] if len(sys.argv) > 4 else "60s"
total = sys.argv[5] if len(sys.argv) > 5 else "40m"
gap = float(sys.argv[6]) if len(sys.argv) > 6 else 20
os.environ["HOME"] = "/root"

for label in labels:
    log = open(f"/tmp/watch_{label}.log", "w")
    p = subprocess.Popen(
        [BIN, "-mode", "watch", "-domain", "hotmail.com", "-label", label,
         "-first", first, "-every", every, "-total", total, "-cookies=true",
         "-timeout", "70m"],
        stdout=log, stderr=subprocess.STDOUT, start_new_session=True, cwd="/opt/chatgpt-register",
    )
    print("started", label, p.pid, flush=True)
    time.sleep(gap)
