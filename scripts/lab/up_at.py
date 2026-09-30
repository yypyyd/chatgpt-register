"""把本机对照浏览器账号的 AT 上传到服务器 /tmp/at_<tag>.txt，并启动 gptlive watchat 观察（浏览器已关闭后 AT 是否还活着）。"""
import os, sys, time
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
import srv

TMP = os.environ["TEMP"]
items = [("B1", "ctl_at.txt"), ("X4", "ctl_at_x4.txt"), ("X7", "ctl_at_x7.txt"), ("X5", "ctl_at_x5.txt")]
only = sys.argv[1:]
cli = srv.conn()
sftp = cli.open_sftp()
for tag, fname in items:
    if only and tag not in only:
        continue
    p = os.path.join(TMP, fname)
    if not os.path.exists(p):
        print("skip", tag)
        continue
    sftp.put(p, "/tmp/at_%s.txt" % tag)
    cmd = ("nohup /opt/chatgpt-register/gptlive3 -mode watchat -label %s-closed -at-file /tmp/at_%s.txt "
           "-first 10s -every 90s -total 60m -timeout 70m > /tmp/watch_%s-closed.log 2>&1 &") % (tag, tag, tag)
    print(srv.run(cli, cmd + " sleep 1; echo started " + tag))
sftp.close()
cli.close()
