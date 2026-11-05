#!/usr/bin/env python3
"""Exercise the real TUI queue and native installers in a disposable WSL PTY.

Run after installing the current checkout into an otherwise fresh test distro.
The native prompts use local test data and one public Telegram channel.
"""
import fcntl
import os
import pathlib
import pty
import re
import select
import signal
import struct
import sys
import termios
import time

if (os.geteuid() != 0 or os.environ.get('WSL_DISTRO_NAME') != 'CottenRouter-Test'
        or os.environ.get('COTTENROUTER_SYSTEMD_TEST') != '1'):
    raise SystemExit('Requires root and COTTENROUTER_SYSTEMD_TEST=1 in CottenRouter-Test')
if pathlib.Path('/opt/cottenrouter/backends').exists() or pathlib.Path('/etc/slipgate').exists() or pathlib.Path('/opt/thefeed').exists():
    raise SystemExit('Requires a fresh bootstrap with no pre-existing backend data')
logs = pathlib.Path('/root/cottenrouter-validation/logs')
logs.mkdir(parents=True, exist_ok=True)
os.umask(0o077)
log = (logs / 'tui-seven-installs.log').open('wb', buffering=0)
pid, fd = pty.fork()
if pid == 0:
    os.environ['TERM'] = 'xterm-256color'
    os.environ['NO_COLOR'] = '1'
    os.execv('/usr/local/bin/cottenrouter', ['cottenrouter', 'tui'])
fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack('HHHH', 24, 80, 0, 0))
buffer = b''
ansi = re.compile(rb'\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))')


def send(data):
    os.write(fd, data)
    time.sleep(0.04)


def wait_for(pattern, timeout=300):
    global buffer
    deadline = time.monotonic() + timeout
    target = pattern.encode() if isinstance(pattern, str) else pattern
    while time.monotonic() < deadline:
        plain = ansi.sub(b'', buffer)
        if target in plain:
            buffer = b''
            return
        ready, _, _ = select.select([fd], [], [], 0.5)
        if ready:
            try:
                data = os.read(fd, 65536)
            except OSError:
                raise RuntimeError('TUI exited before expected stage; see private PTY log')
            if not data:
                raise RuntimeError('TUI closed before expected stage')
            log.write(data)
            buffer = (buffer + data)[-131072:]
            if b'\x1b[6n' in data:
                os.write(fd, b'\x1b[1;1R')
    raise RuntimeError('Timed out waiting for ' + target.decode(errors='replace') + '; see private PTY log')


def form(project, domain, field_count):
    wait_for('INSTALL ' + project.upper())
    send(b'\x15' + domain.encode())
    for _ in range(field_count - 1):
        send(b'\t')
    send(b'\r')
    print('Submitted guided form: ' + project, flush=True)


try:
    wait_for('COTTENROUTER CONTROL DECK', 30)
    send(b'\t')
    wait_for('PROJECT MANAGER', 15)
    for index in range(7):
        send(b' ')
        if index < 6:
            send(b'\x1b[B')
    wait_for('7 selected', 15)
    send(b'i')
    form('CottenDNS', 'cottendns.systemd.test', 5)
    form('MasterDnsVPN', 'master.systemd.test', 3)
    form('StormDNS', 'storm.systemd.test', 3)
    form('thefeed', 'feed.systemd.test', 4)
    for prompt, answer in [
        ('Channel: ', b'telegram\n'), ('Channel: ', b'\n'), ('X: ', b'\n'),
        ('Add private channel invite links?', b'n\n'),
        ('Main DNS domain (e.g., t.example.com): ', b'feed.systemd.test\n'),
        ('Add extra domains?', b'n\n'), ('Enable the messenger?', b'n\n'),
        ('Enable the Telegram proxy?', b'n\n'),
        ('Encryption passphrase: ', b'tui-local-test-only\n'),
        ('Max messages per channel', b'\n'), ('Fetch cycle interval', b'\n'),
        ('Enable remote management?', b'n\n'), ('Enable DNS relay?', b'n\n'),
        ('Enable GitHub relay?', b'n\n'), ('Choose mode [1/2]', b'1\n'),
        ('DNS listen address', b'127.0.0.1:5304\n'),
        ('Choice (comma-separated, e.g. 1,3,4): ', b'1\n'),
        ('Choice [SOCKS', b'1\n'),
        ('Domain for dnstt/noizdns', b'slip.systemd.test\n'),
        ('MTU [1232]: ', b'\n'), ('Create a user now?', b'n\n'),
        ('Enable WARP outbound', b'n\n'),
    ]:
        wait_for(prompt)
        send(answer)
        print('Answered native setup: ' + prompt, flush=True)
    form('Soar', 'soar.systemd.test', 2)
    form('Soar 2.2', 'soar22.systemd.test', 2)
    wait_for('Installed 7 projects', 300)
    print('All seven real installers completed through the TUI queue', flush=True)
    send(b'v')
    wait_for('KEYS', 15)
    # A refresh must leave the result visible and readable.
    time.sleep(2.2)
    send(b'\r')
    wait_for('PROJECT MANAGER', 15)
    send(b'q')
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        result, status = os.waitpid(pid, os.WNOHANG)
        if result:
            assert os.waitstatus_to_exitcode(status) == 0
            pid = None
            print('TUI returned after native wizards, retained key results, and exited cleanly', flush=True)
            break
        time.sleep(0.1)
    else:
        raise RuntimeError('TUI did not exit')
finally:
    if pid is not None:
        try:
            os.killpg(pid, signal.SIGINT)
        except ProcessLookupError:
            pass
    log.close()
