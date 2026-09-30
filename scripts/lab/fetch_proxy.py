"""把服务器上的 proxy_list 首项写到本机 TEMP（不打印凭据），供对照实验的本地代理桥使用。"""
import os, sys, sqlite3, io, paramiko

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
import srv  # noqa: E402

REMOTE = r'''
import sqlite3
con = sqlite3.connect("file:/opt/chatgpt-register/adskull.db?mode=ro", uri=True)
v = con.execute("select value from settings where key='proxy_list'").fetchone()[0]
line = [x.strip() for x in v.replace(",", "\n").split("\n") if x.strip()][0]
print("PROXY=" + line)
'''

cli = srv.conn()
sftp = cli.open_sftp()
with sftp.open("/tmp/_getproxy.py", "w") as f:
    f.write(REMOTE)
sftp.close()
out = srv.run(cli, "python3 /tmp/_getproxy.py; rm -f /tmp/_getproxy.py")
cli.close()
line = [l for l in out.splitlines() if l.startswith("PROXY=")]
if not line:
    print("no proxy found")
    sys.exit(1)
dst = os.path.join(os.environ["TEMP"], "bestgo_proxy.txt")
with open(dst, "w") as f:
    f.write(line[0][len("PROXY="):])
print("saved proxy to TEMP (len=%d)" % len(line[0]))
