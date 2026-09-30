import sqlite3

con = sqlite3.connect("/opt/chatgpt-register/adskull.db", timeout=30)
cur = con.cursor()
n = cur.execute(
    "update registrations set status='already_registered', note='真浏览器对照实验账号（账号已在 OpenAI 侧创建，凭据未入库）' "
    "where status='registering' and note like 'browser-control-%'"
).rowcount
con.commit()
print("updated", n)
print("registering left:", cur.execute("select count(*) from registrations where status='registering'").fetchone()[0])
