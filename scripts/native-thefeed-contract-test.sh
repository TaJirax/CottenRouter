#!/usr/bin/env bash
# Exercise the unmodified native Feed wizard and real server in disposable
# mount/PID/network namespaces. Package management, release transport and the
# systemd boundary are shimmed; service commands execute the generated unit.
#
# COTTENROUTER_NAMESPACE_TEST=1 THEFEED_INSTALLER=/path/to/scripts/install.sh \
#   THEFEED_BINARY=/path/to/thefeed-server-linux-amd64 \
#   COTTENROUTER_BINARY=/path/to/cottenrouter-linux-amd64 \
#   bash scripts/native-thefeed-contract-test.sh
set -euo pipefail
[[ ${COTTENROUTER_NAMESPACE_TEST:-} == 1 && $EUID == 0 ]] || {
  echo 'Requires root and COTTENROUTER_NAMESPACE_TEST=1' >&2; exit 2;
}
native=$(realpath "${THEFEED_BINARY:?set THEFEED_BINARY}")
installer=$(realpath "${THEFEED_INSTALLER:?set THEFEED_INSTALLER}")
router=$(realpath "${COTTENROUTER_BINARY:?set COTTENROUTER_BINARY}")
[[ -f $native && -f $installer && -f $router ]] || exit 2
scratch=$(mktemp -d /tmp/cottenrouter-native-thefeed.XXXXXXXX)
cleanup() {
  case "$scratch" in
    /tmp/cottenrouter-native-thefeed.*) rm -rf -- "$scratch" ;;
    *) echo 'Refusing unsafe cleanup path' >&2; return 1 ;;
  esac
}
trap cleanup EXIT
mkdir -p "$scratch/etc" "$scratch/local-bin"
for name in passwd group nsswitch.conf os-release; do
  [[ ! -f /etc/$name ]] || cp -L "/etc/$name" "$scratch/etc/$name"
done
printf '127.0.0.1 localhost\n' > "$scratch/etc/hosts"
printf 'nameserver 127.0.0.1\n' > "$scratch/etc/resolv.conf"
cp "$native" "$scratch/native-server"
# A Windows checkout may convert LF to CRLF; normalize transport line endings
# without changing the upstream installer's instructions.
sed 's/\r$//' "$installer" > "$scratch/native-install.sh"
cp "$router" "$scratch/local-bin/cottenrouter"
cat > "$scratch/local-bin/curl" <<'CURL'
#!/usr/bin/python3
import os, pathlib, shutil, sys
args = sys.argv[1:]
url = next((arg for arg in args if arg.startswith('https://')), '')
if not url.startswith('https://github.com/sartoopjj/thefeed/releases/download/'):
    raise SystemExit('Unexpected external request in isolated test: ' + url)
output = next((args[index+1] for index, arg in enumerate(args[:-1])
               if arg == '--output' or arg.startswith('-') and arg.endswith('o')), None)
if not output:
    raise SystemExit('Missing release output argument')
shutil.copyfile(pathlib.Path(os.environ['CONTRACT_SCRATCH']) / 'native-server', output)
CURL
cat > "$scratch/local-bin/systemctl" <<'SYSTEMCTL'
#!/usr/bin/python3
import os, pathlib, re, shlex, signal, subprocess, sys, time
args = sys.argv[1:]
with open('/run/native-systemctl.log', 'a') as log:
    log.write(' '.join(args) + '\n')
action = args[0] if args else ''
names = [arg.removesuffix('.service') for arg in args[1:] if not arg.startswith('-')]
name = names[0] if names else ''
pidfile = pathlib.Path('/run/' + name + '.pid')
def pid():
    try:
        value = int(pidfile.read_text())
        if pathlib.Path('/proc/' + str(value) + '/stat').read_text().split()[2] == 'Z':
            return None
        return value
    except (OSError, ValueError):
        return None
def stop():
    value = pid()
    if value:
        os.kill(value, signal.SIGTERM)
        for _ in range(100):
            if not pid():
                break
            time.sleep(.02)
        if pid():
            os.kill(value, signal.SIGKILL)
    pidfile.unlink(missing_ok=True)
def start():
    if name not in ('thefeed-server', 'cottenrouter'):
        return
    if pid():
        return
    env = dict(os.environ)
    if name == 'thefeed-server':
        data = pathlib.Path('/opt/thefeed/data/thefeed.env').read_text()
        for line in data.splitlines():
            if '=' in line and not line.startswith('#'):
                key, value = line.split('=', 1)
                env[key] = value
        unit = pathlib.Path('/etc/systemd/system/thefeed-server.service').read_text()
        unit = re.sub(r'\\\n\s*', ' ', unit)
        command, = [line.removeprefix('ExecStart=') for line in unit.splitlines() if line.startswith('ExecStart=')]
        command = re.sub(r'\$\{([A-Za-z_][A-Za-z0-9_]*)\}', lambda m: env.get(m[1], ''), command)
        argv = shlex.split(command)
    else:
        argv = ['/usr/local/bin/cottenrouter', 'serve', '--config', '/etc/cottenrouter/config.json']
    with open('/run/' + name + '.log', 'a') as log:
        child = subprocess.Popen(argv, env=env, stdout=log, stderr=log, start_new_session=True)
    pidfile.write_text(str(child.pid))
    time.sleep(.1)
if action == 'is-active':
    print('active' if pid() else 'inactive')
    raise SystemExit(0 if pid() else 3)
if action == 'is-enabled':
    print('enabled' if name in ('thefeed-server', 'cottenrouter') else 'disabled')
    raise SystemExit(0 if name in ('thefeed-server', 'cottenrouter') else 1)
if action in ('restart', 'try-restart'):
    stop(); start()
elif action == 'start' or action == 'enable' and '--now' in args:
    start()
elif action == 'stop' or action == 'disable' and '--now' in args:
    stop()
SYSTEMCTL
for name in apt-get dnf yum pacman apk iptables ip6tables; do
  printf '#!/bin/sh\nexit 0\n' > "$scratch/local-bin/$name"
done
for name in ufw firewall-cmd wget; do
  printf '#!/bin/sh\nexit 1\n' > "$scratch/local-bin/$name"
done
chmod 0755 "$scratch/native-server" "$scratch/local-bin/"*
export CONTRACT_SCRATCH="$scratch"
unshare --mount --net --pid --fork --mount-proc bash -euo pipefail <<'NAMESPACE'
mount --make-rprivate /
mount --bind "$CONTRACT_SCRATCH/etc" /etc
mount --bind "$CONTRACT_SCRATCH/local-bin" /usr/local/bin
mount -t tmpfs tmpfs /run
mount -t tmpfs tmpfs /opt
mount -t tmpfs tmpfs /var/lib
mkdir -p /etc/systemd/system /etc/cottenrouter /run/systemd/system
export PATH=/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ip link set lo up
python3 - <<'PY'
import hashlib
import json
import os
import pathlib
import pty
import re
import select
import signal
import socket
import struct
import subprocess
import time

root = pathlib.Path(os.environ['CONTRACT_SCRATCH'])
installer = root / 'native-install.sh'
ansi = re.compile(rb'\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))')
pid, fd = pty.fork()
if pid == 0:
    os.execv('/bin/bash', ['bash', str(installer), '--github', '--version', 'v0.0.0-contract'])
buffer = b''
log = bytearray()
def read():
    global buffer
    ready, _, _ = select.select([fd], [], [], .1)
    if ready:
        try:
            data = os.read(fd, 65536)
        except OSError:
            data = b''
        if not data:
            return False
        log.extend(data)
        buffer = ansi.sub(b'', buffer + data)[-131072:]
    return True
def expect(prompt, answer):
    global buffer
    deadline = time.monotonic() + 30
    target = prompt.encode()
    while time.monotonic() < deadline:
        at = buffer.find(target)
        if at >= 0:
            buffer = buffer[at + len(target):]
            os.write(fd, (answer + '\n').encode())
            return
        if not read():
            break
    raise RuntimeError('Native installer stopped before prompt: ' + prompt)
try:
    for prompt, answer in [
        ('Channel: ', 'telegram'), ('Channel: ', ''), ('X: ', ''),
        ('Add private channel invite links?', 'n'),
        ('Main DNS domain (e.g., t.example.com): ', 'feed.native.test'),
        ('Add extra domains?', 'y'), ('  Domain: ', 'alias.native.test'), ('  Domain: ', ''),
        ('Enable the messenger?', 'y'), ('  Domain: ', 'chat.native.test'), ('  Domain: ', ''),
        ('Change messenger limits?', 'n'), ('Enable the Telegram proxy?', 'y'),
        ('  Domain: ', 'mtproto.native.test'), ('  Domain: ', ''),
        ('Max concurrent Telegram proxy users', '32'),
        ('Telegram proxy sessions kept at once', '1500'),
        ('Telegram proxy retry memory per session, KB', '64'),
        ('Telegram proxy retry memory for all sessions, MB', '64'),
        ('Encryption passphrase: ', 'native-test-only'), ('Max messages per channel', ''),
        ('Fetch cycle interval', ''), ('Enable remote management?', 'n'),
        ('Enable DNS relay?', 'y'), ('DNS relay max file size in KB', '160'),
        ('DNS relay TTL in minutes', '60'), ('DNS relay compression', 'gzip'),
        ('Enable GitHub relay?', 'n'), ('Choose mode [1/2]', '1'),
        ('DNS listen address', '127.0.0.1:5304'),
    ]:
        expect(prompt, answer)
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        finished, status = os.waitpid(pid, os.WNOHANG)
        if finished:
            pid = None
            assert os.waitstatus_to_exitcode(status) == 0, 'Native installer exited unsuccessfully'
            break
        read()
    else:
        raise RuntimeError('Native installer did not finish')
except BaseException:
    print(ansi.sub(b'', bytes(log))[-12000:].decode(errors='replace'))
    raise
finally:
    if pid is not None:
        os.killpg(pid, signal.SIGTERM)
        os.waitpid(pid, 0)
    os.close(fd)

env_path = pathlib.Path('/opt/thefeed/data/thefeed.env')
def settings():
    return dict(line.split('=', 1) for line in env_path.read_text().splitlines() if '=' in line and not line.startswith('#'))
before = settings()
for key, expected in {
    'THEFEED_DOMAIN': 'feed.native.test', 'THEFEED_EXTRA_DOMAINS': 'alias.native.test',
    'THEFEED_CHAT_DOMAINS': 'chat.native.test', 'THEFEED_MTPROTO_DOMAINS': 'mtproto.native.test',
    'THEFEED_MTPROTO_MAX_TUNNELS': '32', 'THEFEED_MTPROTO_MAX_SESSIONS': '1500',
    'THEFEED_MTPROTO_REPLY_SESSION_KB': '64', 'THEFEED_MTPROTO_REPLY_BUDGET_MB': '64',
    'THEFEED_DNS_MEDIA_ENABLED': '1', 'THEFEED_DNS_MEDIA_MAX_SIZE_KB': '160',
    'THEFEED_DNS_MEDIA_CACHE_TTL_MIN': '60', 'THEFEED_DNS_MEDIA_COMPRESSION': 'gzip',
    'THEFEED_LISTEN': '127.0.0.1:5304', 'THEFEED_NO_TELEGRAM': '1',
}.items():
    assert before[key] == expected, 'Native feature setting changed: ' + key
assert pathlib.Path('/opt/thefeed/thefeed-server').read_bytes() == (root / 'native-server').read_bytes()
unit = pathlib.Path('/etc/systemd/system/thefeed-server.service').read_text()
assert 'EnvironmentFile=/opt/thefeed/data/thefeed.env' in unit
assert 'WorkingDirectory=/opt/thefeed' in unit and '--no-telegram' in unit
native_files = {path.name: hashlib.sha256(path.read_bytes()).hexdigest()
                for path in env_path.parent.iterdir() if path.is_file() and 'key' in path.name}
assert native_files, 'Native signing identity was not generated'
config_path = pathlib.Path('/etc/cottenrouter/config.json')
config_path.write_text(json.dumps({'listen_udp': '0.0.0.0:53', 'listen_tcp': '0.0.0.0:53', 'routes': []}))
subprocess.run(['cottenrouter', 'configure', '--project', 'thefeed', '--domain', 'feed.native.test',
                '--extra-domains', 'alias.native.test', '--chat-domains', 'chat.native.test', '--port', '5304'], check=True)
after = settings()
assert all(after[key] == value for key, value in before.items()), 'Router changed native feature settings'
assert all(hashlib.sha256((env_path.parent / name).read_bytes()).hexdigest() == digest
           for name, digest in native_files.items()), 'Router changed native identity'
config = json.loads(config_path.read_text())
route, = config['routes']
domains = ['feed.native.test', 'alias.native.test', 'chat.native.test', 'mtproto.native.test']
assert set(route['domains']) == set(domains), route
assert route['backend'] == '127.0.0.1:5304'
subprocess.run(['cottenrouter', 'check', '--config', str(config_path)], check=True, stdout=subprocess.DEVNULL)

def query(domain):
    # Deliberately opaque unsupported cells: the backend, not CottenRouter,
    # decides their protocol meaning and returns its native NXDOMAIN reply.
    name = 'opaque-probe.' + domain
    return (struct.pack('!6H', 0x7163, 0x100, 1, 0, 0, 0)
            + b''.join(bytes([len(label)]) + label.encode() for label in name.split('.'))
            + b'\0' + struct.pack('!HH', 16, 1))
def exchange(port, packet):
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as connection:
        connection.settimeout(2)
        connection.connect(('127.0.0.1', port))
        connection.send(packet)
        reply = connection.recv(65535)
    assert reply[:2] == packet[:2] and reply[2] & 0x80 and reply[3] & 15 == 3
    return reply
for domain in domains:
    packet = query(domain)
    assert exchange(53, packet) == exchange(5304, packet), 'Native response changed on ' + domain
print(json.dumps({
    'native_server_version': subprocess.check_output(['/opt/thefeed/thefeed-server', '--version'], text=True).strip(),
    'native_binary_sha256': hashlib.sha256((root / 'native-server').read_bytes()).hexdigest(),
    'native_installer_sha256': hashlib.sha256(installer.read_bytes()).hexdigest(),
    'namespace_isolation': ['mount', 'network', 'pid'], 'unmodified_native_wizard': 'passed',
    'native_binary_data_and_unit_paths': 'passed', 'real_generated_service_command': 'passed',
    'native_settings_and_identity_preserved': True, 'routed_feature_domains': domains,
    'native_vs_routed_opaque_udp_byte_exact': True,
    'shimmed_boundaries': ['systemd', 'package_management', 'release_download'],
    'not_exercised': ['Telegram_login', 'MTProto_outbound', 'media_downloads', 'systemd_daemon'],
}, indent=2))
PY
NAMESPACE
