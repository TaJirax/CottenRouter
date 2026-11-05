#!/usr/bin/env bash
# Run the real native SlipGate CLI in disposable mount/PID/network namespaces.
# Only systemd/firewall boundaries are shimmed; generated config, keys, service
# artifacts and the Soar server are real. Nothing changes the host's DNS setup.
#
# COTTENROUTER_NAMESPACE_TEST=1 SLIPGATE_BINARY=/path/to/slipgate-linux-amd64 \
#   COTTENROUTER_BINARY=/path/to/cottenrouter-linux-amd64 \
#   bash scripts/native-slipgate-contract-test.sh
set -euo pipefail
[[ ${COTTENROUTER_NAMESPACE_TEST:-} == 1 && $EUID == 0 ]] || {
  echo 'Requires root and COTTENROUTER_NAMESPACE_TEST=1' >&2; exit 2;
}
native=$(realpath "${SLIPGATE_BINARY:?set SLIPGATE_BINARY}")
router=$(realpath "${COTTENROUTER_BINARY:?set COTTENROUTER_BINARY}")
[[ -f $native && -f $router ]] || exit 2
scratch=$(mktemp -d /tmp/cottenrouter-native-slipgate.XXXXXXXX)
cleanup() {
  case "$scratch" in
    /tmp/cottenrouter-native-slipgate.*) rm -rf -- "$scratch" ;;
    *) echo "Refusing unsafe cleanup path" >&2; return 1 ;;
  esac
}
trap cleanup EXIT
mkdir -p "$scratch/etc" "$scratch/local-bin"
for name in passwd group nsswitch.conf os-release; do
  [[ ! -f /etc/$name ]] || cp -L "/etc/$name" "$scratch/etc/$name"
done
printf '127.0.0.1 localhost\n' > "$scratch/etc/hosts"
printf 'nameserver 127.0.0.1\n' > "$scratch/etc/resolv.conf"
cp "$native" "$scratch/local-bin/slipgate"
cp "$router" "$scratch/local-bin/cottenrouter"
chmod 0755 "$scratch/local-bin/slipgate" "$scratch/local-bin/cottenrouter"
cat > "$scratch/local-bin/systemctl" <<'SHIM'
#!/bin/sh
printf '%s\n' "$*" >> /run/native-systemctl.log
case "$1" in
  is-active) echo inactive; exit 3 ;;
  is-enabled) echo disabled; exit 1 ;;
  *) exit 0 ;;
esac
SHIM
for name in ufw firewall-cmd curl wget; do
  printf '#!/bin/sh\nexit 1\n' > "$scratch/local-bin/$name"
done
printf '#!/bin/sh\nexit 0\n' > "$scratch/local-bin/iptables"
chmod 0755 "$scratch/local-bin/"*
export CONTRACT_SCRATCH="$scratch"
unshare --mount --net --pid --fork --mount-proc bash -euo pipefail <<'NAMESPACE'
mount --make-rprivate /
mount --bind "$CONTRACT_SCRATCH/etc" /etc
mount --bind "$CONTRACT_SCRATCH/local-bin" /usr/local/bin
mount -t tmpfs tmpfs /run
mkdir -p /etc/slipgate /etc/systemd/system /run/systemd/system
export PATH=/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ip link set lo up
python3 - <<'PY'
import base64
import json
import os
import pathlib
import shlex
import socket
import struct
import subprocess
import time

root = pathlib.Path(os.environ['CONTRACT_SCRATCH'])
native_config = pathlib.Path('/etc/slipgate/config.json')
native_config.write_text(json.dumps({
    'listen': {'address': '0.0.0.0:53'}, 'tunnels': [],
    'backends': [{'tag': 'ssh', 'type': 'ssh', 'address': '127.0.0.1:22'}],
    'users': [], 'route': {'mode': 'multi', 'active': '', 'default': ''},
    'warp': {'enabled': False}, 'public_address': '192.0.2.10',
}))
result = subprocess.run([
    'slipgate', 'tunnel', 'add', '--transport', 'soar', '--backend', 'direct',
    '--tag', 'soar', '--domain', 'soar.native.test', '--allow-ports', '80,443',
], stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=30)
if result.returncode:
    raise RuntimeError('Native SlipGate tunnel add failed: ' + result.stderr[-1000:])
configuration = json.loads(native_config.read_text())
tunnel, = configuration['tunnels']
assert tunnel['tag'] == 'soar' and tunnel['transport'] == 'soar'
assert tunnel['backend'] == 'direct' and tunnel['port'] == 5310
assert configuration['public_address'] == '192.0.2.10'
assert tunnel['soar']['private_key'] == '/etc/slipgate/tunnels/soar/server.key'
assert len(base64.b64decode(tunnel['soar']['public_key'])) == 32
assert pathlib.Path(tunnel['soar']['private_key']).is_file()
unit = pathlib.Path('/etc/systemd/system/slipgate-soar.service').read_text()
command, = [line.removeprefix('ExecStart=') for line in unit.splitlines() if line.startswith('ExecStart=')]
assert command.startswith('/usr/local/bin/slipgate soar serve --listen 127.0.0.1:5310 ')
assert '--allow-ports 80,443' in command
assert pathlib.Path('/etc/systemd/system/slipgate-dnsrouter.service').is_file()
keys = subprocess.check_output(['cottenrouter', 'keys', '--project', 'slipgate'], text=True)
assert tunnel['soar']['public_key'] in keys
before = native_config.read_bytes()
router_config = root / 'router.json'
subprocess.run(['cottenrouter', 'slipgate-import', '--input', str(native_config), '--output', str(router_config)], check=True)
imported = json.loads(router_config.read_text())
assert imported['listen_tcp'] == '0.0.0.0:53'
route, = imported['routes']
assert route['name'] == 'slipgate:soar' and route['backend'] == '127.0.0.1:5310'
assert route['tcp_backend'] == 'udp' and 'verify' not in route
assert native_config.read_bytes() == before
subprocess.run(['cottenrouter', 'check', '--config', str(router_config)], check=True, stdout=subprocess.DEVNULL)

def query(qtype):
    # A valid X25519 basepoint gives a real signed Soar hello response. Reusing
    # it hits Soar's hello cache, making replies byte-comparable across paths.
    encoded = base64.b32encode(b'\0\0\x09' + bytes(31)).decode().rstrip('=').lower()
    name = encoded + '.soar.native.test'
    return (struct.pack('!6H', 0x7163, 0x100, 1, 0, 0, 1)
            + b''.join(bytes([len(label)]) + label.encode() for label in name.split('.'))
            + b'\0' + struct.pack('!HH', qtype, 1)
            + b'\0' + struct.pack('!HHIH', 41, 1232, 0, 0))

def exchange(port, packet, tcp=False):
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM if tcp else socket.SOCK_DGRAM) as connection:
        connection.settimeout(3)
        connection.connect(('127.0.0.1', port))
        if tcp:
            connection.sendall(struct.pack('!H', len(packet)) + packet)
            def read(length):
                data = b''
                while len(data) < length:
                    part = connection.recv(length - len(data))
                    assert part, 'short TCP reply'
                    data += part
                return data
            reply = read(struct.unpack('!H', read(2))[0])
        else:
            connection.sendall(packet)
            reply = connection.recv(65535)
    assert reply[:2] == packet[:2] and reply[2] & 0x80 and reply[3] & 15 == 0
    assert struct.unpack('!H', reply[6:8])[0] > 0, 'no signed hello answer'
    return reply

processes = []
try:
    with (root / 'native.log').open('w') as native_log, (root / 'router.log').open('w') as router_log:
        processes.append(subprocess.Popen(shlex.split(command), stdout=native_log, stderr=native_log))
        processes.append(subprocess.Popen(['cottenrouter', 'serve', '--config', str(router_config)], stdout=router_log, stderr=router_log))
        for attempt in range(30):
            try:
                baseline = exchange(5310, query(16))
                exchange(53, query(16), tcp=True)
                break
            except (OSError, AssertionError):
                assert all(process.poll() is None for process in processes), 'native server or router exited'
                if attempt == 29:
                    raise
                time.sleep(.1)
        for qtype in [16, 28]:
            packet = query(qtype)
            native_reply = exchange(5310, packet)
            assert exchange(53, packet) == native_reply, 'routed UDP changed native signed reply'
            assert exchange(53, packet, tcp=True) == native_reply, 'routed TCP changed native signed reply'
finally:
    for process in reversed(processes):
        process.terminate()
    for process in reversed(processes):
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()

version_result = subprocess.run(['slipgate', '--version'], capture_output=True, text=True, check=True)
version = (version_result.stdout + version_result.stderr).strip()
print(json.dumps({
    'native_version': version, 'namespace_isolation': ['mount', 'network', 'pid'],
    'native_cli_add': 'passed', 'native_config_and_unit_paths': 'passed',
    'native_soar_keys': 'passed', 'source_config_preserved': True,
    'signed_soar_hello_types': ['TXT', 'AAAA'],
    'native_vs_routed_udp_byte_exact': True, 'native_vs_routed_tcp_byte_exact': True,
    'shimmed_boundaries': ['systemd', 'firewall', 'external_downloads'],
}, indent=2))
PY
NAMESPACE
