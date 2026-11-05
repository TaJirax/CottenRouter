package installer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/TaJirax/CottenRouter/internal/config"
)

// routerConfigWithFirstBackend is a server that already runs cottendns with
// both encrypted transports, plus an operator who moved the admin port and
// turned DNS-over-TCP off.
func routerConfigWithFirstBackend(t *testing.T) string {
	t.Helper()
	cfg := config.Config{
		ListenUDP:   "0.0.0.0:53",
		AdminListen: "127.0.0.1:9090",
		Routes: []config.Route{
			{Name: "cottendns", Domains: []string{"dns.example"}, Backend: "127.0.0.1:5301", TCPBackend: "disabled"},
		},
		TLSListeners: []config.TLSListener{
			{Name: "dot", Listen: "0.0.0.0:853", Routes: []config.TLSRoute{
				{Name: "cottendns-dot", ServerNames: []string{"dot.example"}, Backend: "127.0.0.1:8853"},
			}},
			{Name: "https", Listen: "0.0.0.0:443", Routes: []config.TLSRoute{
				{Name: "cottendns-doh", ServerNames: []string{"doh.example"}, Backend: "127.0.0.1:8443"},
			}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	path := filepath.Join(t.TempDir(), "router.json")
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
	return path
}

// Installing a second protocol must add its route and leave everything the
// first protocol owns alone - including the encrypted routes it cannot use.
func TestSecondBackendKeepsTheFirstBackendsRoutesAndOperatorSettings(t *testing.T) {
	path := routerConfigWithFirstBackend(t)
	spec, _ := FindSpec("stormdns")
	request := Request{ProjectID: "stormdns", Domain: "storm.example", PrivatePort: 5303, RouterConfig: path}
	if err := updateRouterConfig(path, spec, request, PortPlan{DNSPort: 5303}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	names := map[string]string{}
	for _, route := range cfg.Routes {
		names[route.Name] = route.Backend
	}
	if names["cottendns"] != "127.0.0.1:5301" {
		t.Fatalf("first backend route lost: %+v", cfg.Routes)
	}
	if names["stormdns"] != "127.0.0.1:5303" {
		t.Fatalf("second backend was not routed: %+v", cfg.Routes)
	}

	tlsRoutes := map[string]bool{}
	for _, listener := range cfg.TLSListeners {
		for _, route := range listener.Routes {
			tlsRoutes[route.Name] = true
		}
	}
	if !tlsRoutes["cottendns-dot"] || !tlsRoutes["cottendns-doh"] {
		t.Fatalf("installing stormdns deleted cottendns encrypted routes: %+v", cfg.TLSListeners)
	}

	if cfg.AdminListen != "127.0.0.1:9090" {
		t.Fatalf("operator admin_listen was overwritten: %q", cfg.AdminListen)
	}
	if cfg.ListenTCP != "" {
		t.Fatalf("DNS-over-TCP was force-enabled for a backend that did not ask for it: %q", cfg.ListenTCP)
	}
}

// A backend that asks for TCP needs the router listening on TCP, so that one
// is seeded rather than left off.
func TestTCPBackendSeedsTheRouterTCPListener(t *testing.T) {
	path := routerConfigWithFirstBackend(t)
	spec, _ := FindSpec("cottendns")
	request := Request{ProjectID: "cottendns", Domain: "dns.example", PrivatePort: 5301, EnableTCP: true, RouterConfig: path}
	if err := updateRouterConfig(path, spec, request, PortPlan{DNSPort: 5301}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenTCP == "" {
		t.Fatal("router never started listening on TCP for a TCP backend")
	}
}

// A backend that is stopped during another install still owns its port: the
// router config routes to it, and it will bind again on its next start.
func TestPlannerAvoidsPortsRoutedToAStoppedBackend(t *testing.T) {
	path := routerConfigWithFirstBackend(t)
	spec, _ := FindSpec("masterdnsvpn")
	reserved := reservedRouterPorts(path, spec)

	ports := map[int]bool{}
	for _, listener := range reserved {
		ports[listener.Port] = true
	}
	for _, want := range []int{5301, 853, 443, 8853, 8443} {
		if !ports[want] {
			t.Fatalf("port %d in the router config was not reserved: %+v", want, reserved)
		}
	}

	// Nothing is listening anywhere, so only the config protects 5301.
	plan := PlanPorts(reserved, Request{PrivatePort: 5301})
	if plan.DNSPort == 5301 {
		t.Fatal("planner handed out a port the stopped first backend takes back on restart")
	}

	// The backend being installed keeps its own ports across a reinstall.
	cottendns, _ := FindSpec("cottendns")
	for _, listener := range reservedRouterPorts(path, cottendns) {
		if listener.Port == 5301 {
			t.Fatalf("a reinstall reserved the backend's own port against itself: %+v", listener)
		}
	}
}

// SlipGate picks tunnel ports from its own config only; a port another route
// already forwards to must be refused instead of cross-wiring two backends.
func TestSlipGateTunnelCannotReuseAnotherRoutesBackendPort(t *testing.T) {
	routerPath := routerConfigWithFirstBackend(t)
	spec, _ := FindSpec("slipgate")
	spec.ConfigPath = filepath.Join(t.TempDir(), "config.json")
	for port, wantErr := range map[string]bool{"5301": true, "5310": false} {
		data := `{"tunnels":[{"tag":"dnstt1","transport":"dnstt","domain":"t.example","port":` + port + `,"enabled":true}]}`
		if err := os.WriteFile(spec.ConfigPath, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := validateSlipGateDNSPorts(spec, routerPath); (err != nil) != wantErr {
			t.Fatalf("port %s: err=%v, want error=%v", port, err, wantErr)
		}
	}
}

func TestSlipGateReinstallRemovesDeletedAndDisabledDNSRoutes(t *testing.T) {
	routerPath := routerConfigWithFirstBackend(t)
	cfg, err := config.Load(routerPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Routes = append(cfg.Routes,
		config.Route{Name: "slipgate:deleted", Domains: []string{"deleted.example"}, Backend: "127.0.0.1:5310", TCPBackend: "udp"},
		config.Route{Name: "slipgate:disabled", Domains: []string{"disabled.example"}, Backend: "127.0.0.1:5311", TCPBackend: "udp"})
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(routerPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	spec, _ := FindSpec("slipgate")
	spec.ConfigPath = filepath.Join(t.TempDir(), "config.json")
	data = []byte(`{"tunnels":[{"tag":"disabled","transport":"dnstt","domain":"disabled.example","port":5311,"enabled":false},{"tag":"soar","transport":"soar","domain":"soar.example","port":5312,"enabled":true}]}`)
	if err := os.WriteFile(spec.ConfigPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := updateRouterConfig(routerPath, spec, Request{}, PortPlan{}); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(routerPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes) != 2 || cfg.Routes[0].Name != "cottendns" || cfg.Routes[1].Name != "slipgate:soar" || cfg.Routes[1].TCPBackend != "udp" {
		t.Fatalf("reinstalled routes = %+v", cfg.Routes)
	}
	if len(cfg.TLSListeners) != 2 || cfg.AdminListen != "127.0.0.1:9090" {
		t.Fatalf("reinstall changed unrelated router settings: %+v", cfg)
	}
}

// A domain another route owns must be refused before the upstream installer
// runs; the router used to stay down for the whole doomed install.
func TestPreflightRejectsDomainOwnedByAnotherRouteWithoutWriting(t *testing.T) {
	path := routerConfigWithFirstBackend(t)
	before, _ := os.ReadFile(path)
	spec, _ := FindSpec("masterdnsvpn")
	request := Request{ProjectID: spec.ID, Domain: "dns.example", PrivatePort: 5302, RouterConfig: path}
	if err := preflightRouterConfig(path, spec, request, PortPlan{DNSPort: 5302}); err == nil {
		t.Fatal("expected the duplicate domain to be refused")
	}
	request.Domain = "vpn.example"
	if err := preflightRouterConfig(path, spec, request, PortPlan{DNSPort: 5302}); err != nil {
		t.Fatalf("free domain refused: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("preflight modified the router config")
	}
}
