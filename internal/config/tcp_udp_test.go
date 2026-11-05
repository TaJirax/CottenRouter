package config

import "testing"

func TestUDPBackendForTCPKeepsEndpointValidation(t *testing.T) {
	for _, backend := range []string{"127.0.0.1:5310", "[::1]:5310"} {
		cfg := Config{Routes: []Route{{Name: "bridge", Domains: []string{"bridge.example"}, Backend: backend, TCPBackend: "udp"}}}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		if cfg.Routes[0].TCPBackend != "udp" {
			t.Fatalf("UDP mode lost: %+v", cfg.Routes[0])
		}
	}
	for _, backend := range []string{"192.0.2.1:5310", "127.0.0.1:53"} {
		cfg := Config{Routes: []Route{{Name: "bridge", Domains: []string{"bridge.example"}, Backend: backend, TCPBackend: "udp"}}}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("unsafe UDP backend %q accepted", backend)
		}
	}
}
