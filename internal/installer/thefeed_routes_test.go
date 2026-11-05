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

func theFeedRouteFixture(t *testing.T) (Spec, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	spec, _ := FindSpec("thefeed")
	spec.WorkDir = dir
	spec.ConfigPath = filepath.Join(dir, "thefeed.env")
	path := filepath.Join(dir, "router.json")
	data, err := json.Marshal(config.Config{Routes: []config.Route{
		{Name: "other", Domains: []string{"other.example"}, Backend: "127.0.0.1:5301", TCPBackend: "disabled"},
		{Name: "thefeed", Domains: []string{"old.example", "old-mtp.example"}, Backend: "127.0.0.1:5304", TCPBackend: "disabled"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return spec, path, data
}

func TestBuildTheFeedRoutesImportsNativeFeatures(t *testing.T) {
	spec, path, _ := theFeedRouteFixture(t)
	native := []byte("THEFEED_DOMAIN=feed.example\nTHEFEED_EXTRA_DOMAINS=alias.example\n" +
		"THEFEED_CHAT_DOMAINS=chat.example\nTHEFEED_MTPROTO_DOMAINS=mtp.example\n" +
		"THEFEED_FUTURE_DOMAINS=future.example\nTHEFEED_MTPROTO_REPLY_BUDGET_MB=64\nTHEFEED_LISTEN=127.0.0.1:5304\n")
	if err := os.WriteFile(spec.ConfigPath, native, 0600); err != nil {
		t.Fatal(err)
	}
	request := Request{Domain: "feed.example", ExtraDomains: "alias.example", ChatDomains: "chat.example", PrivatePort: 5304}
	cfg, err := buildRouterConfig(path, spec, request, PortPlan{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"feed.example", "alias.example", "chat.example", "mtp.example", "future.example"}
	if len(cfg.Routes) != 2 || cfg.Routes[0].Name != "other" || !reflect.DeepEqual(cfg.Routes[1].Domains, want) {
		t.Fatalf("native features were not fully routed: %+v", cfg.Routes)
	}
	if cfg.Routes[1].Backend != "127.0.0.1:5304" || cfg.Routes[1].TCPBackend != "disabled" {
		t.Fatalf("native UDP backend changed: %+v", cfg.Routes[1])
	}
	after, err := os.ReadFile(spec.ConfigPath)
	if err != nil || string(after) != string(native) {
		t.Fatalf("route planning changed native settings: %v\n%s", err, after)
	}
}

func TestSyncTheFeedRoutesAddsAndRemovesNativeFeatureZones(t *testing.T) {
	spec, path, _ := theFeedRouteFixture(t)
	native := []byte("THEFEED_DOMAIN=feed.example\nTHEFEED_EXTRA_DOMAINS=alias.example\n" +
		"THEFEED_CHAT_DOMAINS=chat.example\nTHEFEED_MTPROTO_DOMAINS=new-mtp.example\n" +
		"THEFEED_FUTURE_DOMAINS=future.example\nTHEFEED_MTPROTO_MAX_SESSIONS=1500\nTHEFEED_LISTEN=127.0.0.1:5308\n")
	if err := os.WriteFile(spec.ConfigPath, native, 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncProjectRoute(path, spec); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"feed.example", "alias.example", "chat.example", "new-mtp.example", "future.example"}
	if len(cfg.Routes) != 2 || !reflect.DeepEqual(cfg.Routes[1].Domains, want) || cfg.Routes[1].Backend != "127.0.0.1:5308" {
		t.Fatalf("advanced native routes were not replaced: %+v", cfg.Routes)
	}
	if after, err := os.ReadFile(spec.ConfigPath); err != nil || string(after) != string(native) {
		t.Fatalf("native tuning was changed: %v\n%s", err, after)
	}
	if err := os.WriteFile(spec.ConfigPath, []byte("THEFEED_DOMAIN=feed.example\nTHEFEED_LISTEN=127.0.0.1:5308\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncProjectRoute(path, spec); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(path)
	if err != nil || !reflect.DeepEqual(cfg.Routes[1].Domains, []string{"feed.example"}) {
		t.Fatalf("removed native features left stale zones: %+v, %v", cfg.Routes, err)
	}
}

func TestTheFeedNativeFeatureConflictsAreAtomic(t *testing.T) {
	for _, operation := range []string{"update", "sync", "preflight"} {
		t.Run(operation, func(t *testing.T) {
			spec, path, original := theFeedRouteFixture(t)
			native := []byte("THEFEED_DOMAIN=feed.example\nTHEFEED_MTPROTO_DOMAINS=other.example\nTHEFEED_LISTEN=127.0.0.1:5304\n")
			if err := os.WriteFile(spec.ConfigPath, native, 0600); err != nil {
				t.Fatal(err)
			}
			request := Request{Domain: "feed.example", PrivatePort: 5304}
			var err error
			switch operation {
			case "update":
				err = updateRouterConfig(path, spec, request, PortPlan{})
			case "sync":
				err = syncProjectRoute(path, spec)
			case "preflight":
				err = preflightRouterConfig(path, spec, request, PortPlan{})
			}
			if err == nil || !strings.Contains(err.Error(), "other.example") {
				t.Fatalf("native feature conflict was not refused: %v", err)
			}
			if after, err := os.ReadFile(path); err != nil || string(after) != string(original) {
				t.Fatalf("failed operation changed router configuration: %v\n%s", err, after)
			}
			if after, err := os.ReadFile(spec.ConfigPath); err != nil || string(after) != string(native) {
				t.Fatalf("failed operation changed native feature settings: %v\n%s", err, after)
			}
		})
	}
}

func TestSyncTheFeedRejectsFinalPublicListenerAssignmentAtomically(t *testing.T) {
	for _, address := range []string{"0.0.0.0:53", "[::]:53"} {
		t.Run(address, func(t *testing.T) {
			spec, path, original := theFeedRouteFixture(t)
			native := []byte("THEFEED_DOMAIN=feed.example\nTHEFEED_MTPROTO_DOMAINS=mtp.example\n" +
				"THEFEED_LISTEN=127.0.0.1:5304\nTHEFEED_LISTEN=" + address + "\n")
			if err := os.WriteFile(spec.ConfigPath, native, 0600); err != nil {
				t.Fatal(err)
			}
			if err := syncProjectRoute(path, spec); err == nil {
				t.Fatalf("final public listener %s was hidden by an earlier loopback assignment", address)
			}
			if after, err := os.ReadFile(path); err != nil || string(after) != string(original) {
				t.Fatalf("rejected listener changed router configuration: %v\n%s", err, after)
			}
			if after, err := os.ReadFile(spec.ConfigPath); err != nil || string(after) != string(native) {
				t.Fatalf("rejected listener changed native configuration: %v\n%s", err, after)
			}
		})
	}
}

func TestSyncTheFeedUsesFinalPrivateListenerAssignment(t *testing.T) {
	spec, path, _ := theFeedRouteFixture(t)
	native := []byte("THEFEED_DOMAIN=old.example\nTHEFEED_DOMAIN=feed.example\nTHEFEED_MTPROTO_DOMAINS=mtp.example\n" +
		"THEFEED_LISTEN=127.0.0.1:5304\nTHEFEED_LISTEN=127.0.0.1:5309\n")
	if err := os.WriteFile(spec.ConfigPath, native, 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncProjectRoute(path, spec); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	route := cfg.Routes[1]
	if route.Backend != "127.0.0.1:5309" || !reflect.DeepEqual(route.Domains, []string{"feed.example", "mtp.example"}) {
		t.Fatalf("route does not match the final native assignments: %+v", route)
	}
}

func TestEnvValueUsesLastSystemdAssignment(t *testing.T) {
	for _, key := range []string{"THEFEED_LISTEN", "SOAR_LISTEN"} {
		data := []byte(key + "=127.0.0.1:5304\n# unrelated comment\n" + key + "=\"127.0.0.1:5309\"\r\n")
		if got := envValue(data, key); got != "127.0.0.1:5309" {
			t.Fatalf("%s = %q; wanted final EnvironmentFile assignment", key, got)
		}
	}
}
