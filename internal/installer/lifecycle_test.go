package installer

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/TaJirax/CottenRouter/internal/config"
)

func TestConfigSummaryPreservesProjectSpecificSettings(t *testing.T) {
	tomlSpec, _ := FindSpec("cottendns")
	domain, extra, port := configSummary(tomlSpec, []byte("DOMAIN = [\"one.example\", \"dot.example\"]\nUDP_PORT = 5301\nDOH_PATH = \"/custom\"\n"))
	if domain != "one.example" || extra != "dot.example" || port != 5301 {
		t.Fatalf("TOML summary = %q %q %d", domain, extra, port)
	}
	envSpec, _ := FindSpec("thefeed")
	domain, extra, port = configSummary(envSpec, []byte("THEFEED_DOMAIN=feed.example\nTHEFEED_EXTRA_DOMAINS=a.example,b.example\nTHEFEED_LISTEN=127.0.0.1:5304\nTHEFEED_KEY=preserved\n"))
	if domain != "feed.example" || extra != "a.example,b.example" || port != 5304 {
		t.Fatalf("env summary = %q %q %d", domain, extra, port)
	}
}

func TestRemoveProjectConfigPreservesOtherBackends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.json")
	cfg := config.Config{Routes: []config.Route{
		{Name: "cottendns", Domains: []string{"cotten.example"}, Backend: "127.0.0.1:5301"},
		{Name: "thefeed", Domains: []string{"feed.example"}, Backend: "127.0.0.1:5304"},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	spec, _ := FindSpec("cottendns")
	if err := removeProjectConfig(path, spec); err != nil {
		t.Fatal(err)
	}
	result, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Routes) != 1 || result.Routes[0].Name != "thefeed" {
		t.Fatalf("unrelated routes changed: %+v", result.Routes)
	}
}

func TestListenerOwnershipIsProcessSpecific(t *testing.T) {
	spec, _ := FindSpec("stormdns")
	if !listenerOwnedBySpec(Listener{Process: `users:(("stormdns",pid=42))`}, spec) {
		t.Fatal("managed listener not recognized")
	}
	if listenerOwnedBySpec(Listener{Process: `users:(("unrelated",pid=43))`}, spec) {
		t.Fatal("unrelated listener was claimed")
	}
}

func TestCottenDomainsIncludeEncryptedSNIWithoutDroppingOptions(t *testing.T) {
	spec, _ := FindSpec("cottendns")
	request := Request{Domain: "dns.example", DoTDomain: "dot.example", DoHDomain: "doh.example", PrivatePort: 5301, EnableDoT: true, EnableDoH: true}
	configured, err := configure(spec, request, PortPlan{DoTPrivatePort: 8853, DoHPrivatePort: 8443, DoHMode: "router-front"}, []byte("DOMAIN = [\"old\"]\nCUSTOM_OPTION = \"keep\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(configured)
	if !strings.Contains(text, `DOMAIN = ["dns.example", "dot.example", "doh.example"]`) || !strings.Contains(text, `CUSTOM_OPTION = "keep"`) {
		t.Fatalf("TLS names or advanced option lost:\n%s", text)
	}
}

func TestSlipGateManagedDNSUnitIsForcedToLoopback(t *testing.T) {
	unit, changed := privateSlipGateUnit([]byte("ExecStart=/usr/local/bin/dnstt-server domain 0.0.0.0:5310 upstream\n"), 5310)
	if !changed || strings.Contains(string(unit), "0.0.0.0:5310") || !strings.Contains(string(unit), "127.0.0.1:5310") {
		t.Fatalf("unit was not privatized: %s", unit)
	}
	if slipGateDNSTransport("naive") || !slipGateDNSTransport("vaydns") {
		t.Fatal("transport classification is unsafe")
	}
}

func TestSlipGateNativeSetupProtectsSharedPublicPorts(t *testing.T) {
	script := protectedFuserScript("/usr/bin/fuser", []string{"53/udp", "53/tcp", "443/tcp", "853/tcp", "2053/tcp"})
	for _, protected := range []string{"53/udp", "53/tcp", "443/tcp", "853/tcp", "2053/tcp"} {
		if !strings.Contains(script, protected) {
			t.Fatalf("guard does not protect %s", protected)
		}
	}
	if !strings.Contains(script, `*" 2053/tcp "*`) {
		t.Fatalf("guard pattern is not shell-safe:\n%s", script)
	}
	if !strings.Contains(script, `exec "/usr/bin/fuser" "$@"`) {
		t.Fatalf("unrelated ports cannot reach real fuser:\n%s", script)
	}
}

func TestProtectedFuserShimExecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shim")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real-fuser")
	shim := filepath.Join(dir, "fuser")
	if err := os.WriteFile(real, []byte("#!/bin/sh\nprintf 'delegated:%s' \"$*\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, []byte(protectedFuserScript(real, []string{"443/tcp"})), 0755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(shim, "-k", "443/tcp").Run(); err == nil {
		t.Fatal("protected port was delegated")
	}
	output, err := exec.Command(shim, "-k", "9443/tcp").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "delegated:-k 9443/tcp") {
		t.Fatalf("safe port delegation failed: %v %s", err, output)
	}
}
