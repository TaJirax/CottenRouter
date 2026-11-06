package installer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TaJirax/CottenRouter/internal/config"
)

func panelRouteFixture(t *testing.T) (Spec, Request, config.Config, []byte) {
	t.Helper()
	path := routerConfigWithFirstBackend(t)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := FindSpec("whitedns-panel")
	spec.WorkDir = t.TempDir()
	spec.ConfigPath = filepath.Join(spec.WorkDir, "panel.env")
	request := Request{ProjectID: spec.ID, Domain: "base.example", PrivatePort: 5307, EnableTCP: true, RouterConfig: path}
	data := []byte("PANEL_ZONE=base.example\nPANEL_TUNNEL_DOMAINS=alias.example,backup.example\n" +
		"PANEL_DNS_LISTEN=127.0.0.1:5307\nPANEL_HTTP_LISTEN=127.0.0.1:2054\nPANEL_NATIVE_TUNING=keep\n")
	if err := os.WriteFile(spec.ConfigPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	return spec, request, cfg, data
}

func TestBuildPanelRoutesUsesAllNativeServingZonesAndPreservesOtherBackends(t *testing.T) {
	spec, request, previous, native := panelRouteFixture(t)
	cfg, err := buildRouterConfig(request.RouterConfig, spec, request, PortPlan{DNSPort: 5307}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes) != 2 || !reflect.DeepEqual(cfg.Routes[0], previous.Routes[0]) || !reflect.DeepEqual(cfg.TLSListeners, previous.TLSListeners) || cfg.AdminListen != previous.AdminListen {
		t.Fatalf("unrelated backend or operator settings changed: %+v", cfg)
	}
	route := cfg.Routes[1]
	if route.Name != spec.ID || !reflect.DeepEqual(route.Domains, []string{"alias.example", "backup.example"}) || route.Backend != "127.0.0.1:5307" || route.TCPBackend != route.Backend {
		t.Fatalf("native serving zones or TCP semantics were not retained: %+v", route)
	}
	for _, domain := range route.Domains {
		if domain == request.Domain {
			t.Fatal("inactive identity base zone was re-added to the serving list")
		}
	}
	if after, err := os.ReadFile(spec.ConfigPath); err != nil || string(after) != string(native) {
		t.Fatalf("pure route planning changed native metadata: %v\n%s", err, after)
	}
}

func TestSyncPanelRoutesReplacesRemovedNativeZonesAndUsesPrivateTCPListener(t *testing.T) {
	spec, request, previous, native := panelRouteFixture(t)
	previous.Routes = append(previous.Routes, config.Route{
		Name: spec.ID, Domains: []string{"base.example", "removed.example"}, Backend: "127.0.0.1:5307", TCPBackend: "127.0.0.1:5307",
	})
	encoded, _ := json.Marshal(previous)
	if err := os.WriteFile(request.RouterConfig, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	native = setEnv(native, "PANEL_DNS_LISTEN", "127.0.0.1:5309")
	if err := os.WriteFile(spec.ConfigPath, native, 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncProjectRoute(request.RouterConfig, spec); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(request.RouterConfig)
	if err != nil {
		t.Fatal(err)
	}
	route := cfg.Routes[1]
	if !reflect.DeepEqual(route.Domains, []string{"alias.example", "backup.example"}) || route.Backend != "127.0.0.1:5309" || route.TCPBackend != route.Backend {
		t.Fatalf("old zones or backend addresses survived native synchronization: %+v", route)
	}
	if !reflect.DeepEqual(cfg.Routes[0], previous.Routes[0]) || !reflect.DeepEqual(cfg.TLSListeners, previous.TLSListeners) {
		t.Fatal("Panel sync changed unrelated backend routes")
	}
	native = setEnv(native, "PANEL_TUNNEL_DOMAINS", "backup.example")
	if err := os.WriteFile(spec.ConfigPath, native, 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncProjectRoute(request.RouterConfig, spec); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(request.RouterConfig)
	if err != nil || !reflect.DeepEqual(cfg.Routes[1].Domains, []string{"backup.example"}) {
		t.Fatalf("removed alias remained routed: %+v, %v", cfg.Routes, err)
	}
}

func TestPanelNativeZoneConflictsAndInvalidCacheLeaveConfigurationUntouched(t *testing.T) {
	for _, domains := range []string{"dns.example", "nested.example,nested.nested.example", "bad zone.example"} {
		for _, operation := range []string{"preflight", "update", "sync"} {
			t.Run(operation+"/"+domains, func(t *testing.T) {
				spec, request, _, native := panelRouteFixture(t)
				native = setEnv(native, "PANEL_TUNNEL_DOMAINS", domains)
				if err := os.WriteFile(spec.ConfigPath, native, 0600); err != nil {
					t.Fatal(err)
				}
				original, err := os.ReadFile(request.RouterConfig)
				if err != nil {
					t.Fatal(err)
				}
				switch operation {
				case "preflight":
					err = preflightRouterConfig(request.RouterConfig, spec, request, PortPlan{DNSPort: 5307})
				case "update":
					err = updateRouterConfig(request.RouterConfig, spec, request, PortPlan{DNSPort: 5307})
				case "sync":
					err = syncProjectRoute(request.RouterConfig, spec)
				}
				if err == nil {
					t.Fatalf("incompatible native zone list %q was accepted", domains)
				}
				if domains == "dns.example" && !strings.Contains(err.Error(), domains) {
					t.Fatalf("zone conflict did not identify the claimed domain: %v", err)
				}
				if after, err := os.ReadFile(request.RouterConfig); err != nil || string(after) != string(original) {
					t.Fatalf("failed synchronization changed router bytes: %v\n%s", err, after)
				}
				if after, err := os.ReadFile(spec.ConfigPath); err != nil || string(after) != string(native) {
					t.Fatalf("failed synchronization changed native metadata: %v\n%s", err, after)
				}
			})
		}
	}
}
