"""在服务器执行一组命令；每条命令独立 channel，带超时与重连。
用法: python scripts/srv.py "cmd1" "cmd2" ...
      python scripts/srv.py --put LOCAL REMOTE
"""
import sys, time, paramiko

HOST, USER, PASS = '186.241.91.101', 'root', 'anxaHBIO0642'


def conn():
    last = None
    for _ in range(6):
        try:
            cli = paramiko.SSHClient()
            cli.set_missing_host_key_policy(paramiko.AutoAddPolicy())
            cli.connect(HOST, 22, USER, PASS, timeout=30, look_for_keys=False,
                        allow_agent=False, banner_timeout=45, auth_timeout=30)
            return cli
        except Exception as e:  # noqa
            last = e
            print('[ssh retry]', e, file=sys.stderr)
            time.sleep(3)
    raise SystemExit(f'ssh failed: {last}')


def run(cli, cmd, timeout=60):
    ch = cli.get_transport().open_session()
    ch.settimeout(timeout)
    ch.exec_command(cmd)
    out = b''
    start = time.time()
    while True:
        if ch.recv_ready():
            out += ch.recv(65536)
        if ch.recv_stderr_ready():
            out += ch.recv_stderr(65536)
        if ch.exit_status_ready() and not ch.recv_ready() and not ch.recv_stderr_ready():
            break
        if time.time() - start > timeout:
            out += b'\n[timeout]'
            break
        time.sleep(0.1)
    ch.close()
    return out.decode('utf-8', 'replace')


def main():
    args = sys.argv[1:]
    cli = conn()
    if args and args[0] == '--put':
        sftp = cli.open_sftp()
        sftp.put(args[1], args[2])
        sftp.close()
        print('put ok')
        args = args[3:]
    for c in args:
        print(f'$ {c}')
        print(run(cli, c))
    cli.close()


if __name__ == '__main__':
    main()
