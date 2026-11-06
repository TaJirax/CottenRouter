#!/usr/bin/env bash
# Verify a published WhiteDNS Panel bundle and its real private DNS frontend
# against CottenRouter in disposable mount/PID/network namespaces.
# COTTENROUTER_NAMESPACE_TEST=1 PANEL_ARCHIVE=/path/to/soar-panel-V-linux-amd64.tar.gz \
#   PANEL_CHECKSUM=/path/to/archive.sha256 COTTENROUTER_BINARY=/path/to/cottenrouter \
#   bash scripts/native-panel-contract-test.sh
set -euo pipefail
[[ ${COTTENROUTER_NAMESPACE_TEST:-} == 1 && $EUID == 0 ]] || {
  echo 'Requires root and COTTENROUTER_NAMESPACE_TEST=1' >&2; exit 2;
}
archive=$(realpath "${PANEL_ARCHIVE:?set PANEL_ARCHIVE}")
checksum=$(realpath "${PANEL_CHECKSUM:?set PANEL_CHECKSUM}")
router=$(realpath "${COTTENROUTER_BINARY:?set COTTENROUTER_BINARY}")
[[ -f $archive && -f $checksum && -f $router ]] || exit 2
scratch=$(mktemp -d /tmp/cottenrouter-native-panel.XXXXXXXX)
cleanup() {
  case "$scratch" in
    /tmp/cottenrouter-native-panel.*) rm -rf -- "$scratch" ;;
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
cp "$router" "$scratch/local-bin/cottenrouter"
chmod 0755 "$scratch/local-bin/cottenrouter"
export CONTRACT_SCRATCH="$scratch" CONTRACT_ARCHIVE="$archive" CONTRACT_CHECKSUM="$checksum"
unshare --mount --net --pid --fork --mount-proc bash -euo pipefail <<'NAMESPACE'
mount --make-rprivate /
mount --bind "$CONTRACT_SCRATCH/etc" /etc
mount --bind "$CONTRACT_SCRATCH/local-bin" /usr/local/bin
mount -t tmpfs tmpfs /run
mount -t tmpfs tmpfs /opt
mount -t tmpfs tmpfs /var/lib
export PATH=/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ip link set lo up
python3 - <<'PY'
import base64
import hashlib
import io
import json
import os
import pathlib
import re
import socket
import struct
import subprocess
import tarfile
import time

root = pathlib.Path(os.environ['CONTRACT_SCRATCH'])
archive = pathlib.Path(os.environ['CONTRACT_ARCHIVE'])
match = re.fullmatch(r'soar-panel-(\d+\.\d+\.\d+)-linux-(amd64|arm64)\.tar\.gz', archive.name)
assert match, 'Unexpected Panel archive name'
version, arch = match.groups()
digest = hashlib.sha256(archive.read_bytes()).hexdigest()
expected, name = pathlib.Path(os.environ['CONTRACT_CHECKSUM']).read_text().split()
assert expected.lower() == digest and name.lstrip('*') == archive.name, 'Archive checksum mismatch'
prefix = archive.name.removesuffix('.tar.gz') + '/'
files = {}
with tarfile.open(fileobj=io.BytesIO(archive.read_bytes()), mode='r:gz') as bundle:
    for member in bundle:
        assert member.name.startswith(prefix) and '..' not in pathlib.PurePosixPath(member.name).parts
        assert member.isdir() or member.isfile(), 'Unsupported archive entry'
        if member.isfile():
            name = member.name.removeprefix(prefix)
            assert name not in files and member.size <= 32 * 1024 * 1024
            files[name] = bundle.extractfile(member).read()
manifest = {}
for line in files['SHA256SUMS'].decode().splitlines():
    value, name = line.split()
    name = name.lstrip('*')
    assert name in files and name not in manifest and hashlib.sha256(files[name]).hexdigest() == value
    manifest[name] = value
assert set(manifest) == set(files) - {'SHA256SUMS'}, 'Unverified archive member'
assert files['ARCH'].decode().strip() == arch
work = pathlib.Path('/opt/cottenrouter/backends/whitedns-panel')
work.mkdir(parents=True)
binary = work / 'whitedns-panel'
binary.write_bytes(files['bin/linux-' + arch + '/soar-panel'])
binary.chmod(0o755)
assert subprocess.check_output([binary, 'version'], text=True).strip() == version
capabilities = subprocess.run([binary, 'capabilities'], capture_output=True, text=True)
password = root / 'password'
password.write_text('panel-native-contract-7A!')
password.chmod(0o600)
db = work / 'panel.db'
zone = 'p.native.test'
subprocess.run([binary, 'init', '--db', db, '--zone', zone, '--admin', 'admin', '--password-file', password], check=True, capture_output=True)
probe = subprocess.check_output([binary, 'create-install-probe', '--db', db], text=True).split()
assert len(probe) == 4 and probe[1].endswith('.' + zone)
router_config = root / 'router.json'
router_config.write_text(json.dumps({
    'listen_udp': '0.0.0.0:53', 'listen_tcp': '0.0.0.0:53',
    'routes': [{'name': 'whitedns-panel', 'domains': [zone], 'backend': '127.0.0.1:5307', 'tcp_backend': '127.0.0.1:5307'}],
}))
subprocess.run(['cottenrouter', 'check', '--config', router_config], check=True, stdout=subprocess.DEVNULL)
def wait(command, child):
    for attempt in range(100):
        result = subprocess.run(command, capture_output=True, text=True)
        if result.returncode == 0:
            return
        assert child.poll() is None, 'Native runtime exited'
        time.sleep(.1)
    raise RuntimeError('Native runtime not ready: ' + result.stderr[-1000:])
def query(name, qtype):
    return (struct.pack('!6H', 0x7163, 0x100, 1, 0, 0, 1)
            + b''.join(bytes([len(label)]) + label.encode() for label in name.split('.'))
            + b'\0' + struct.pack('!HH', qtype, 1)
            + b'\0' + struct.pack('!HHIH', 41, 1232, 0, 0))
def exchange(port, packet, tcp):
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM if tcp else socket.SOCK_DGRAM) as client:
        client.settimeout(3)
        client.connect(('127.0.0.1', port))
        if tcp:
            client.sendall(struct.pack('!H', len(packet)) + packet)
            def exact(length):
                out = b''
                while len(out) < length:
                    chunk = client.recv(length - len(out))
                    assert chunk, 'Short TCP reply'
                    out += chunk
                return out
            reply = exact(struct.unpack('!H', exact(2))[0])
        else:
            client.send(packet)
            reply = client.recv(65535)
    assert len(reply) >= 12 and reply[:2] == packet[:2] and reply[2] & 0x80
    return reply
children = []
cases = []
try:
    with (root / 'services.log').open('w') as logs:
        panel = subprocess.Popen([binary, 'run', '--db', db, '--dns-listen', '127.0.0.1:5307', '--listen', '127.0.0.1:2054', '--ops-socket', str(work / 'router-managed-ops.sock')], stdout=logs, stderr=logs)
        children.append(panel)
        wait([binary, 'health', '--db', db, '--listen', '127.0.0.1:2054'], panel)
        router = subprocess.Popen(['cottenrouter', 'serve', '--config', router_config], stdout=logs, stderr=logs)
        children.append(router)
        wait(['cottenrouter', 'healthz', '--config', router_config], router)
        hello = base64.b32encode(b'\0\0\x09' + bytes(31)).decode().rstrip('=').lower()
        for tcp in (False, True):
            for name, qtype, kind in [(zone, 6, 'base-SOA'), (probe[1], 6, 'account-SOA'),
                                      (hello + '.' + probe[1], 16, 'signed-Soar-TXT'),
                                      (hello + '.' + probe[1], 28, 'signed-Soar-AAAA')]:
                packet = query(name, qtype)
                native = exchange(5307, packet, tcp)
                if kind.startswith('signed') and not tcp:
                    # Panel readiness precedes asynchronous per-account engine
                    # startup; wait for the real Soar engine, not only HTTP.
                    for attempt in range(50):
                        if struct.unpack('!H', native[6:8])[0] > 0 and native[3] & 15 == 0:
                            break
                        time.sleep(.1)
                        native = exchange(5307, packet, tcp)
                routed = exchange(53, packet, tcp)
                assert native == routed, 'Routed bytes differ: ' + kind
                if kind.startswith('signed') and not tcp:
                    assert struct.unpack('!H', native[6:8])[0] > 0 and native[3] & 15 == 0, 'Missing signed Soar answer'
                elif kind.startswith('signed'):
                    # Panel's native frontend intentionally exposes Soar over
                    # UDP, while Cotten/MasterDNS have native TCP handlers.
                    assert struct.unpack('!H', native[6:8])[0] == 0 and native[3] & 15 == 0
                    kind = kind.replace('signed-Soar', 'native-Soar-NODATA')
                cases.append(('tcp' if tcp else 'udp') + ':' + kind)
except BaseException:
    print((root / 'services.log').read_text()[-10000:])
    raise
finally:
    for child in reversed(children):
        if child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=10)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait()
assert subprocess.check_output([binary, 'zone', '--db', db], text=True).strip() == zone
subprocess.run([binary, 'remove-install-probe', '--db', db, '--probe-id', probe[0]], check=True, capture_output=True)
print(json.dumps({
    'native_version': version, 'architecture': arch, 'archive_sha256': digest,
    'archive_and_embedded_manifest_verified': True,
    'capabilities_command_supported': capabilities.returncode == 0,
    'init_probe_health_run_and_database_reopen': 'passed',
    'native_vs_routed_byte_exact': cases,
    'namespace_isolation': ['mount', 'network', 'pid'],
    'not_exercised': ['systemd_daemon', 'upstream_install_script', 'external_tunnel_destinations', 'panel_ops_service'],
}, indent=2))
PY
NAMESPACE
