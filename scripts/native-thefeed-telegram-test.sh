#!/usr/bin/env bash
# Optional real Feed client/server MTProto stream regression. No Telegram
# account, authentication, public endpoint or personal token is used.
#
# COTTENROUTER_NAMESPACE_TEST=1 THEFEED_SOURCE=/path/to/thefeed-v0.42.0 \
#   THEFEED_BINARY=/path/to/thefeed-server-linux-amd64 \
#   COTTENROUTER_BINARY=/path/to/cottenrouter-linux-amd64 \
#   bash scripts/native-thefeed-telegram-test.sh
#
# The probe compiles in a disposable copy of Feed's module using Feed's own
# Go requirement. The router's Go module and dependencies remain unchanged.
set -euo pipefail
[[ ${COTTENROUTER_NAMESPACE_TEST:-} == 1 && $EUID == 0 ]] || {
  echo 'Requires root and COTTENROUTER_NAMESPACE_TEST=1' >&2; exit 2;
}
source_dir=$(realpath "${THEFEED_SOURCE:?set THEFEED_SOURCE}")
native=$(realpath "${THEFEED_BINARY:?set THEFEED_BINARY}")
router=$(realpath "${COTTENROUTER_BINARY:?set COTTENROUTER_BINARY}")
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
[[ -f $source_dir/go.mod && -f $native && -f $router ]] || exit 2
scratch=$(mktemp -d /tmp/cottenrouter-thefeed-telegram.XXXXXXXX)
cleanup() {
  case "$scratch" in
    /tmp/cottenrouter-thefeed-telegram.*) rm -rf -- "$scratch" ;;
    *) echo 'Refusing unsafe cleanup path' >&2; return 1 ;;
  esac
}
trap cleanup EXIT
cp "$native" "$scratch/thefeed-server"
cp "$router" "$scratch/cottenrouter"
if [[ -n ${THEFEED_PROBE_BINARY:-} ]]; then
  cp "$(realpath "$THEFEED_PROBE_BINARY")" "$scratch/probe"
else
  mkdir -p "$scratch/source/cmd/cottenrouter-mtproto-probe"
  cp "$source_dir/go.mod" "$source_dir/go.sum" "$scratch/source/"
  cp -a "$source_dir/internal" "$scratch/source/internal"
  cp "$script_dir/thefeed-mtproto-probe.go.fixture" "$scratch/source/cmd/cottenrouter-mtproto-probe/main.go"
  (cd "$scratch/source" && CGO_ENABLED=0 go build -o "$scratch/probe" ./cmd/cottenrouter-mtproto-probe)
fi
chmod 0755 "$scratch/thefeed-server" "$scratch/cottenrouter" "$scratch/probe"
export CONTRACT_SCRATCH="$scratch"
export THEFEED_SOURCE_SHA
THEFEED_SOURCE_SHA=$(git -c safe.directory="$source_dir" -C "$source_dir" rev-parse HEAD)
unshare --mount --net --pid --fork --mount-proc bash -euo pipefail <<'NAMESPACE'
mount --make-rprivate /
ip link set lo up
# This namespace has only loopback. Telegram's test DC address is local here;
# it is unreachable from or through the host's actual network interfaces.
ip address add 149.154.167.40/32 dev lo
python3 - <<'PY'
import hashlib, json, os, pathlib, socket, subprocess, threading, time
root=pathlib.Path(os.environ['CONTRACT_SCRATCH'])
data=root/'data'; data.mkdir()
(data/'channels.txt').write_text('telegram\n')
config=root/'router.json'
domains=['feed.native.test','alias.native.test','chat.native.test','mtp.feed.native.test','mtp2.native.test']
config.write_text(json.dumps({'listen_udp':'127.0.0.1:53','routes':[{'name':'thefeed','domains':domains,'backend':'127.0.0.1:5304','tcp_backend':'disabled'}]}))
native_args=[str(root/'thefeed-server'),'--data-dir',str(data),'--domain','feed.native.test','--extra-domains','alias.native.test','--chat-domains','chat.native.test','--mtproto-domains','mtp.feed.native.test,mtp2.native.test','--key','native-telegram-test-only','--no-telegram','--listen','127.0.0.1:5304','--mtproto-test-dcs','--mtproto-max-tunnels','32','--mtproto-max-sessions','1500','--mtproto-reply-session-kb','64','--mtproto-reply-budget-mb','64','--mtproto-down-kb-per-min','0','--fetch-interval','99999']
pub=subprocess.check_output(native_args+['--print-pubkey'],text=True,stderr=subprocess.DEVNULL).strip()
fake=socket.socket(); fake.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); fake.bind(('149.154.167.40',443)); fake.listen()
counts={'connections':0,'received_bytes':0}
lock=threading.Lock()
def echo(connection):
    with connection:
        while True:
            payload=connection.recv(65536)
            if not payload:return
            with lock: counts['received_bytes']+=len(payload)
            connection.sendall(payload)
def accept():
    while True:
        try: connection,_=fake.accept()
        except OSError:return
        with lock: counts['connections']+=1
        threading.Thread(target=echo,args=(connection,),daemon=True).start()
threading.Thread(target=accept,daemon=True).start()
processes=[]
try:
    with (root/'native.log').open('w') as native_log,(root/'router.log').open('w') as router_log:
        processes.append(subprocess.Popen(native_args,stdout=native_log,stderr=native_log))
        processes.append(subprocess.Popen([str(root/'cottenrouter'),'serve','--config',str(config)],stdout=router_log,stderr=router_log))
        time.sleep(.3)
        assert all(process.poll() is None for process in processes),'native server or router failed startup'
        probe=subprocess.run([str(root/'probe'),'--pubkey',pub],capture_output=True,text=True,timeout=140)
        if probe.returncode:
            raise RuntimeError('Native MTProto probe failed: '+probe.stderr[-3000:]+'\n'+(root/'native.log').read_text()[-3000:]+'\n'+(root/'router.log').read_text()[-1000:])
        cases=json.loads(probe.stdout)
        assert len(cases)==12 and all(case['byte_exact_echo'] and case['signed_capability_verified'] for case in cases)
        assert counts['connections']==12 and counts['received_bytes']==sum(case['stream_bytes'] for case in cases),counts
        report={
          'feed_version':subprocess.check_output([str(root/'thefeed-server'),'--version'],text=True).strip(),
          'router_version':subprocess.check_output([str(root/'cottenrouter'),'version'],text=True).strip(),
          'feed_source_sha':os.environ['THEFEED_SOURCE_SHA'],
          'feed_binary_sha256':hashlib.sha256((root/'thefeed-server').read_bytes()).hexdigest(),
          'router_binary_sha256':hashlib.sha256((root/'cottenrouter').read_bytes()).hexdigest(),
          'namespace_isolation':['network','pid','mount'],
          'fake_dc_address':'149.154.167.40:443 (namespace-local loopback)',
          'fake_dc':counts,'cases':cases,
          'not_exercised':['Telegram_authentication','Telegram_API_messages','real_Telegram_network_connectivity','restricted_network_devices'],
        }
        print(json.dumps(report,indent=2))
finally:
    fake.close()
    for process in reversed(processes):process.terminate()
    for process in reversed(processes):
        try:process.wait(timeout=5)
        except subprocess.TimeoutExpired:process.kill();process.wait()
PY
NAMESPACE
