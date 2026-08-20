package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateDefaultsAndNormalizes(t *testing.T) {
	cfg := Config{Routes: []Route{{Name: "one", Domains: []string{"VPN.Example.COM."}, Backend: "127.0.0.1:5301"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.ListenUDP != "0.0.0.0:53" || cfg.Routes[0].Domains[0] != "vpn.example.com" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.MaxPacketSize != 16*1024 || cfg.Limits.UDPQueue != 1024 || cfg.AdminListen != "127.0.0.1:9088" {
		t.Fatalf("unsafe defaults: %+v", cfg)
	}
}

func TestValidateRejectsRemoteAdminListener(t *testing.T) {
	cfg := Config{AdminListen: "0.0.0.0:9088", Routes: []Route{{Name: "one", Domains: []string{"vpn.example"}, Backend: "127.0.0.1:5301"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected loopback-only admin error")
	}
}

func TestValidateRejectsRemoteBackend(t *testing.T) {
	cfg := Config{Routes: []Route{{Name: "one", Domains: []string{"vpn.example.com"}, Backend: "192.0.2.1:53"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected remote backend error")
	}
}

func TestValidateRejectsDuplicateDomain(t *testing.T) {
	cfg := Config{Routes: []Route{
		{Name: "one", Domains: []string{"vpn.example.com"}, Backend: "127.0.0.1:5301"},
		{Name: "two", Domains: []string{"VPN.EXAMPLE.COM."}, Backend: "127.0.0.1:5302"},
	}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected duplicate domain error")
	}
}

func TestValidateNormalizesTLSRoutes(t *testing.T) {
	cfg := Config{TLSListeners: []TLSListener{{
		Name: "https", Listen: "127.0.0.1:443",
		Routes: []TLSRoute{{Name: "doh", ServerNames: []string{"*.DoH.Example."}, Backend: "127.0.0.1:8443"}},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := cfg.TLSListeners[0].Routes[0].ServerNames[0]; got != "doh.example" {
		t.Fatalf("normalized SNI = %q", got)
	}
}

func TestValidateRejectsRemoteTLSBackend(t *testing.T) {
	cfg := Config{TLSListeners: []TLSListener{{
		Name: "dot", Listen: "127.0.0.1:853", DefaultBackend: "192.0.2.1:8853",
	}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected remote TLS backend error")
	}
}

func TestValidateRejectsDNSAndTLSOnSameTCPAddress(t *testing.T) {
	cfg := Config{
		ListenTCP: "127.0.0.1:53",
		Routes:    []Route{{Name: "dns", Domains: []string{"dns.example"}, Backend: "127.0.0.1:5301"}},
		TLSListeners: []TLSListener{{
			Name: "tls", Listen: "127.0.0.1:53", DefaultBackend: "127.0.0.1:853",
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected duplicate TCP listener error")
	}
}

func TestLoadImportsEnabledSlipGateDNSTunnels(t *testing.T) {
	dir := t.TempDir()
	slipGate := `{
  "tunnels": [
    {"tag":"dns","transport":"dnstt","domain":"dns.example","port":5310,"enabled":true},
    {"tag":"quic","transport":"slipstream","domain":"quic.example","port":5311,"enabled":true},
    {"tag":"kcp","transport":"vaydns","domain":"kcp.example","port":5312,"enabled":true},
    {"tag":"web","transport":"naive","domain":"web.example","port":443,"enabled":true},
    {"tag":"off","transport":"dnstt","domain":"off.example","port":5313,"enabled":false}
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, "slipgate.json"), []byte(slipGate), 0600); err != nil {
		t.Fatal(err)
	}
	mainConfig := `{"slipgate_configs":["slipgate.json"]}`
	path := filepath.Join(dir, "cottenrouter.json")
	if err := os.WriteFile(path, []byte(mainConfig), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes) != 3 {
		t.Fatalf("got %d imported routes: %+v", len(cfg.Routes), cfg.Routes)
	}
	for _, route := range cfg.Routes {
		if route.TCPBackend != "disabled" {
			t.Fatalf("SlipGate UDP tunnel gained TCP backend: %+v", route)
		}
	}
}
