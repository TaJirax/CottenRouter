# Compatibility notice

CottenRouter interoperates with the public wire formats and configuration
schemas of CottenDNS, MasterDnsVPN, StormDNS, thefeed, and SlipGate. No upstream
binaries or source trees are vendored in this repository. Each upstream project
retains its own copyright and license; its installer is fetched directly from
its current default branch only when the operator requests that workflow.

SlipGate compatibility includes importing enabled DNS-tunnel entries and
answering SlipNet's authenticated reachability/MTU probe. Non-DNS SlipGate
features continue to run in SlipGate itself.
