package installer

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestBuildSlipGateTLSPlanPatchesNaiveAndStunTLS(t *testing.T) {
	configuration := slipGateTLSFixture(
		naiveTunnelFixture("web", "naive.example", 443),
		stunTunnelFixture("ssh-tls", 443),
	)
	files := map[string][]byte{
		"/etc/slipgate/tunnels/web/Caddyfile":          naiveCaddyFixture("naive.example", 443, "\n"),
		"/etc/systemd/system/slipgate-ssh-tls.service": stunUnitFixture("ssh-tls", "0.0.0.0", 443, false, "\n"),
	}
	plan, err := BuildSlipGateTLSPlan(configuration, SlipGateTLSOptions{
		PrivatePortStart: 9443,
		PrivatePortEnd:   9450,
		Listeners:        []Listener{{Port: 9443, Protocol: "tcp", Process: `users:(("nginx",pid=9))`}},
		ReadFile:         fixtureTLSReader(files),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Backends) != 2 || len(plan.Patches) != 2 || len(plan.Listeners) != 1 {
		t.Fatalf("unexpected plan sizes: backends=%d patches=%d listeners=%d", len(plan.Backends), len(plan.Patches), len(plan.Listeners))
	}
	backends := backendMap(plan)
	if backends["web"].PrivatePort != 9444 || backends["ssh-tls"].PrivatePort != 9445 {
		t.Fatalf("private allocation is not distinct/free: %+v", backends)
	}
	listener := plan.Listeners[0]
	if listener.Listen != "0.0.0.0:443" || listener.DefaultBackend != "127.0.0.1:9445" || listener.DefaultRouteName != "slipgate:stuntls:ssh-tls" || len(listener.Routes) != 1 {
		t.Fatalf("router merge fragment = %+v", listener)
	}
	if got := listener.Routes[0]; got.Name != "slipgate:naive:web" || got.Backend != "127.0.0.1:9444" || len(got.ServerNames) != 1 || got.ServerNames[0] != "naive.example" {
		t.Fatalf("naive SNI route = %+v", got)
	}

	caddy := string(patchForPath(t, plan, "/etc/slipgate/tunnels/web/Caddyfile").After)
	for _, want := range []string{"https://naive.example:9444 {", "bind 127.0.0.1", "basic_auth alice correct-horse", "reverse_proxy https://decoy.example"} {
		if !strings.Contains(caddy, want) {
			t.Fatalf("patched Caddyfile missing %q:\n%s", want, caddy)
		}
	}
	if strings.Contains(caddy, ":443, naive.example") {
		t.Fatalf("patched Caddyfile retained a public bind:\n%s", caddy)
	}
	unit := string(patchForPath(t, plan, "/etc/systemd/system/slipgate-ssh-tls.service").After)
	for _, want := range []string{"--addr 127.0.0.1", "--port 9445", "--ssh 127.0.0.1:22", "--cert /etc/slipgate/tunnels/ssh-tls/cert.pem", "--key /etc/slipgate/tunnels/ssh-tls/key.pem"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("patched StunTLS unit missing %q:\n%s", want, unit)
		}
	}
}

func TestBuildSlipGateTLSPlanReusesOwnedPrivateBindings(t *testing.T) {
	configuration := slipGateTLSFixture(
		naiveTunnelFixture("web", "Naive.Example.", 443),
		stunTunnelFixture("ssh-tls", 443),
	)
	caddy := []byte("{\n  admin off\n}\n\nhttps://naive.example:9444 {\n  bind 127.0.0.1\n  tls ops@example.com\n}\n")
	unit := stunUnitFixture("ssh-tls", "127.0.0.1", 9445, false, "\n")
	files := map[string][]byte{
		"/etc/slipgate/tunnels/web/Caddyfile":          caddy,
		"/etc/systemd/system/slipgate-ssh-tls.service": unit,
	}
	plan, err := BuildSlipGateTLSPlan(configuration, SlipGateTLSOptions{
		PrivatePortStart: 9443,
		PrivatePortEnd:   9450,
		Listeners: []Listener{
			{Port: 9444, Protocol: "tcp", Process: `users:(("caddy-naive",pid=10))`},
			{Port: 9445, Protocol: "tcp", Process: `users:(("slipgate",pid=11))`},
		},
		ReadFile: fixtureTLSReader(files),
	})
	if err != nil {
		t.Fatal(err)
	}
	backends := backendMap(plan)
	if backends["web"].PrivatePort != 9444 || backends["ssh-tls"].PrivatePort != 9445 {
		t.Fatalf("owned bindings were not reused: %+v", backends)
	}
	if !bytes.Equal(patchForPath(t, plan, "/etc/slipgate/tunnels/web/Caddyfile").After, caddy) {
		t.Fatal("idempotent Caddyfile patch changed an already private artifact")
	}
	if !bytes.Equal(patchForPath(t, plan, "/etc/systemd/system/slipgate-ssh-tls.service").After, unit) {
		t.Fatal("idempotent StunTLS patch changed an already private artifact")
	}
}

func TestBuildSlipGateTLSPlanDoesNotReuseUnrelatedListener(t *testing.T) {
	configuration := slipGateTLSFixture(naiveTunnelFixture("web", "naive.example", 443))
	path := "/etc/slipgate/tunnels/web/Caddyfile"
	private := []byte("https://naive.example:9444 {\n  bind 127.0.0.1\n  tls ops@example.com\n}\n")
	plan, err := BuildSlipGateTLSPlan(configuration, SlipGateTLSOptions{
		PrivatePortStart: 9443,
		PrivatePortEnd:   9450,
		Listeners:        []Listener{{Port: 9444, Protocol: "tcp", Process: `users:(("nginx",pid=12))`}},
		ReadFile:         fixtureTLSReader(map[string][]byte{path: private}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Backends[0].PrivatePort; got != 9443 {
		t.Fatalf("unrelated occupied port was reused: %d", got)
	}
}

func TestBuildSlipGateTLSPlanRejectsNoSNICollision(t *testing.T) {
	configuration := slipGateTLSFixture(stunTunnelFixture("one", 443), stunTunnelFixture("two", 443))
	files := map[string][]byte{
		"/etc/systemd/system/slipgate-one.service": stunUnitFixture("one", "0.0.0.0", 443, false, "\n"),
		"/etc/systemd/system/slipgate-two.service": stunUnitFixture("two", "0.0.0.0", 443, false, "\n"),
	}
	_, err := BuildSlipGateTLSPlan(configuration, SlipGateTLSOptions{ReadFile: fixtureTLSReader(files)})
	if err == nil || !strings.Contains(err.Error(), "no-SNI") {
		t.Fatalf("expected no-SNI collision, got %v", err)
	}
}

func TestBuildSlipGateTLSPlanAllowsNoSNIOnDistinctPublicPorts(t *testing.T) {
	configuration := slipGateTLSFixture(stunTunnelFixture("one", 443), stunTunnelFixture("two", 8443))
	files := map[string][]byte{
		"/etc/systemd/system/slipgate-one.service": stunUnitFixture("one", "0.0.0.0", 443, false, "\n"),
		"/etc/systemd/system/slipgate-two.service": stunUnitFixture("two", "0.0.0.0", 8443, false, "\n"),
	}
	plan, err := BuildSlipGateTLSPlan(configuration, SlipGateTLSOptions{ReadFile: fixtureTLSReader(files)})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Listeners) != 2 || plan.Listeners[0].Listen != "0.0.0.0:443" || plan.Listeners[1].Listen != "0.0.0.0:8443" {
		t.Fatalf("distinct default listeners were not retained: %+v", plan.Listeners)
	}
}

func TestBuildSlipGateTLSPlanRejectsDuplicateNaiveSNI(t *testing.T) {
	configuration := slipGateTLSFixture(
		naiveTunnelFixture("one", "same.example", 443),
		naiveTunnelFixture("two", "same.example", 443),
	)
	files := map[string][]byte{
		"/etc/slipgate/tunnels/one/Caddyfile": naiveCaddyFixture("same.example", 443, "\n"),
		"/etc/slipgate/tunnels/two/Caddyfile": naiveCaddyFixture("same.example", 443, "\n"),
	}
	_, err := BuildSlipGateTLSPlan(configuration, SlipGateTLSOptions{ReadFile: fixtureTLSReader(files)})
	if err == nil || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("expected duplicate SNI rejection, got %v", err)
	}
}

func TestBuildSlipGateTLSPlanPortExhaustionAndPublicPortExclusion(t *testing.T) {
	configuration := slipGateTLSFixture(
		naiveTunnelFixture("one", "one.example", 443),
		naiveTunnelFixture("two", "two.example", 443),
	)
	files := map[string][]byte{
		"/etc/slipgate/tunnels/one/Caddyfile": naiveCaddyFixture("one.example", 443, "\n"),
		"/etc/slipgate/tunnels/two/Caddyfile": naiveCaddyFixture("two.example", 443, "\n"),
	}
	_, err := BuildSlipGateTLSPlan(configuration, SlipGateTLSOptions{PrivatePortStart: 9443, PrivatePortEnd: 9443, ReadFile: fixtureTLSReader(files)})
	if err == nil || !strings.Contains(err.Error(), "no distinct loopback port") {
		t.Fatalf("expected bounded range exhaustion, got %v", err)
	}

	publicInRange := slipGateTLSFixture(naiveTunnelFixture("web", "web.example", 9443))
	publicFile := map[string][]byte{"/etc/slipgate/tunnels/web/Caddyfile": naiveCaddyFixture("web.example", 9443, "\n")}
	plan, err := BuildSlipGateTLSPlan(publicInRange, SlipGateTLSOptions{PrivatePortStart: 9443, PrivatePortEnd: 9444, ReadFile: fixtureTLSReader(publicFile)})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Backends[0].PrivatePort != 9444 {
		t.Fatalf("public port was reused as its own loopback backend: %+v", plan.Backends[0])
	}
	alreadyPrivateOnPublic := []byte("https://web.example:9443 {\n  bind 127.0.0.1\n  tls ops@example.com\n}\n")
	plan, err = BuildSlipGateTLSPlan(publicInRange, SlipGateTLSOptions{
		PrivatePortStart: 9443,
		PrivatePortEnd:   9444,
		Listeners:        []Listener{{Port: 9443, Protocol: "tcp", Process: `users:(("caddy-naive",pid=13))`}},
		ReadFile:         fixtureTLSReader(map[string][]byte{"/etc/slipgate/tunnels/web/Caddyfile": alreadyPrivateOnPublic}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Backends[0].PrivatePort != 9444 {
		t.Fatalf("owned reuse exception overrode public-port exclusion: %+v", plan.Backends[0])
	}
}

func TestBuildSlipGateTLSPlanFailsClosedOnConfigShape(t *testing.T) {
	tests := map[string]string{
		"unknown top-level field": `{"listen":{},"tunnels":[],"backends":[],"route":{},"future":true}`,
		"wrong top-level shape":   `{"listen":[],"tunnels":[],"backends":[],"route":{}}`,
		"unknown tunnel field":    slipGateTLSFixtureText(`{"tag":"web","transport":"naive","domain":"web.example","enabled":true,"future":true,"naive":{"email":"a","decoy_url":"https://x","port":443}}`),
		"unknown naive field":     slipGateTLSFixtureText(`{"tag":"web","transport":"naive","domain":"web.example","enabled":true,"naive":{"email":"a","decoy_url":"https://x","port":443,"future":true}}`),
		"unsafe traversal tag":    slipGateTLSFixtureText(`{"tag":"..","transport":"naive","domain":"web.example","enabled":true,"naive":{"email":"a","decoy_url":"https://x","port":443}}`),
		"invalid naive domain":    slipGateTLSFixtureText(`{"tag":"web","transport":"naive","domain":"bad domain","enabled":true,"naive":{"email":"a","decoy_url":"https://x","port":443}}`),
		"missing stunt cert":      slipGateTLSFixtureText(`{"tag":"stun","transport":"stuntls","enabled":true,"stuntls":{"cert":"","key":"/key","port":443}}`),
		"invalid public port":     slipGateTLSFixtureText(`{"tag":"stun","transport":"stuntls","enabled":true,"stuntls":{"cert":"/cert","key":"/key","port":0}}`),
	}
	for name, fixture := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := BuildSlipGateTLSPlan([]byte(fixture), SlipGateTLSOptions{ReadFile: func(string) ([]byte, error) { return nil, errors.New("must not read") }})
			if err == nil {
				t.Fatal("unexpected success")
			}
		})
	}
}

func TestBuildSlipGateTLSPlanFailsClosedOnCaddyfileShape(t *testing.T) {
	configuration := slipGateTLSFixture(naiveTunnelFixture("web", "web.example", 443))
	path := "/etc/slipgate/tunnels/web/Caddyfile"
	tests := map[string]string{
		"wrong identity":       ":443, other.example {\n  tls ops@example.com\n}\n",
		"wrong public port":    ":8443, web.example {\n  tls ops@example.com\n}\n",
		"multiple sites":       ":443, web.example {\n}\nother.example {\n}\n",
		"unbalanced":           ":443, web.example {\n  route {\n}\n",
		"multiple bind":        "https://web.example:9444 {\n  bind 127.0.0.1\n  bind ::1\n}\n",
		"multi-address bind":   "https://web.example:9444 {\n  bind 127.0.0.1 0.0.0.0\n}\n",
		"unknown site address": "web.example {\n  tls ops@example.com\n}\n",
		"top-level import":     "import /etc/caddy/sites/*\n:443, web.example {\n}\n",
	}
	for name, fixture := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := BuildSlipGateTLSPlan(configuration, SlipGateTLSOptions{ReadFile: fixtureTLSReader(map[string][]byte{path: []byte(fixture)})})
			if err == nil {
				t.Fatal("unexpected success")
			}
		})
	}
}

func TestBuildSlipGateTLSPlanFailsClosedOnStunUnitShape(t *testing.T) {
	configuration := slipGateTLSFixture(stunTunnelFixture("stun", 443))
	path := "/etc/systemd/system/slipgate-stun.service"
	base := string(stunUnitFixture("stun", "0.0.0.0", 443, false, "\n"))
	tests := map[string]string{
		"unsafe address":     strings.Replace(base, "--addr 0.0.0.0", "--addr 192.0.2.1", 1),
		"wrong public port":  strings.Replace(base, "--port 443", "--port 8443", 1),
		"missing cert flag":  strings.Replace(base, " --cert /etc/slipgate/tunnels/stun/cert.pem", "", 1),
		"wrong command":      strings.Replace(base, "stuntls serve", "tunnel serve", 1),
		"multiple ExecStart": strings.Replace(base, "Restart=always", "ExecStart=/bin/false\nRestart=always", 1),
		"continued command":  strings.Replace(base, " --key /etc/slipgate/tunnels/stun/key.pem", " --key /etc/slipgate/tunnels/stun/key.pem \\", 1),
	}
	for name, fixture := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := BuildSlipGateTLSPlan(configuration, SlipGateTLSOptions{ReadFile: fixtureTLSReader(map[string][]byte{path: []byte(fixture)})})
			if err == nil {
				t.Fatal("unexpected success")
			}
		})
	}
}

func TestBuildSlipGateTLSPlanPreservesCRLFAndEqualsFlags(t *testing.T) {
	configuration := slipGateTLSFixture(stunTunnelFixture("stun", 443))
	path := "/etc/systemd/system/slipgate-stun.service"
	unit := stunUnitFixture("stun", "0.0.0.0", 443, true, "\r\n")
	plan, err := BuildSlipGateTLSPlan(configuration, SlipGateTLSOptions{ReadFile: fixtureTLSReader(map[string][]byte{path: unit})})
	if err != nil {
		t.Fatal(err)
	}
	after := patchForPath(t, plan, path).After
	if bytes.Contains(bytes.ReplaceAll(after, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatalf("patch introduced mixed line endings: %q", after)
	}
	text := string(after)
	if !strings.Contains(text, "--addr=127.0.0.1") || !strings.Contains(text, "--port=9443") {
		t.Fatalf("equals-style flags were not patched: %s", text)
	}
}

func TestBuildSlipGateTLSPlanNoTLSIsANoop(t *testing.T) {
	configuration := slipGateTLSFixture(`{"tag":"dns","transport":"dnstt","backend":"socks","domain":"dns.example","port":5310,"enabled":true,"dnstt":{"future":"ignored by TLS planner"}}`)
	plan, err := BuildSlipGateTLSPlan(configuration, SlipGateTLSOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Backends) != 0 || len(plan.Patches) != 0 || len(plan.Listeners) != 0 {
		t.Fatalf("non-TLS config generated TLS work: %+v", plan)
	}
}

func naiveTunnelFixture(tag, domain string, port int) string {
	return fmt.Sprintf(`{"tag":%q,"transport":"naive","backend":"socks","domain":%q,"enabled":true,"naive":{"email":"ops@example.com","decoy_url":"https://decoy.example","port":%d,"user":"alice","password":"correct-horse"}}`, tag, domain, port)
}

func stunTunnelFixture(tag string, port int) string {
	return fmt.Sprintf(`{"tag":%q,"transport":"stuntls","backend":"ssh","domain":"","enabled":true,"stuntls":{"cert":%q,"key":%q,"port":%d}}`, tag, "/etc/slipgate/tunnels/"+tag+"/cert.pem", "/etc/slipgate/tunnels/"+tag+"/key.pem", port)
}

func slipGateTLSFixture(tunnels ...string) []byte {
	return []byte(slipGateTLSFixtureText(tunnels...))
}

func slipGateTLSFixtureText(tunnels ...string) string {
	return `{"listen":{"address":"0.0.0.0:53"},"tunnels":[` + strings.Join(tunnels, ",") + `],"backends":[],"users":[],"route":{"mode":"multi","active":"","default":""},"warp":{"enabled":false}}`
}

func naiveCaddyFixture(domain string, port int, newline string) []byte {
	lines := []string{
		"{", "  admin off", "  log {", "    output stdout", "    level WARN", "  }", "}", "",
		fmt.Sprintf(":%d, %s {", port, domain),
		"  tls ops@example.com", "  route {", "    forward_proxy {", "      basic_auth alice correct-horse", "    }",
		"    reverse_proxy https://decoy.example {", "      header_up Host {upstream_hostport}", "    }", "  }", "}",
	}
	return []byte(strings.Join(lines, newline) + newline)
}

func stunUnitFixture(tag, address string, port int, equals bool, newline string) []byte {
	addrFlag, portFlag := "--addr "+address, fmt.Sprintf("--port %d", port)
	if equals {
		addrFlag, portFlag = "--addr="+address, fmt.Sprintf("--port=%d", port)
	}
	lines := []string{
		"[Unit]", "Description=SlipGate StunTLS: " + tag, "After=network.target", "", "[Service]", "Type=simple", "User=root", "Group=slipgate",
		fmt.Sprintf("ExecStart=/usr/local/bin/slipgate stuntls serve %s %s --ssh 127.0.0.1:22 --cert /etc/slipgate/tunnels/%s/cert.pem --key /etc/slipgate/tunnels/%s/key.pem", addrFlag, portFlag, tag, tag),
		"Restart=always", "", "[Install]", "WantedBy=multi-user.target",
	}
	return []byte(strings.Join(lines, newline) + newline)
}

func fixtureTLSReader(files map[string][]byte) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		data, ok := files[path]
		if !ok {
			return nil, errors.New("fixture file not found")
		}
		return append([]byte(nil), data...), nil
	}
}

func backendMap(plan SlipGateTLSPlan) map[string]SlipGateTLSBackend {
	result := make(map[string]SlipGateTLSBackend, len(plan.Backends))
	for _, backend := range plan.Backends {
		result[backend.Tag] = backend
	}
	return result
}

func patchForPath(t *testing.T, plan SlipGateTLSPlan, path string) SlipGateTLSFilePatch {
	t.Helper()
	for _, patch := range plan.Patches {
		if patch.Path == path {
			return patch
		}
	}
	t.Fatalf("patch %q not found in %+v", path, plan.Patches)
	return SlipGateTLSFilePatch{}
}
