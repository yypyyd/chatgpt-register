import os, sys
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
import srv

cli = srv.conn()
sftp = cli.open_sftp()
sftp.get(sys.argv[1], sys.argv[2])
sftp.close()
cli.close()
print("got", sys.argv[1], "->", sys.argv[2], os.path.getsize(sys.argv[2]))
