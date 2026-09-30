#!/usr/bin/env python3
import os
import signal
import subprocess
import time

os.environ["DISPLAY"] = ":111"
os.environ["HOME"] = "/root"
for line in os.popen("ps -eo pid,cmd").read().splitlines():
    if "/opt/chatgpt-register/gptlive" in line and "python" not in line:
        os.kill(int(line.split()[0]), signal.SIGTERM)
open("/tmp/gptlive.out", "w").close()
p = subprocess.Popen(
    ["/opt/chatgpt-register/gptlive", "-mode", "register", "-domain", "hotmail.com", "-timeout", "35m"],
    stdout=open("/tmp/gptlive.out", "a"),
    stderr=subprocess.STDOUT,
    start_new_session=True,
    cwd="/opt/chatgpt-register",
)
print("started", p.pid)
time.sleep(4)
print(open("/tmp/gptlive.out", errors="replace").read())
