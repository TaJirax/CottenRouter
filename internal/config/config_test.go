package config

import (
	"os"
	"path/filepath"
	"strconv"
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
	if cfg.MaxPacketSize != 16*1024 || cfg.MaxTCPMessageSize != 16*1024 || cfg.Limits.UDPQueue != 1024 || cfg.AdminListen != "127.0.0.1:9088" || cfg.MaxTLSConnectionsPerProtocol != 128 || cfg.Limits.MaxTLSConnectionsPerIP != 32 {
		t.Fatalf("unsafe defaults: %+v", cfg)
	}
	if cfg.Limits.AggregateResponseBytesPerSecond != 6*cfg.Limits.ResponseBytesPerSecond || cfg.Limits.AggregateIngressBytesPerSecond != 6*cfg.Limits.IngressBytesPerSecond {
		t.Fatalf("aggregate traffic ceilings do not bound all six protocol classes: %+v", cfg.Limits)
	}
}

func TestValidateCapsUDPTCPMessagesAt16KiB(t *testing.T) {
	for _, cfg := range []Config{
		{MaxPacketSize: 16*1024 + 1, Routes: []Route{{Name: "one", Domains: []string{"vpn.example"}, Backend: "127.0.0.1:5301"}}},
		{MaxTCPMessageSize: 16*1024 + 1, Routes: []Route{{Name: "one", Domains: []string{"vpn.example"}, Backend: "127.0.0.1:5301"}}},
	} {
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected 16 KiB ceiling error")
		}
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
		Name: "dot", Listen: "127.0.0.1:853", DefaultBackend: "192.0.2.1:8853", DefaultRouteName: "default",
	}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected remote TLS backend error")
	}
}

func TestTLSDefaultBackendRequiresStableRouteName(t *testing.T) {
	cfg := Config{TLSListeners: []TLSListener{{Name: "tls", Listen: "127.0.0.1:853", DefaultBackend: "127.0.0.1:9443"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("unnamed TLS default backend was accepted")
	}
	cfg = Config{TLSListeners: []TLSListener{{Name: "tls", Listen: "127.0.0.1:853", DefaultRouteName: "stale"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("orphan TLS default route name was accepted")
	}
}

func TestValidateRejectsDNSAndTLSOnSameTCPAddress(t *testing.T) {
	cfg := Config{
		ListenTCP: "127.0.0.1:53",
		Routes:    []Route{{Name: "dns", Domains: []string{"dns.example"}, Backend: "127.0.0.1:5301"}},
		TLSListeners: []TLSListener{{
			Name: "tls", Listen: "127.0.0.1:53", DefaultBackend: "127.0.0.1:853", DefaultRouteName: "default",
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected duplicate TCP listener error")
	}
}

func TestValidateRejectsTCPPortSharedAcrossDifferentAddresses(t *testing.T) {
	cfg := Config{
		ListenTCP: "0.0.0.0:53",
		Routes:    []Route{{Name: "dns", Domains: []string{"dns.example"}, Backend: "127.0.0.1:5301"}},
		TLSListeners: []TLSListener{{
			Name: "tls", Listen: "127.0.0.1:53", DefaultBackend: "127.0.0.1:853", DefaultRouteName: "default",
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected overlapping TCP port error")
	}
}

func TestValidateRejectsTLSOnAdminPort(t *testing.T) {
	cfg := Config{TLSListeners: []TLSListener{{
		Name: "tls", Listen: "0.0.0.0:9088", DefaultBackend: "127.0.0.1:853", DefaultRouteName: "default",
	}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected admin/TLS port collision error")
	}
}

func TestLoadImportsEnabledSlipGateDNSTunnels(t *testing.T) {
	dir := t.TempDir()
	slipGate := `{
  "tunnels": [
    {"tag":"dns","transport":"dnstt","domain":"dns.example","port":5310,"enabled":true},
    {"tag":"quic","transport":"slipstream","domain":"quic.example","port":5311,"enabled":true},
    {"tag":"kcp","transport":"vaydns","domain":"kcp.example","port":5312,"enabled":true},
    {"tag":"soar","transport":"soar","domain":"soar.example","port":5314,"enabled":true,"soar":{"public_key":"base64-ed25519-public-key","allow_ports":"80,443"}},
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
	if len(cfg.Routes) != 4 {
		t.Fatalf("got %d imported routes: %+v", len(cfg.Routes), cfg.Routes)
	}
	for _, route := range cfg.Routes {
		if route.TCPBackend != "udp" {
			t.Fatalf("SlipGate tunnel lost DNS TCP-to-UDP forwarding: %+v", route)
		}
		if route.Name == "slipgate:soar" && route.Verify != nil {
			t.Fatalf("Soar's Ed25519 handshake was replaced by legacy HMAC verification: %+v", route)
		}
	}
}

func TestValidateRejectsBackendPointingAtRouterItself(t *testing.T) {
	for name, cfg := range map[string]Config{
		"udp": {ListenUDP: "0.0.0.0:53", Routes: []Route{{Name: "cottendns", Domains: []string{"vpn.example"}, Backend: "127.0.0.1:53"}}},
		"tcp": {ListenUDP: "0.0.0.0:53", ListenTCP: "0.0.0.0:53", Routes: []Route{{Name: "cottendns", Domains: []string{"vpn.example"}, Backend: "127.0.0.1:5301", TCPBackend: "127.0.0.1:53"}}},
		"tls": {ListenUDP: "0.0.0.0:53", TLSListeners: []TLSListener{{Name: "dot", Listen: "0.0.0.0:853", Routes: []TLSRoute{{Name: "cottendns-dot", ServerNames: []string{"dot.example"}, Backend: "127.0.0.1:853"}}}}},
	} {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s: expected a self-routing loop to be rejected", name)
		}
	}
	sane := Config{ListenUDP: "0.0.0.0:53", ListenTCP: "0.0.0.0:53", Routes: []Route{{Name: "cottendns", Domains: []string{"vpn.example"}, Backend: "127.0.0.1:5301", TCPBackend: "127.0.0.1:5301"}}}
	if err := sane.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The router runs unprivileged and cannot enter SlipGate's root-only tunnel
// directories, so the certificate pin must be embedded, not a file path.
func TestSlipGateSlipstreamRouteEmbedsCertificatePin(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(cert, []byte("-----BEGIN CERTIFICATE-----\nAQIDBA==\n-----END CERTIFICATE-----\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "slipgate.json")
	data := `{"tunnels":[{"tag":"quic","transport":"slipstream","domain":"quic.example","port":5311,"enabled":true,"slipstream":{"cert":` + strconv.Quote(cert) + `}}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	routes, err := LoadSlipGateRoutes(path)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := loadVerifyKey(VerifyConfig{CertFile: cert})
	if len(routes) != 1 || routes[0].Verify == nil || routes[0].Verify.CertFile != "" {
		t.Fatalf("route still depends on a certificate file: %+v", routes)
	}
	if err := os.Remove(cert); err != nil {
		t.Fatal(err)
	}
	got, err := loadVerifyKey(*routes[0].Verify)
	if err != nil || string(got) != string(want) {
		t.Fatalf("embedded pin differs from the certificate hash: %x vs %x (%v)", got, want, err)
	}
}
