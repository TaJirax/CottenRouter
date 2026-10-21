package installer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestUpdatePreservesExistingIntegrationSettings(t *testing.T) {
	state := ProjectState{ID: "cottendns", Name: "CottenDNS", Installed: true, Integrated: true, Domain: "t.example", ExtraDomains: "rotate.example", PrivatePort: 5411, EnableTCP: true, EnableDoT: true, EnableDoH: true, DoTDomain: "dot.example", DoHDomain: "doh.example"}
	request, err := updateRequest(state, "custom-router.json")
	if err != nil {
		t.Fatal(err)
	}
	want := Request{ProjectID: state.ID, Domain: state.Domain, ExtraDomains: state.ExtraDomains, PrivatePort: state.PrivatePort, EnableTCP: true, EnableDoT: true, EnableDoH: true, DoTDomain: state.DoTDomain, DoHDomain: state.DoHDomain, RouterConfig: "custom-router.json"}
	if !reflect.DeepEqual(request, want) {
		t.Fatalf("update changed settings: %+v", request)
	}
	for _, test := range []ProjectState{{ID: "cottendns", Name: "CottenDNS"}, {ID: "cottendns", Name: "CottenDNS", Installed: true}} {
		if _, err := updateRequest(test, "router.json"); err == nil {
			t.Fatal("update adopted a missing/detached backend")
		}
	}
}

func TestDiscoverPreservesImplicitTCPRouting(t *testing.T) {
	for _, test := range []struct {
		route   string
		enabled bool
	}{
		{`{"name":"cottendns","domain":"t.example","backend":"127.0.0.1:5301"}`, true},
		{`{"name":"cottendns","domain":"t.example","backend":"127.0.0.1:5301","tcp_backend":"disabled"}`, false},
	} {
		path := filepath.Join(t.TempDir(), "router.json")
		if err := os.WriteFile(path, []byte(`{"routes":[`+test.route+`]}`), 0600); err != nil {
			t.Fatal(err)
		}
		states, err := Discover(path)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, state := range states {
			if state.ID == "cottendns" {
				found = true
				if !state.Integrated || state.EnableTCP != test.enabled {
					t.Fatalf("TCP routing changed: %+v", state)
				}
			}
		}
		if !found {
			t.Fatal("CottenDNS state missing")
		}
	}
}

func TestCottenDNSBinarySelectionIgnoresOldExecutableMtime(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native bash installer")
	}
	dir := t.TempDir()
	for _, version := range []string{"v2026.09.26.091349-cdf084f", "v2026.10.08.163633-1f3cc52"} {
		if err := os.WriteFile(filepath.Join(dir, "CottenDns_Server_Linux_AMD64_"+version), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	old := filepath.Join(dir, "CottenDns_Server_Linux_AMD64_v2026.09.26.091349-cdf084f")
	if err := os.Chtimes(old, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	script := []byte("PREFIX=CottenDns_Server_Linux_AMD64\nTARGET_VERSION=v2026.10.08.163633-1f3cc52\n" + `EXECUTABLE="$(ls -t ${PREFIX}_v* 2>/dev/null | head -n1 || true)"` + "\nprintf '%s' \"$EXECUTABLE\"\n")
	spec, _ := FindSpec("cottendns")
	command := exec.Command("bash", "-c", string(prepareNativeInstaller(spec, script)))
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "CottenDns_Server_Linux_AMD64_v2026.10.08.163633-1f3cc52" {
		t.Fatalf("selected old binary: %s %v", output, err)
	}
}

type releaseProbeRunner struct{ binary, version string }

func (r releaseProbeRunner) Run(context.Context, string, []string, string, bool) error { return nil }
func (r releaseProbeRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	if name == "systemctl" {
		return []byte("{ path=" + r.binary + " ; argv[]=server ; }"), nil
	}
	if len(args) == 1 && args[0] == "--capabilities" {
		return []byte("cottendns-normal cottenmax dns-udp dns-tcp shared-key"), nil
	}
	return []byte(r.version), nil
}

func TestCottenDNSReleaseVerificationRejectsStaleOrUnmanagedBinary(t *testing.T) {
	spec, _ := FindSpec("cottendns")
	spec.WorkDir = t.TempDir()
	binary := filepath.Join(spec.WorkDir, "server")
	if err := os.WriteFile(binary, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	const expected = "v2026.10.08.163633-1f3cc52"
	for _, test := range []struct {
		version string
		valid   bool
	}{{"CottenDNS Server " + expected, true}, {"v2026.09.26.091349-cdf084f", false}, {expected + "-other", false}, {"", false}} {
		err := (Manager{Runner: releaseProbeRunner{binary: binary, version: test.version}}).verifyCottenDNSRelease(context.Background(), spec, expected)
		if (err == nil) != test.valid {
			t.Fatalf("verification of %q: %v", test.version, err)
		}
	}
	err := (Manager{Runner: releaseProbeRunner{binary: filepath.Join(t.TempDir(), "server"), version: expected}}).verifyCottenDNSRelease(context.Background(), spec, expected)
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("adopted unowned executable: %v", err)
	}
}
