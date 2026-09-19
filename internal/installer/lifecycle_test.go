package installer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
	configured, err := configure(spec, request, PortPlan{DoTPublicPort: 853, DoTPrivatePort: 8853, DoHPublicPort: 443, DoHPrivatePort: 8443, DoHMode: "router-front"}, []byte("DOMAIN = [\"old\"]\nCUSTOM_OPTION = \"keep\"\nTLS_CERT_FILE = \"/etc/cert.pem\"\nTLS_KEY_FILE = \"/etc/key.pem\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(configured)
	if !strings.Contains(text, `DOMAIN = ["dns.example", "dot.example", "doh.example"]`) || !strings.Contains(text, `CUSTOM_OPTION = "keep"`) {
		t.Fatalf("TLS names or advanced option lost:\n%s", text)
	}
}

func TestCottenTCPIPv6ListenerRemainsPrivate(t *testing.T) {
	spec, _ := FindSpec("cottendns")
	configured, err := configure(spec, Request{Domain: "dns.example", PrivatePort: 5301, EnableTCP: true}, PortPlan{}, []byte("TCP_IPV6_ENABLED = true\nTCP_IPV6_HOST = \"::\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configured), `TCP_IPV6_HOST = "::1"`) {
		t.Fatalf("IPv6 TCP listener remained public:\n%s", configured)
	}
	// The udp6 listener is on by default at [::] even with no key present.
	if !strings.Contains(string(configured), `UDP_IPV6_HOST = "::1"`) {
		t.Fatalf("IPv6 UDP listener remained public:\n%s", configured)
	}
}

func TestAdvancedCottenEncryptedSettingsStaySynchronized(t *testing.T) {
	listeners := []config.TLSListener{
		{Name: "dot", Listen: "0.0.0.0:853", Routes: []config.TLSRoute{{Name: "cottendns-dot", ServerNames: []string{"dot.example"}, Backend: "127.0.0.1:8853"}}},
		{Name: "https", Listen: "0.0.0.0:443", Routes: []config.TLSRoute{{Name: "cottendns-doh", ServerNames: []string{"doh.example"}, Backend: "127.0.0.1:8443"}}},
	}
	backend := []byte("DOT_LISTENER_ENABLED = true\nDOT_LISTEN_HOST = \"127.0.0.1\"\nDOT_LISTEN_PORT = 9953\nDOH_LISTENER_ENABLED = false\n")
	got, err := syncCottenEncryptedRoutes(listeners, backend, []string{"dns.example", "dot.example", "doh.example"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Routes[0].Backend != "127.0.0.1:9953" || got[0].Routes[0].Name != "cottendns-dot" {
		t.Fatalf("encrypted routes were stale after advanced edit: %+v", got)
	}
}

func TestAdvancedCottenCannotCreateTLSRouteWithoutHostname(t *testing.T) {
	backend := []byte("DOT_LISTENER_ENABLED = true\nDOT_LISTEN_HOST = \"127.0.0.1\"\nDOT_LISTEN_PORT = 9953\nDOH_LISTENER_ENABLED = false\n")
	if _, err := syncCottenEncryptedRoutes(nil, backend, []string{"dns.example"}); err == nil {
		t.Fatal("native TLS enable without a public SNI hostname was accepted")
	}
}

func TestAdvancedCottenRejectsRouterFrontDoHWithoutTrustedCertificate(t *testing.T) {
	listeners := []config.TLSListener{{Name: "https", Listen: "0.0.0.0:443", Routes: []config.TLSRoute{{Name: "cottendns-doh", ServerNames: []string{"doh.example"}, Backend: "127.0.0.1:8443"}}}}
	backend := []byte("DOH_LISTENER_ENABLED = true\nDOH_COEXIST_MODE = \"front\"\nDOH_TLS_ENABLED = true\nDOH_LISTEN_HOST = \"127.0.0.1\"\nDOH_LISTEN_PORT = 8443\n")
	if _, err := syncCottenEncryptedRoutes(listeners, backend, []string{"dns.example", "doh.example"}); err == nil || !strings.Contains(err.Error(), "private listener") {
		t.Fatalf("Advanced accepted silent self-signed DoH fallback: %v", err)
	}
}

func TestSlipGateManagedDNSUnitIsForcedToLoopback(t *testing.T) {
	unit, changed, err := privateSlipGateUnit([]byte("ExecStart=/usr/local/bin/dnstt-server domain 0.0.0.0:5310 upstream\n"), 5310, "dnstt")
	if err != nil || !changed || strings.Contains(string(unit), "0.0.0.0:5310") || !strings.Contains(string(unit), "127.0.0.1:5310") {
		t.Fatalf("unit was not privatized: %s (%v)", unit, err)
	}
	// A second install or Advanced run finds the unit already private.
	if _, changed, err := privateSlipGateUnit(unit, 5310, "dnstt"); err != nil || changed {
		t.Fatalf("already-private unit was rejected or rewritten: changed=%v err=%v", changed, err)
	}
	if _, _, err := privateSlipGateUnit([]byte("ExecStart=/usr/local/bin/vaydns-server -udp 0.0.0.0:9999\n"), 5312, "vaydns"); err == nil {
		t.Fatal("a unit listening somewhere unexpected was accepted")
	}
	if _, _, err := privateSlipGateUnit([]byte("ExecStart=/usr/local/bin/slipstream-server --dns-listen-host 127.0.0.1 --dns-listen-port 5311\n"), 5311, "slipstream"); err != nil {
		t.Fatalf("slipstream's separate host and port flags were rejected: %v", err)
	}
	if slipGateDNSTransport("naive") || !slipGateDNSTransport("vaydns") || !slipGateDNSTransport("external") {
		t.Fatal("transport classification is unsafe")
	}
}

func TestResolveSlipGateBinaryFromWorkDir(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	workDir := t.TempDir()
	binary := filepath.Join(workDir, "slipgate")
	if err := os.WriteFile(binary, []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveSlipGateBinary(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != binary {
		t.Fatalf("resolved %q, want %q", got, binary)
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

func TestSystemdResolvedLoopbackStubCanBeHandedOff(t *testing.T) {
	for _, address := range []string{"127.0.0.53:53", "127.0.0.53%lo:53", "[::1%lo]:53"} {
		stub := Listener{Port: 53, Protocol: "udp", Address: address, Process: `users:(("systemd-resolve",pid=321,fd=13))`}
		if !isSystemdResolvedStub(stub) {
			t.Fatalf("normal systemd-resolved loopback stub %q was treated as an unrelated DNS service", address)
		}
	}
	stubProcess := `users:(("systemd-resolve",pid=321,fd=13))`
	for _, listener := range []Listener{
		{Port: 53, Protocol: "udp", Address: "0.0.0.0:53", Process: stubProcess},
		{Port: 53, Protocol: "udp", Address: "127.0.0.1:53", Process: `users:(("unbound",pid=322,fd=4))`},
		{Port: 5353, Protocol: "udp", Address: "127.0.0.53:5353", Process: stubProcess},
	} {
		if isSystemdResolvedStub(listener) {
			t.Fatalf("unrelated listener was mistaken for the system resolver stub: %+v", listener)
		}
	}
}

func TestSlipGateInitialSetupRunsOnlyWhenConfigIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	needed, err := slipGateNeedsInitialSetup(path)
	if err != nil || !needed {
		t.Fatalf("missing config should require setup: needed=%v err=%v", needed, err)
	}
	if err := os.WriteFile(path, []byte(`{"tunnels":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	needed, err = slipGateNeedsInitialSetup(path)
	if err != nil || needed {
		t.Fatalf("upstream-created config should suppress duplicate setup: needed=%v err=%v", needed, err)
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

func TestProtectedFirewallShimBlocksMutationAndDelegatesInspection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shim")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real-firewall")
	shim := filepath.Join(dir, "iptables")
	if err := os.WriteFile(real, []byte("#!/bin/sh\nprintf 'delegated:%s' \"$*\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, []byte(protectedFirewallScript(real)), 0755); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(shim, "-t", "nat", "-I", "OUTPUT", "1", "-j", "DROP").CombinedOutput()
	if err != nil || strings.Contains(string(output), "delegated:") {
		t.Fatalf("firewall mutation reached native tool: %v %s", err, output)
	}
	// A blocked delete has to fail: upstream installers loop until the rule is
	// gone, and reporting success there never terminates.
	output, err = exec.Command(shim, "-t", "nat", "-D", "OUTPUT", "1").CombinedOutput()
	if err == nil || strings.Contains(string(output), "delegated:") {
		t.Fatalf("blocked delete reported success or reached native tool: %v %s", err, output)
	}
	output, err = exec.Command(shim, "-t", "nat", "-S").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "delegated:-t nat -S") {
		t.Fatalf("read-only firewall command was not delegated: %v %s", err, output)
	}
}

func TestProtectedSystemctlShimPreservesArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shim")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real-systemctl")
	shim := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(real, []byte("#!/bin/sh\nprintf 'delegated:%s' \"$*\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, []byte(protectedSystemctlScript(real, []string{"x-ui.service"})), 0755); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(shim, "--no-block", "restart", "x-ui.service").CombinedOutput()
	if err != nil || strings.Contains(string(output), "delegated:") {
		t.Fatalf("protected service restart reached native tool: %v %s", err, output)
	}
	output, err = exec.Command(shim, "is-active", "--quiet", "x-ui.service").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "delegated:is-active --quiet x-ui.service") {
		t.Fatalf("safe systemctl command lost arguments: %v %s", err, output)
	}
}

func TestProtectedPersistentFirewallShimsBlockWrites(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shim")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real-firewall")
	if err := os.WriteFile(real, []byte("#!/bin/sh\nprintf 'delegated:%s' \"$*\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		name string
		args []string
	}{
		{name: "ufw", args: []string{"allow", "443/tcp"}},
		{name: "firewall-cmd", args: []string{"--permanent", "--add-port=443/tcp"}},
		{name: "iptables-restore", args: nil},
	} {
		shim := filepath.Join(dir, fixture.name)
		if err := os.WriteFile(shim, []byte(protectedPersistentFirewallScript(fixture.name, real)), 0755); err != nil {
			t.Fatal(err)
		}
		output, err := exec.Command(shim, fixture.args...).CombinedOutput()
		if err != nil || strings.Contains(string(output), "delegated:") {
			t.Fatalf("%s mutation reached native tool: %v %s", fixture.name, err, output)
		}
	}
	shim := filepath.Join(dir, "ufw")
	output, err := exec.Command(shim, "status").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "delegated:status") {
		t.Fatalf("read-only ufw status was not delegated: %v %s", err, output)
	}
}

func TestAtomicWriteRefusesSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "managed")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := atomicWrite(filepath.Join(link, "config.json"), []byte("secret"), 0600); err == nil {
		t.Fatal("atomic write followed a symlinked parent")
	}
	if _, err := os.Stat(filepath.Join(target, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("symlink target was modified: %v", err)
	}
}

// Upstream installers clear rules with a delete-until-gone loop. The shim has
// to break it instead of spinning forever on a rule it refuses to remove.
func TestProtectedFirewallShimTerminatesDeleteUntilGoneLoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shim")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real-firewall")
	// The real tool always reports the rule as present, as it would while the
	// shim refuses every delete.
	if err := os.WriteFile(real, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "iptables")
	if err := os.WriteFile(shim, []byte(protectedFirewallScript(real)), 0755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "loop.sh")
	newline := "\n"
	loop := "#!/bin/sh" + newline +
		"while " + shim + " -C OUTPUT -p tcp --dport 53 -j REJECT 2>/dev/null; do" + newline +
		shim + " -D OUTPUT -p tcp --dport 53 -j REJECT || break" + newline +
		"done" + newline
	if err := os.WriteFile(script, []byte(loop), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, script).Run(); err != nil {
		t.Fatalf("delete-until-gone loop failed: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("delete-until-gone loop never terminated")
	}
}

// egressRuleRunner holds StormDNS's outbound TCP/53 reject rule twice on
// iptables and has no ip6tables at all.
type egressRuleRunner struct {
	copies  int
	deletes int
}

func (r *egressRuleRunner) Run(_ context.Context, name string, args []string, _ string, _ bool) error {
	switch {
	case name == "ip6tables":
		return errors.New("ip6tables: not found")
	case name == "iptables" && args[0] == "-C":
		if r.copies == 0 {
			return errors.New("rule absent")
		}
	case name == "iptables" && args[0] == "-D":
		r.copies--
		r.deletes++
	}
	return nil
}

func (r *egressRuleRunner) Output(context.Context, string, ...string) ([]byte, error) {
	return nil, nil
}

func TestStormDNSOutboundTCP53BlockIsRemoved(t *testing.T) {
	runner := &egressRuleRunner{copies: 2}
	if err := (Manager{Runner: runner}).disableStormDNSEgressFilter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.copies != 0 || runner.deletes != 2 {
		t.Fatalf("expected both copies removed, left %d after %d deletes", runner.copies, runner.deletes)
	}
}

// failedUnitRunner models a backend that exits right after start, the way
// thefeed does when its Telegram credentials are missing.
type failedUnitRunner struct{}

func (failedUnitRunner) Run(_ context.Context, name string, args []string, _ string, _ bool) error {
	return errors.New("exit status 3")
}

func (failedUnitRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	if name == "journalctl" {
		// The real error is followed by systemd's own crash and restart lines.
		return []byte("Starting thefeed server v0.38.0\nError: --api-id, --api-hash, and --phone are required (use --no-telegram to skip)\n" +
			"thefeed-server.service: Main process exited, code=exited, status=1/FAILURE\nthefeed-server.service: Failed with result 'exit-code'.\n" +
			"thefeed-server.service: Scheduled restart job, restart counter is at 3.\nStarted thefeed-server.service - thefeed DNS-based Telegram Feed Server.\n"), nil
	}
	return []byte("activating\n"), errors.New("exit status 3")
}

func TestInactiveBackendErrorNamesTheCause(t *testing.T) {
	err := (Manager{Runner: failedUnitRunner{}}).requireActive(context.Background(), "thefeed-server", "thefeed")
	if err == nil {
		t.Fatal("inactive unit reported as active")
	}
	for _, want := range []string{"thefeed did not become active", "state: activating", "--api-id, --api-hash, and --phone are required"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q is missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("error must stay on one line for the TUI notice: %q", err)
	}
}
