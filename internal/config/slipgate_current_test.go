package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewFromSlipGateSupportsCurrentDNSFrontend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slipgate.json")
	data := []byte(`{"public_address":"192.0.2.10","tunnels":[
{"tag":"soar","transport":"soar","backend":"direct","domain":"soar.example","port":5310,"enabled":true,"soar":{"private_key":"/etc/slipgate/tunnels/soar/server.key","public_key":"base64-ed25519-key","allow_ports":"any"}},
{"tag":"external","transport":"external","domain":"external.example","port":5311,"enabled":true},
{"tag":"ssh","transport":"direct-ssh","enabled":true},
{"tag":"future","transport":"future-direct","enabled":true,"domain":"future.example","port":443},
{"tag":"off","transport":"soar","domain":"off.example","port":5312,"enabled":false}
]}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := NewFromSlipGate(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenUDP != "0.0.0.0:53" || cfg.ListenTCP != "0.0.0.0:53" {
		t.Fatalf("SlipGate import lost a native DNS frontend: udp=%q tcp=%q", cfg.ListenUDP, cfg.ListenTCP)
	}
	if len(cfg.Routes) != 2 || cfg.Routes[0].Name != "slipgate:soar" || cfg.Routes[1].Name != "slipgate:external" {
		t.Fatalf("unexpected routes: %+v", cfg.Routes)
	}
	for _, route := range cfg.Routes {
		if route.TCPBackend != "udp" {
			t.Fatalf("route lost native TCP-to-UDP forwarding: %+v", route)
		}
		if route.Verify != nil {
			t.Fatalf("native handshake was replaced with unsupported HMAC verification: %+v", route)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatalf("native config was modified: %v", err)
	}
}
