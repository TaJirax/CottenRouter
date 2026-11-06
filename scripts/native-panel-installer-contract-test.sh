#!/usr/bin/env bash
# Run actual CottenRouter Panel install/update/configure/Advanced operations
# with the published archive. GitHub transport and systemd are the only mocked
# process boundaries; the shim starts real generated unit commands and sockets.
# COTTENROUTER_NAMESPACE_TEST=1 PANEL_ARCHIVE=/path/to/soar-panel-0.14.4-linux-amd64.tar.gz \
#   PANEL_CHECKSUM=/path/to/archive.sha256 bash scripts/native-panel-installer-contract-test.sh
set -euo pipefail
[[ ${COTTENROUTER_NAMESPACE_TEST:-} == 1 && $EUID == 0 ]] || {
  echo 'Requires root and COTTENROUTER_NAMESPACE_TEST=1' >&2; exit 2;
}
archive=$(realpath "${PANEL_ARCHIVE:?set PANEL_ARCHIVE}")
checksum=$(realpath "${PANEL_CHECKSUM:?set PANEL_CHECKSUM}")
source_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
[[ -f $archive && -f $checksum ]] || exit 2
scratch=$(mktemp -d /tmp/cottenrouter-panel-installer.XXXXXXXX)
cleanup() {
  case "$scratch" in
    /tmp/cottenrouter-panel-installer.*) rm -rf -- "$scratch" ;;
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
(cd "$source_dir" && go test -c -o "$scratch/installer.test" ./internal/installer)
(cd "$source_dir" && go build -trimpath -o "$scratch/local-bin/cottenrouter" ./cmd/cottenrouter)
cat > "$scratch/local-bin/systemctl" <<'SYSTEMCTL'
#!/usr/bin/python3
import os, pathlib, re, shlex, signal, subprocess, sys, time
args = sys.argv[1:]
with open('/run/native-systemctl.log', 'a') as log:
    log.write(' '.join(args) + '\n')
action = args[0] if args else ''
names = [arg.removesuffix('.service') for arg in args[1:] if not arg.startswith('-')]
name = names[0] if names else ''
assert not name or re.fullmatch(r'[A-Za-z0-9_.@-]+', name), 'Unsafe service name'
pidfile = pathlib.Path('/run/' + name + '.pid')
enabled = pathlib.Path('/run/' + name + '.enabled')
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
        for _ in range(150):
            if not pid():
                break
            time.sleep(.02)
        if pid():
            os.kill(value, signal.SIGKILL)
    pidfile.unlink(missing_ok=True)
def start():
    if name not in ('whitedns-panel', 'cottenrouter') or pid():
        return
    env = dict(os.environ)
    cwd = '/'
    if name == 'whitedns-panel':
        unit = pathlib.Path('/etc/systemd/system/whitedns-panel.service').read_text()
        for line in unit.splitlines():
            if line.startswith('EnvironmentFile='):
                for entry in pathlib.Path(line.split('=', 1)[1]).read_text().splitlines():
                    if '=' in entry and not entry.startswith('#'):
                        key, value = entry.split('=', 1)
                        env[key] = value
            if line.startswith('WorkingDirectory='):
                cwd = line.split('=', 1)[1]
        unit = re.sub(r'\\\n\s*', ' ', unit)
        command, = [line.removeprefix('ExecStart=') for line in unit.splitlines() if line.startswith('ExecStart=')]
        command = re.sub(r'\$\{([A-Za-z_][A-Za-z0-9_]*)\}', lambda m: env.get(m[1], ''), command)
        argv = shlex.split(command)
    else:
        argv = ['/usr/local/bin/cottenrouter', 'serve', '--config', '/etc/cottenrouter/config.json']
    with open('/run/' + name + '.log', 'a') as log:
        child = subprocess.Popen(argv, cwd=cwd, env=env, stdout=log, stderr=log, start_new_session=True)
    pidfile.write_text(str(child.pid))
    time.sleep(.15)
if action == 'is-active':
    print('active' if pid() else 'inactive')
    raise SystemExit(0 if pid() else 3)
if action == 'is-enabled':
    print('enabled' if enabled.exists() else 'disabled')
    raise SystemExit(0 if enabled.exists() else 1)
if action in ('restart', 'try-restart'):
    stop(); start()
elif action == 'start':
    start()
elif action == 'stop':
    stop()
elif action == 'enable':
    enabled.touch()
    if '--now' in args:
        start()
elif action == 'disable':
    enabled.unlink(missing_ok=True)
    if '--now' in args:
        stop()
SYSTEMCTL
for name in iptables ip6tables; do
  printf '#!/bin/sh\nexit 0\n' > "$scratch/local-bin/$name"
done
for name in ufw firewall-cmd; do
  printf '#!/bin/sh\nexit 1\n' > "$scratch/local-bin/$name"
done
printf '#!/bin/sh\nexit 0\n' > "$scratch/local-bin/native-panel-editor"
chmod 0755 "$scratch/local-bin/"*
export CONTRACT_SCRATCH="$scratch" CONTRACT_ARCHIVE="$archive" CONTRACT_CHECKSUM="$checksum"
unshare --mount --net --pid --fork --mount-proc bash -euo pipefail <<'NAMESPACE'
mount --make-rprivate /
mount --bind "$CONTRACT_SCRATCH/etc" /etc
mount --bind "$CONTRACT_SCRATCH/local-bin" /usr/local/bin
mount -t tmpfs tmpfs /run
mount -t tmpfs tmpfs /opt
mount -t tmpfs tmpfs /var/lib
mkdir -p /etc/systemd/system /run/systemd/system
printf 'private-namespace\n' > /run/cottenrouter-native-panel-contract
export PATH=/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export COTTENROUTER_NATIVE_PANEL_CONTRACT=1
ip link set lo up
"$CONTRACT_SCRATCH/installer.test" -test.run '^TestPanelNativeInstallerContract$' -test.v -test.timeout 4m
NAMESPACE
