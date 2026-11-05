package installer

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// This schema matches SlipGate e1df982e (2026-11-03). Native Soar, public
// addressing, VayDNS tuning, users and WARP coexist with TLS transports.
func TestSlipGateCurrentSchemaPreservesNativeFeatures(t *testing.T) {
	configuration := slipGateTLSFixture(
		`{"tag":"soar","transport":"soar","backend":"direct","domain":"soar.example","port":5310,"enabled":true,"soar":{"private_key":"/etc/slipgate/tunnels/soar/server.key","public_key":"base64-public-key","allow_ports":"any"}}`,
		`{"tag":"vay","transport":"vaydns","backend":"ssh","domain":"vay.example","port":5311,"enabled":true,"vaydns":{"record_type":"caa","dnstt_compat":true,"queue_size":1024,"kcp_window_size":512,"queue_overflow":"block","fallback":"1.1.1.1:53"}}`,
		naiveTunnelFixture("web", "web.example", 443),
		stunTunnelFixture("tls", 443),
	)
	var document map[string]any
	if err := json.Unmarshal(configuration, &document); err != nil {
		t.Fatal(err)
	}
	document["public_address"] = "192.0.2.10"
	document["future_native_feature"] = map[string]any{"enabled": true}
	document["warp"] = map[string]any{"enabled": true}
	document["users"] = []any{map[string]any{"username": "alice", "password": "keep-native"}}
	document["tunnels"].([]any)[0].(map[string]any)["future_native_setting"] = "preserve"
	configuration, _ = json.Marshal(document)
	before := append([]byte(nil), configuration...)
	plan, err := BuildSlipGateTLSPlan(configuration, SlipGateTLSOptions{ReadFile: fixtureTLSReader(map[string][]byte{
		"/etc/slipgate/tunnels/web/Caddyfile":      naiveCaddyFixture("web.example", 443, "\n"),
		"/etc/systemd/system/slipgate-tls.service": stunUnitFixture("tls", "0.0.0.0", 443, false, "\n"),
	})})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Backends) != 2 || len(plan.Patches) != 2 || len(plan.Listeners) != 1 {
		t.Fatalf("new native features changed TLS planning: %+v", plan)
	}
	if !bytes.Equal(configuration, before) {
		t.Fatal("TLS planning changed native configuration")
	}

	units := []byte("slipgate-soar.service enabled\nslipgate-vay.service enabled\nslipgate-web.service enabled\nslipgate-tls.service enabled\nslipgate-socks5.service enabled\nslipgate-warp.service enabled\nslipgate-dnsrouter.service enabled\nslipgate-iptables.service enabled\n")
	services, err := enabledSlipGateManagedServices(configuration, units)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"slipgate-soar", "slipgate-socks5", "slipgate-tls", "slipgate-vay", "slipgate-warp", "slipgate-web"}
	if !reflect.DeepEqual(services, want) {
		t.Fatalf("native feature services = %v, want %v", services, want)
	}

	document["warp"] = map[string]any{"enabled": false}
	configuration, _ = json.Marshal(document)
	services, err = enabledSlipGateManagedServices(configuration, units)
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range services {
		if service == "slipgate-warp" {
			t.Fatal("integration enabled native WARP while disabled in its config")
		}
	}
}

func TestSlipGateNativeUnknownTransportDoesNotBecomeDNSOrTLS(t *testing.T) {
	configuration := slipGateTLSFixture(`{"tag":"future","transport":"future-direct","enabled":true,"new_transport":{"port":443}}`)
	plan, err := BuildSlipGateTLSPlan(configuration, SlipGateTLSOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Backends) != 0 || len(plan.Patches) != 0 || len(plan.Listeners) != 0 {
		t.Fatalf("unknown native transport created TLS routes: %+v", plan)
	}
}
