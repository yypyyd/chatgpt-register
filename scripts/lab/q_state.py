"""线上库里实验账号与生产状态概览（只读，不输出任何凭据）。在服务器上运行。"""
import sqlite3

con = sqlite3.connect("file:/opt/chatgpt-register/adskull.db?mode=ro", uri=True)
cur = con.cursor()
print("registering rows:", cur.execute("select count(*) from registrations where status='registering'").fetchone()[0])
print("-- experiment rows by note/status")
for r in cur.execute("select coalesce(substr(note,1,26),''), status, coalesce(access_token_status,''), count(*) from registrations "
                     "where note like 'gptlive%' or note like 'browser-control%' group by 1,2,3 order by 1"):
    print(r)
print("-- last 12 registrations (id, status, at_status, created)")
for r in cur.execute("select id, status, coalesce(access_token_status,''), substr(created_at,1,19), coalesce(substr(note,1,28),'') from registrations order by id desc limit 12"):
    print(r)
