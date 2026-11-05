#!/usr/bin/env python3
"""Compare real native replies with shared UDP/TCP routing in the test distro.

Requires all seven backends installed using the *.systemd.test fixture zones.
This deliberately probes DNS routing, not each project's client protocol.
"""
import concurrent.futures
import json
import os
import socket
import struct
import subprocess

if (os.geteuid() != 0 or os.environ.get('WSL_DISTRO_NAME') != 'CottenRouter-Test'
        or os.environ.get('COTTENROUTER_SYSTEMD_TEST') != '1'):
    raise SystemExit('Requires root and COTTENROUTER_SYSTEMD_TEST=1 in CottenRouter-Test')

with open('/etc/cottenrouter/config.json') as source:
    config = json.load(source)
routes = {route['name']: route for route in config['routes']}
expected = {'cottendns', 'masterdnsvpn', 'stormdns', 'thefeed', 'slipgate:dnstt', 'soar', 'soar22'}
assert set(routes) == expected, set(routes)
subprocess.run(['systemctl', 'is-active', 'cottenrouter', 'cottendns', 'masterdnsvpn',
                'stormdns', 'thefeed-server', 'slipgate-dnstt', 'soar-server', 'soar22-server'], check=True)
assert subprocess.run(['systemctl', 'is-active', '--quiet', 'slipgate-dnsrouter']).returncode != 0


def packet(zone):
    name = 'probe.' + zone
    # Every backend receives the same transaction ID; route isolation must not
    # depend on the incoming ID being unique between clients or zones.
    return (struct.pack('!6H', 0x7143, 0x100, 1, 0, 0, 0)
            + b''.join(bytes([len(label)]) + label.encode() for label in name.split('.'))
            + b'\x00' + struct.pack('!2H', 16, 1))


def exchange(destination, query, tcp=False):
    host, port = destination.rsplit(':', 1)
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM if tcp else socket.SOCK_DGRAM) as sock:
        sock.settimeout(5)
        sock.connect((host, int(port)))
        if tcp:
            sock.sendall(struct.pack('!H', len(query)) + query)

            def read_exact(length):
                data = b''
                while len(data) < length:
                    part = sock.recv(length - len(data))
                    assert part, 'truncated TCP reply'
                    data += part
                return data

            response = read_exact(struct.unpack('!H', read_exact(2))[0])
        else:
            sock.sendall(query)
            response = sock.recv(65535)
    assert len(response) >= 12 and response[:2] == query[:2] and response[2] & 0x80
    return response


baselines = {}
for name, route in routes.items():
    assert route['domains'][0].endswith('.systemd.test')
    assert route['backend'].startswith('127.0.0.1:')
    query = packet(route['domains'][0])
    baselines[name] = (query, exchange(route['backend'], query))


def verify(name):
    query, baseline = baselines[name]
    for _ in range(50):
        assert exchange('127.0.0.1:53', query) == baseline, name + ': UDP reply changed in transit'
    response = exchange('127.0.0.1:53', query, tcp=True)
    if name == 'cottendns':
        assert response == exchange(routes[name]['backend'], query, tcp=True), 'TCP reply changed in transit'
    elif routes[name].get('tcp_backend') == 'udp':
        assert response == baseline, name + ': TCP-to-UDP reply changed in transit'
    else:
        assert response[3] & 15 == 2, name + ': UDP-only route must return TCP SERVFAIL'
    return name


with concurrent.futures.ThreadPoolExecutor(max_workers=len(expected)) as pool:
    for name in pool.map(verify, sorted(expected)):
        print(name + ': 50 concurrent UDP replies and TCP behavior passed')
