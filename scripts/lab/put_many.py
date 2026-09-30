import os, sys
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
import srv

cli = srv.conn()
sftp = cli.open_sftp()
for pair in sys.argv[1:]:
    local, remote = pair.split("=>")
    sftp.put(local, remote)
    print("put", local, "->", remote)
sftp.close()
print(srv.run(cli, "chmod +x /opt/chatgpt-register/gptlive7 2>/dev/null; md5sum /opt/chatgpt-register/scripts/sentinel_node.js /opt/chatgpt-register/scripts/font_metrics.js /opt/chatgpt-register/scripts/nav_keys.js; node /opt/chatgpt-register/scripts/sentinel_node.js --selftest"))
cli.close()

