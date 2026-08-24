package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/TaJirax/CottenRouter/internal/config"
)

func TestApplySlipGateTLSPatchesRollsBackExactBytes(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "Caddyfile")
	second := filepath.Join(dir, "service")
	before := []byte("first-before\r\n")
	if err := os.WriteFile(first, before, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("changed-after-plan\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := applySlipGateTLSPatches(SlipGateTLSPlan{Patches: []SlipGateTLSFilePatch{
		{Path: first, Before: before, After: []byte("first-after\n"), Mode: 0644},
		{Path: second, Before: []byte("second-before\n"), After: []byte("second-after\n"), Mode: 0644},
	}})
	if err == nil || !strings.Contains(err.Error(), "changed after planning") {
		t.Fatalf("stale artifact was not rejected: %v", err)
	}
	got, readErr := os.ReadFile(first)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, before) {
		t.Fatalf("first patch was not restored exactly: %q", got)
	}
}

func TestSlipGateTLSPatchTransactionRollsBackLaterFailureAndCommitKeepsPatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unit")
	before := []byte("before\n")
	after := []byte("after\n")
	if err := os.WriteFile(path, before, 0644); err != nil {
		t.Fatal(err)
	}
	plan := SlipGateTLSPlan{Patches: []SlipGateTLSFilePatch{{Path: path, Before: before, After: after, Mode: 0644}}}
	tx, err := applySlipGateTLSPatches(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, before) {
		t.Fatalf("rollback mismatch: %q", got)
	}

	tx, err = applySlipGateTLSPatches(plan)
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if !bytes.Equal(got, after) {
		t.Fatalf("committed patch was rolled back: %q", got)
	}
}

func TestMergeSlipGateTLSListenersPreservesUnrelatedAndReplacesManaged(t *testing.T) {
	existing := []config.TLSListener{{
		Name: "https", Listen: ":443", DefaultBackend: "127.0.0.1:9444", DefaultRouteName: "slipgate:stuntls:old",
		Routes: []config.TLSRoute{
			{Name: "panel", ServerNames: []string{"panel.example"}, Backend: "127.0.0.1:10000"},
			{Name: "slipgate:naive:old", ServerNames: []string{"old.example"}, Backend: "127.0.0.1:9443"},
		},
	}}
	previous := SlipGateTLSPlan{Listeners: []config.TLSListener{{Listen: "0.0.0.0:443", DefaultBackend: "127.0.0.1:9444", DefaultRouteName: "slipgate:stuntls:old"}}}
	current := SlipGateTLSPlan{Listeners: []config.TLSListener{{
		Name: "slipgate-tls-443", Listen: "0.0.0.0:443", DefaultBackend: "127.0.0.1:9446", DefaultRouteName: "slipgate:stuntls:new",
		Routes: []config.TLSRoute{{Name: "slipgate:naive:new", ServerNames: []string{"new.example"}, Backend: "127.0.0.1:9445"}},
	}}}
	got, err := mergeSlipGateTLSListeners(existing, current, previous)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "https" || got[0].DefaultBackend != "127.0.0.1:9446" {
		t.Fatalf("listener identity/default not merged: %+v", got)
	}
	if len(got[0].Routes) != 2 || got[0].Routes[0].Name != "panel" || got[0].Routes[1].Name != "slipgate:naive:new" {
		t.Fatalf("unrelated or managed SNI routes were mishandled: %+v", got[0].Routes)
	}
}

func TestMergeSlipGateTLSListenersRemovesDisabledStaleEntries(t *testing.T) {
	existing := []config.TLSListener{
		{Name: "shared", Listen: "0.0.0.0:443", DefaultBackend: "127.0.0.1:9444", DefaultRouteName: "slipgate:stuntls:old", Routes: []config.TLSRoute{{Name: "panel", ServerNames: []string{"panel.example"}, Backend: "127.0.0.1:10000"}, {Name: "slipgate:naive:old", ServerNames: []string{"old.example"}, Backend: "127.0.0.1:9443"}}},
		{Name: "slipgate-tls-8443", Listen: "0.0.0.0:8443", Routes: []config.TLSRoute{{Name: "slipgate:naive:gone", ServerNames: []string{"gone.example"}, Backend: "127.0.0.1:9445"}}},
	}
	previous := SlipGateTLSPlan{Listeners: []config.TLSListener{{Listen: "0.0.0.0:443", DefaultBackend: "127.0.0.1:9444", DefaultRouteName: "slipgate:stuntls:old"}}}
	got, err := mergeSlipGateTLSListeners(existing, SlipGateTLSPlan{}, previous)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DefaultBackend != "" || len(got[0].Routes) != 1 || got[0].Routes[0].Name != "panel" {
		t.Fatalf("stale SlipGate TLS entries were not removed safely: %+v", got)
	}
}

func TestMergeSlipGateTLSListenersRejectsUnrelatedDefaultAndSNI(t *testing.T) {
	stun := SlipGateTLSPlan{Listeners: []config.TLSListener{{Listen: "0.0.0.0:443", DefaultBackend: "127.0.0.1:9444", DefaultRouteName: "slipgate:stuntls:web"}}}
	_, err := mergeSlipGateTLSListeners([]config.TLSListener{{Name: "panel", Listen: ":443", DefaultBackend: "127.0.0.1:10000", DefaultRouteName: "panel-default"}}, stun, SlipGateTLSPlan{})
	if err == nil || !strings.Contains(err.Error(), "unrelated default_backend") {
		t.Fatalf("unrelated default was not rejected: %v", err)
	}

	naive := SlipGateTLSPlan{Listeners: []config.TLSListener{{Listen: "0.0.0.0:443", Routes: []config.TLSRoute{{Name: "slipgate:naive:web", ServerNames: []string{"same.example"}, Backend: "127.0.0.1:9443"}}}}}
	_, err = mergeSlipGateTLSListeners([]config.TLSListener{{Name: "panel", Listen: ":443", Routes: []config.TLSRoute{{Name: "panel", ServerNames: []string{"*.same.example"}, Backend: "127.0.0.1:10000"}}}}, naive, SlipGateTLSPlan{})
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("unrelated SNI was not rejected: %v", err)
	}
}

func TestValidateSlipGateTLSPublicPortsRejectsPanelAndReservedSockets(t *testing.T) {
	plan := integrationTLSPlan(443, 9443, "naive")
	missingConfig := filepath.Join(t.TempDir(), "missing.json")
	if err := validateSlipGateTLSPublicPorts(plan, []Listener{{Port: 443, Protocol: "tcp", Process: "xray", Address: "0.0.0.0:443"}}, missingConfig); err == nil || !strings.Contains(err.Error(), "unrelated") {
		t.Fatalf("panel-owned port was accepted: %v", err)
	}
	if err := validateSlipGateTLSPublicPorts(plan, []Listener{{Port: 443, Protocol: "tcp", Process: "caddy-naive", Address: "0.0.0.0:443"}}, missingConfig); err != nil {
		t.Fatalf("planned native listener was rejected: %v", err)
	}
	if err := validateSlipGateTLSPublicPorts(integrationTLSPlan(53, 9443, "naive"), nil, missingConfig); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("router DNS port was accepted: %v", err)
	}
}

func TestValidateSlipGateTLSPublicPortsReadsCustomRouterAdmin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.json")
	if err := os.WriteFile(path, []byte(`{"listen_tcp":"0.0.0.0:5353","admin_listen":"127.0.0.1:9443"}`), 0644); err != nil {
		t.Fatal(err)
	}
	err := validateSlipGateTLSPublicPorts(integrationTLSPlan(9443, 9444, "stuntls"), nil, path)
	if err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("custom admin port was accepted: %v", err)
	}
}

func TestMissingSlipGateTLSListenersRequiresLoopbackAndRouterOwnership(t *testing.T) {
	plan := SlipGateTLSPlan{Backends: []SlipGateTLSBackend{
		{Transport: "naive", Service: "slipgate-web", PublicPort: 443, PrivatePort: 9443},
		{Transport: "stuntls", Service: "slipgate-ssh", PublicPort: 443, PrivatePort: 9444},
	}}
	listeners := []Listener{
		{Port: 9443, Protocol: "tcp", Process: "caddy-naive", Address: "127.0.0.1:9443"},
		{Port: 9444, Protocol: "tcp6", Process: "slipgate", Address: "[::1]:9444"},
		{Port: 443, Protocol: "tcp", Process: "cottenrouter", Address: "0.0.0.0:443"},
	}
	private, public := missingSlipGateTLSListeners(listeners, plan)
	if len(private) != 0 || len(public) != 0 {
		t.Fatalf("healthy TLS listeners reported missing: private=%v public=%v", private, public)
	}
	listeners[0].Address = "0.0.0.0:9443"
	listeners[2].Process = "xray"
	private, public = missingSlipGateTLSListeners(listeners, plan)
	if len(private) != 1 || len(public) != 1 || public[0] != 443 {
		t.Fatalf("unsafe/missing listeners were accepted: private=%v public=%v", private, public)
	}
}

func TestEnabledSlipGateServicesReattachOnlyEnabledTunnelsAndSocks(t *testing.T) {
	configuration := []byte(`{"listen":{"address":"0.0.0.0:53"},"tunnels":[{"tag":"dns","transport":"dnstt","backend":"ssh","domain":"dns.example","port":5310,"enabled":true,"dnstt":{}},{"tag":"web","transport":"naive","backend":"socks","domain":"web.example","enabled":true,"naive":{"port":443}},{"tag":"off","transport":"stuntls","backend":"ssh","domain":"","enabled":false,"stuntls":{"port":8443}}],"backends":[],"users":[],"route":{"mode":"multi"},"warp":{}}`)
	units := []byte("slipgate-dns.service disabled\nslipgate-web.service disabled\nslipgate-off.service enabled\nslipgate-socks5.service disabled\nslipgate-dnsrouter.service enabled\nslipgate-iptables.service enabled\n")
	got, err := enabledSlipGateManagedServices(configuration, units)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"slipgate-dns", "slipgate-socks5", "slipgate-web"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reattach services=%v want=%v", got, want)
	}
}

func TestInstallRollbackRestoresPriorServiceStateAndStopsNewSlipGateUnits(t *testing.T) {
	runner := &recordingIntegrationRunner{unitList: []byte("slipgate-dns.service enabled\nslipgate-web.service disabled\nslipgate-socks5.service disabled\n")}
	previous := []managedServiceState{{name: "slipgate-dns", active: "active", enabled: "enabled"}}
	stopNewSlipGateManagedServices(previous, runner)
	restoreManagedServicesExact(previous, runner)
	commands := strings.Join(runner.runs, "\n")
	for _, want := range []string{
		"systemctl disable --now slipgate-web",
		"systemctl disable --now slipgate-socks5",
		"systemctl enable slipgate-dns",
		"systemctl start slipgate-dns",
	} {
		if !strings.Contains(commands, want) {
			t.Errorf("rollback command %q missing from:\n%s", want, commands)
		}
	}
	if strings.Contains(commands, "disable --now slipgate-dns\n") {
		t.Fatalf("prior DNS tunnel was treated as a new service:\n%s", commands)
	}
}

func TestSlipGateTLSIntegratedRequiresEveryPlannedMapping(t *testing.T) {
	plan := SlipGateTLSPlan{Listeners: []config.TLSListener{
		{Name: "shared", Listen: "0.0.0.0:443", DefaultBackend: "127.0.0.1:9444", DefaultRouteName: "slipgate:stuntls:ssh", Routes: []config.TLSRoute{{Name: "slipgate:naive:web", ServerNames: []string{"web.example"}, Backend: "127.0.0.1:9443"}}},
		{Name: "other", Listen: "0.0.0.0:8443", Routes: []config.TLSRoute{{Name: "slipgate:naive:other", ServerNames: []string{"other.example"}, Backend: "127.0.0.1:9445"}}},
	}, Backends: []SlipGateTLSBackend{{Tag: "web"}, {Tag: "ssh"}, {Tag: "other"}}}
	if slipGateTLSPlanIntegrated(plan.Listeners[:1], plan) {
		t.Fatal("partial SlipGate TLS integration was reported as integrated")
	}
	if !slipGateTLSPlanIntegrated(plan.Listeners, plan) {
		t.Fatal("complete SlipGate TLS integration was not detected")
	}
}

func TestRemoveProjectConfigRemovesSlipGateDNSNaiveAndOwnedStunOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.json")
	previous := integrationTLSPlan(443, 9444, "stuntls")
	router := config.Config{
		Routes: []config.Route{
			{Name: "other", Domains: []string{"other.example"}, Backend: "127.0.0.1:5301", TCPBackend: "disabled"},
			{Name: "slipgate:dns", Domains: []string{"dns.example"}, Backend: "127.0.0.1:5310", TCPBackend: "disabled"},
		},
		TLSListeners: []config.TLSListener{{
			Name: "https", Listen: "0.0.0.0:443", DefaultBackend: previous.Listeners[0].DefaultBackend, DefaultRouteName: previous.Listeners[0].DefaultRouteName,
			Routes: []config.TLSRoute{{Name: "panel", ServerNames: []string{"panel.example"}, Backend: "127.0.0.1:10000"}, {Name: "slipgate:naive:web", ServerNames: []string{"web.example"}, Backend: "127.0.0.1:9443"}},
		}},
	}
	encoded, _ := json.Marshal(router)
	if err := os.WriteFile(path, encoded, 0644); err != nil {
		t.Fatal(err)
	}
	if err := removeProjectConfigWithSlipGateTLS(path, Spec{ID: "slipgate", Kind: ConfigSlipGate}, previous); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	var got config.Config
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Routes) != 1 || got.Routes[0].Name != "other" || len(got.TLSListeners) != 1 || len(got.TLSListeners[0].Routes) != 1 || got.TLSListeners[0].Routes[0].Name != "panel" || got.TLSListeners[0].DefaultBackend != "" || got.TLSListeners[0].DefaultRouteName != "" {
		t.Fatalf("detach removed unrelated state or left SlipGate state: %+v", got)
	}
}

func TestUpsertTLSMergesCottenDoHIntoSlipGateListenerByListen(t *testing.T) {
	existing := []config.TLSListener{{Name: "slipgate-tls-443", Listen: ":443", Routes: []config.TLSRoute{{Name: "slipgate:naive:web", ServerNames: []string{"web.example"}, Backend: "127.0.0.1:9443"}}}}
	got := upsertTLS(existing, "https", 443, "cottendns-doh", "doh.example", 8443)
	if len(got) != 1 || got[0].Name != "slipgate-tls-443" || len(got[0].Routes) != 2 {
		t.Fatalf("Cotten DoH created a duplicate listener instead of merging by Listen: %+v", got)
	}
	cfg := config.Config{Routes: []config.Route{{Name: "dns", Domains: []string{"dns.example"}, Backend: "127.0.0.1:5301", TCPBackend: "disabled"}}, TLSListeners: got}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("merged DoH/SlipGate listener is invalid: %v", err)
	}
}

func integrationTLSPlan(publicPort, privatePort int, transport string) SlipGateTLSPlan {
	backend := SlipGateTLSBackend{Tag: "web", Transport: transport, Service: "slipgate-web", PublicPort: publicPort, PrivatePort: privatePort}
	listener := config.TLSListener{Name: "slipgate", Listen: "0.0.0.0:" + strconv.Itoa(publicPort)}
	if transport == "stuntls" {
		listener.DefaultBackend = "127.0.0.1:" + strconv.Itoa(privatePort)
		listener.DefaultRouteName = "slipgate:stuntls:web"
	} else {
		backend.Domain = "web.example"
		backend.RouteName = "slipgate:naive:web"
		listener.Routes = []config.TLSRoute{{Name: backend.RouteName, ServerNames: []string{backend.Domain}, Backend: "127.0.0.1:" + strconv.Itoa(privatePort)}}
	}
	return SlipGateTLSPlan{Backends: []SlipGateTLSBackend{backend}, Listeners: []config.TLSListener{listener}}
}

type recordingIntegrationRunner struct {
	unitList []byte
	runs     []string
}

func (runner *recordingIntegrationRunner) Run(_ context.Context, name string, args []string, _ string, _ bool) error {
	runner.runs = append(runner.runs, name+" "+strings.Join(args, " "))
	return nil
}

func (runner *recordingIntegrationRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	if name == "systemctl" && len(args) > 0 && args[0] == "list-unit-files" {
		return append([]byte(nil), runner.unitList...), nil
	}
	return nil, nil
}

// SlipGate only creates slipgate-dnsrouter when a DNS tunnel exists, so a
// NaiveProxy/StunTLS-only install must not fail on the missing unit.
func TestDisableNativeSlipGateDNSRouterToleratesMissingUnit(t *testing.T) {
	runner := &stateRunner{active: "inactive", enabled: "not-found"}
	if err := disableNativeSlipGateDNSRouter(context.Background(), runner); err != nil {
		t.Fatal(err)
	}
	runner = &stateRunner{active: "active", enabled: "enabled", runErr: errors.New("boom")}
	if err := disableNativeSlipGateDNSRouter(context.Background(), runner); err == nil {
		t.Fatal("expected an error while the native DNS router still holds port 53")
	}
}

type stateRunner struct {
	active, enabled string
	runErr          error
}

func (runner *stateRunner) Run(context.Context, string, []string, string, bool) error {
	return runner.runErr
}

func (runner *stateRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	if name == "systemctl" && len(args) > 0 && args[0] == "is-active" {
		return []byte(runner.active), nil
	}
	if name == "systemctl" && len(args) > 0 && args[0] == "is-enabled" {
		return []byte(runner.enabled), nil
	}
	return nil, nil
}
