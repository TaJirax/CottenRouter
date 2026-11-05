#!/usr/bin/env python3
"""Run current native Feed and SlipGate wizards through CottenRouter in a PTY.

Run after bootstrap-test.sh in a fresh, disposable CottenRouter-Test distro:
  COTTENROUTER_SYSTEMD_TEST=1 python3 tests/systemd/feed-slipgate-install-test.py
Pass --project thefeed or --project slipgate to exercise one fresh backend.
The test selects Feed's aliases, messenger, MTProto and DNS media, and
SlipGate's DNSTT plus its new built-in Soar transport. Public DNS delegation,
Telegram logins, GitHub relay credentials and CA issuance are not required.
"""
import argparse
import fcntl
import json
import os
import pathlib
import pty
import re
import select
import signal
import struct
import subprocess
import termios
import time


def require_test_host(project):
    if (os.geteuid() != 0 or os.environ.get('WSL_DISTRO_NAME') != 'CottenRouter-Test'
            or os.environ.get('COTTENROUTER_SYSTEMD_TEST') != '1'):
        raise SystemExit('Requires root and COTTENROUTER_SYSTEMD_TEST=1 in CottenRouter-Test')
    if pathlib.Path('/proc/1/comm').read_text().strip() != 'systemd':
        raise SystemExit('Requires systemd as PID 1')
    if not pathlib.Path('/etc/cottenrouter/config.json').is_file():
        raise SystemExit('Run bootstrap-test.sh first')
    paths = {'thefeed': '/opt/thefeed', 'slipgate': '/etc/slipgate'}
    for name, path in paths.items():
        if project in ('all', name) and pathlib.Path(path).exists():
            raise SystemExit('Requires a fresh backend installation: ' + path)


class NativePTY:
    ansi = re.compile(rb'\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))')

    def __init__(self, project, *args):
        logs = pathlib.Path('/root/cottenrouter-validation/logs')
        logs.mkdir(parents=True, exist_ok=True)
        self.log_path = logs / (project + '-native-features.log')
        self.log = self.log_path.open('wb', buffering=0)
        self.buffer = b''
        self.pid, self.fd = pty.fork()
        if self.pid == 0:
            os.environ['TERM'] = 'xterm-256color'
            os.environ['NO_COLOR'] = '1'
            os.execv('/usr/local/bin/cottenrouter', [
                'cottenrouter', 'install', '--project', project, *args])
        fcntl.ioctl(self.fd, termios.TIOCSWINSZ, struct.pack('HHHH', 30, 120, 0, 0))

    def read(self):
        ready, _, _ = select.select([self.fd], [], [], 0.25)
        if ready:
            try:
                data = os.read(self.fd, 65536)
            except OSError:
                data = b''
            if not data:
                return False
            self.log.write(data)
            self.buffer = self.ansi.sub(b'', self.buffer + data)[-262144:]
            if b'\x1b[6n' in data:
                os.write(self.fd, b'\x1b[1;1R')
        return True

    def expect(self, prompt, answer=None, timeout=600):
        target = prompt.encode()
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            index = self.buffer.find(target)
            if index >= 0:
                self.buffer = self.buffer[index + len(target):]
                if answer is not None:
                    os.write(self.fd, (answer + '\n').encode())
                print('Answered native prompt: ' + prompt, flush=True)
                return
            if not self.read():
                raise RuntimeError('Installer exited before ' + prompt + '; see ' + str(self.log_path))
        raise RuntimeError('Timed out waiting for ' + prompt + '; see ' + str(self.log_path))

    def finish(self):
        self.expect('Installation completed safely.', timeout=600)
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            result, status = os.waitpid(self.pid, os.WNOHANG)
            if result:
                self.pid = None
                if os.waitstatus_to_exitcode(status) != 0:
                    raise RuntimeError('Installer failed; see ' + str(self.log_path))
                return
            self.read()
        raise RuntimeError('Installer did not exit; see ' + str(self.log_path))

    def close(self):
        if self.pid is not None:
            try:
                os.killpg(self.pid, signal.SIGINT)
            except ProcessLookupError:
                pass
        os.close(self.fd)
        self.log.close()


def routes():
    data = json.loads(pathlib.Path('/etc/cottenrouter/config.json').read_text())
    return {route['name']: route for route in data['routes']}


def active(service):
    subprocess.run(['systemctl', 'is-active', '--quiet', service], check=True)


def install_feed():
    session = NativePTY('thefeed', '--domain', 'feed.systemd.test')
    try:
        prompts = [
            ('Channel: ', 'telegram'), ('Channel: ', ''), ('X: ', ''),
            ('Add private channel invite links?', 'n'),
            ('Main DNS domain (e.g., t.example.com): ', 'feed.systemd.test'),
            ('Add extra domains?', 'y'),
            ('  Domain: ', 'feed-alias.systemd.test'), ('  Domain: ', ''),
            ('Enable the messenger?', 'y'),
            ('  Domain: ', 'chat.systemd.test'), ('  Domain: ', ''),
            ('Change messenger limits?', 'n'),
            ('Enable the Telegram proxy?', 'y'),
            ('  Domain: ', 'mtproto.systemd.test'), ('  Domain: ', ''),
            ('Max concurrent Telegram proxy users', '32'),
            ('Telegram proxy sessions kept at once', '1500'),
            ('Telegram proxy retry memory per session, KB', '64'),
            ('Telegram proxy retry memory for all sessions, MB', '64'),
            ('Encryption passphrase: ', 'native-test-only'),
            ('Max messages per channel', ''), ('Fetch cycle interval', ''),
            ('Enable remote management?', 'n'), ('Enable DNS relay?', 'y'),
            ('DNS relay max file size in KB', '160'),
            ('DNS relay TTL in minutes', '60'), ('DNS relay compression', 'gzip'),
            ('Enable GitHub relay?', 'n'), ('Choose mode [1/2]', '1'),
            ('DNS listen address', '127.0.0.1:5304'),
        ]
        for prompt, answer in prompts:
            session.expect(prompt, answer)
        session.finish()
    finally:
        session.close()
    root = pathlib.Path('/opt/thefeed')
    assert (root / 'thefeed-server').is_file(), 'Native Feed binary path changed'
    assert (root / 'data/channels.txt').is_file(), 'Native Feed data path changed'
    env = dict(line.split('=', 1) for line in (root / 'data/thefeed.env').read_text().splitlines()
               if '=' in line and not line.startswith('#'))
    assert env['THEFEED_LISTEN'] == '127.0.0.1:5304'
    for key, value in {
        'THEFEED_EXTRA_DOMAINS': 'feed-alias.systemd.test',
        'THEFEED_CHAT_DOMAINS': 'chat.systemd.test',
        'THEFEED_MTPROTO_DOMAINS': 'mtproto.systemd.test',
        'THEFEED_MTPROTO_MAX_TUNNELS': '32', 'THEFEED_MTPROTO_MAX_SESSIONS': '1500',
        'THEFEED_MTPROTO_REPLY_SESSION_KB': '64', 'THEFEED_MTPROTO_REPLY_BUDGET_MB': '64',
        'THEFEED_DNS_MEDIA_ENABLED': '1', 'THEFEED_DNS_MEDIA_MAX_SIZE_KB': '160',
    }.items():
        assert env[key] == value, 'Native feature setting changed: ' + key
    route = routes()['thefeed']
    assert set(route['domains']) == {
        'feed.systemd.test', 'feed-alias.systemd.test', 'chat.systemd.test', 'mtproto.systemd.test'}
    assert route['backend'] == '127.0.0.1:5304'
    active('thefeed-server')
    active('cottenrouter')
    print('Feed native install paths, messenger, MTProto, aliases and DNS media: PASS', flush=True)


def install_slipgate():
    session = NativePTY('slipgate')
    try:
        for prompt, answer in [
            ('Choice (comma-separated, e.g. 1,3,4): ', '1,4'),
            ('Choice [SOCKS', '1'),
            ('Domain for dnstt/noizdns', 'dnstt.systemd.test'), ('MTU [1232]: ', ''),
            ('Domain for soar', 'soar.systemd.test'), ('Choice [Direct', '1'),
            ('Allowed destination ports', '80,443'),
            ('Create a user now?', 'n'), ('Enable WARP outbound', 'n'),
        ]:
            session.expect(prompt, answer)
        session.finish()
    finally:
        session.close()
    assert pathlib.Path('/usr/local/bin/slipgate').is_file(), 'Native SlipGate binary path changed'
    native = json.loads(pathlib.Path('/etc/slipgate/config.json').read_text())
    selected = {tunnel['transport']: tunnel for tunnel in native['tunnels'] if tunnel['enabled']}
    assert set(selected) == {'dnstt', 'soar'}, selected
    imported = routes()
    for transport, tunnel in selected.items():
        route = imported['slipgate:' + tunnel['tag']]
        assert route['domains'] == [transport + '.systemd.test']
        assert route['backend'] == '127.0.0.1:' + str(tunnel['port'])
        assert route['tcp_backend'] == 'udp'
        active('slipgate-' + tunnel['tag'])
    assert selected['soar']['backend'] == 'direct'
    assert subprocess.run(['systemctl', 'is-active', '--quiet', 'slipgate-dnsrouter']).returncode != 0
    active('cottenrouter')
    print('SlipGate native install paths, DNSTT, built-in Soar and DNS TCP bridge: PASS', flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--project', choices=('all', 'thefeed', 'slipgate'), default='all')
    args = parser.parse_args()
    require_test_host(args.project)
    os.umask(0o077)
    if args.project in ('all', 'thefeed'):
        install_feed()
    if args.project in ('all', 'slipgate'):
        install_slipgate()
    subprocess.run(['/usr/local/bin/cottenrouter', 'check', '--config', '/etc/cottenrouter/config.json'], check=True)
    subprocess.run(['/usr/local/bin/cottenrouter', 'healthz', '--config', '/etc/cottenrouter/config.json'], check=True)


if __name__ == '__main__':
    main()
