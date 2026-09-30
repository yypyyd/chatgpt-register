"""线上生产路径验证辅助（只读，不输出任何凭据/邮箱明文）。

在服务器上运行:
    python3 q_prod.py stock            # 邮箱库存概况
    python3 q_prod.py recent [N]       # 最近 N 条注册记录状态
    python3 q_prod.py log <email前缀> # 打印该账号日志里与 Sentinel/会话观察器相关的行
"""
import re
import sqlite3
import sys

DB = "file:/opt/chatgpt-register/adskull.db?mode=ro"


def mask(s: str) -> str:
    if "@" not in s:
        return s[:3] + "***"
    name, dom = s.split("@", 1)
    return name[:3] + "***@" + dom


def main() -> None:
    con = sqlite3.connect(DB, uri=True)
    cur = con.cursor()
    mode = sys.argv[1] if len(sys.argv) > 1 else "stock"
    if mode == "stock":
        print("mailboxes by status:", cur.execute("select status, count(*) from mailboxes group by 1").fetchall())
        n = cur.execute(
            "select count(*) from mailboxes m where m.status='verified' and not exists "
            "(select 1 from registrations r where r.email=m.email and r.status='registered')"
        ).fetchone()[0]
        print("verified mailboxes whose mother is not registered:", n)
        print("registrations by status:", cur.execute("select status, count(*) from registrations group by 1").fetchall())
        print("registering rows:", cur.execute("select count(*) from registrations where status='registering'").fetchone()[0])
        for k in ("max_concurrency", "fission_count", "mailbox_interval", "launch_stagger", "retry_cooldown"):
            row = cur.execute("select value from settings where key=?", (k,)).fetchone()
            print(f"setting {k} =", row[0] if row else None)
    elif mode == "recent":
        n = int(sys.argv[2]) if len(sys.argv) > 2 else 10
        for r in cur.execute(
            "select id, email, status, coalesce(access_token_status,''), substr(created_at,1,19), coalesce(substr(note,1,30),'') "
            "from registrations order by id desc limit ?", (n,)
        ):
            print((r[0], mask(r[1]), r[2], r[3], r[4], r[5]))
    elif mode == "log":
        prefix = sys.argv[2]
        row = cur.execute(
            "select id, email, status, coalesce(access_token_status,''), coalesce(log,''), coalesce(note,'') "
            "from registrations where email like ? order by id desc limit 1", (prefix + "%",)
        ).fetchone()
        if not row:
            print("not found")
            return
        rid, email, status, ats, log, note = row
        print("id", rid, mask(email), "status", status, "at_status", ats)
        print("note:", note[:200])
        pat = re.compile(r"Session Observer|sentinel|Sentinel|so_empty|sentinel_t|token_revoked|验证|注册成功|会话|session", re.I)
        for line in log.splitlines():
            if pat.search(line):
                # 日志已由服务端脱敏；这里再截断，避免意外长串
                print(line[:220])
    else:
        print("unknown mode")


if __name__ == "__main__":
    main()
