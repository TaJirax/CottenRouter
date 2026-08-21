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
