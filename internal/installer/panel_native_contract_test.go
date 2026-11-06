package installer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TaJirax/CottenRouter/internal/config"
)

type panelContractTransport struct {
	archive, checksum []byte
	fallback          http.RoundTripper
}

// This optional artifact check uses the production verifier on both actual
// published bundles. ARM64 bytes are verified but not executed on an x86 host.
func TestPanelPublishedNativeBundlesContract(t *testing.T) {
	dir := os.Getenv("COTTENROUTER_PANEL_BUNDLE_DIR")
	if dir == "" {
		t.Skip("set COTTENROUTER_PANEL_BUNDLE_DIR to the downloaded published archives")
	}
	for _, arch := range []string{"amd64", "arm64"} {
		asset := "soar-panel-0.14.4-linux-" + arch + ".tar.gz"
		archive := panelContractRead(t, filepath.Join(dir, asset))
		checksum := panelContractRead(t, filepath.Join(dir, asset+".sha256"))
		files, err := verifyPanelBundle(archive, checksum, asset)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(files["ARCH"])) != arch {
			t.Fatal("published architecture declaration mismatch")
		}
		t.Logf("published %s archive, complete manifest, required runtime layout and ELF machine: PASS", arch)
	}
}

func (p panelContractTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Hostname() == "127.0.0.1" {
		return p.fallback.RoundTrip(request)
	}
	var data []byte
	path := request.URL.Path
	switch {
	case request.URL.Host == "api.github.com" && path == "/repos/WhiteDNS/Soar-Server":
		data = []byte(`{"default_branch":"main"}`)
	case request.URL.Host == "api.github.com" && path == "/repos/WhiteDNS/Soar-Server/releases":
		data = []byte(`[{"tag_name":"panel-v0.14.4","assets":[{"name":"soar-panel-0.14.4-linux-amd64.tar.gz"},{"name":"soar-panel-0.14.4-linux-amd64.tar.gz.sha256"}]}]`)
	case request.URL.Host == "api.github.com" && path == "/repos/WhiteDNS/Soar-Server/commits/panel-v0.14.4":
		data = []byte(`{"sha":"7889645568ea5b430a6c64e0cc83da01a2454201"}`)
	case request.URL.Host == "raw.githubusercontent.com" && request.Method == http.MethodHead && strings.HasSuffix(path, "/panel-setup.sh"):
		data = nil
	case request.URL.Host == "github.com" && path == "/WhiteDNS/Soar-Server/releases/download/panel-v0.14.4/soar-panel-0.14.4-linux-amd64.tar.gz":
		data = p.archive
	case request.URL.Host == "github.com" && path == "/WhiteDNS/Soar-Server/releases/download/panel-v0.14.4/soar-panel-0.14.4-linux-amd64.tar.gz.sha256":
		data = p.checksum
	default:
		return nil, fmt.Errorf("unexpected native contract request: %s %s", request.Method, request.URL.Redacted())
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: request}, nil
}

// TestPanelNativeInstallerContract is opt-in and refuses an ordinary host. The
// companion shell script creates the marker only inside a private /run mount.
// GitHub metadata/release transport is substituted; actual Manager operations,
// OSRunner, published Panel binary, unit arguments, database and sockets run.
func TestPanelNativeInstallerContract(t *testing.T) {
	if os.Getenv("COTTENROUTER_NATIVE_PANEL_CONTRACT") != "1" {
		t.Skip("requires scripts/native-panel-installer-contract-test.sh")
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" || os.Geteuid() != 0 {
		t.Fatal("native Panel contract requires Linux amd64 root namespaces")
	}
	if marker, err := os.ReadFile("/run/cottenrouter-native-panel-contract"); err != nil || string(marker) != "private-namespace\n" {
		t.Fatal("missing private namespace marker; refusing host installation")
	}
	archive := panelContractRead(t, os.Getenv("CONTRACT_ARCHIVE"))
	checksum := panelContractRead(t, os.Getenv("CONTRACT_CHECKSUM"))
	previousTransport := http.DefaultTransport
	http.DefaultTransport = panelContractTransport{archive: archive, checksum: checksum, fallback: previousTransport}
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	manager := DefaultManager()
	spec, _ := FindSpec("whitedns-panel")
	const routerPath = "/etc/cottenrouter/config.json"
	for _, dir := range []string{filepath.Dir(routerPath), "/etc/systemd/system"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	panelContractWriteConfig(t, routerPath, config.Config{ListenUDP: "0.0.0.0:53", ListenTCP: "0.0.0.0:53", Routes: []config.Route{{Name: "bootstrap-placeholder", Domains: []string{"unconfigured.invalid"}, Backend: "127.0.0.1:5399", TCPBackend: "disabled"}}})
	passwordPath := "/run/panel-password"
	const password = "panel-native-contract-7A!"
	if err := os.WriteFile(passwordPath, []byte(password), 0600); err != nil {
		t.Fatal(err)
	}
	request := Request{ProjectID: spec.ID, Domain: "p.native.test", PrivatePort: 5307, EnableTCP: true, RouterConfig: routerPath, PanelPasswordFile: passwordPath}
	progress := func(message string) { t.Log(message) }
	if _, err := manager.Install(ctx, request, progress); err != nil {
		panelContractFatal(t, "fresh native install", err)
	}
	panelContractCheckDomains(t, routerPath, []string{"p.native.test"})
	panelContractCheckDNS(t, "p.native.test")
	unit := panelContractRead(t, "/etc/systemd/system/whitedns-panel.service")
	if !bytes.Contains(unit, []byte("WorkingDirectory="+spec.WorkDir)) || !bytes.Contains(unit, []byte("--ops-socket "+spec.WorkDir+"/router-managed-ops.sock")) {
		t.Fatal("managed native unit path changed")
	}
	client, csrf := panelContractLogin(t, password)
	account := panelContractAPI(t, client, csrf, http.MethodPost, "/api/clients", map[string]any{"name": "persistent contract", "enabled": true, "engines": "all", "quota_bytes": 123456789, "rate_bytes": 32768, "stream_limit": 7, "soar_session_limit": 9, "cotten_session_limit": 11, "masterdns_session_limit": 13})
	identity := panelContractIdentity(account)
	t.Log("fresh install, real generated unit/listeners, authenticated account and limits: PASS")

	// The original identity base is intentionally no longer a serving zone.
	panelContractAPI(t, client, csrf, http.MethodPut, "/api/tunnel-domains", map[string]any{"zones": []string{"next.native.test", "spare.native.test"}, "primary": "next.native.test"})
	if _, err := manager.Configure(ctx, request, progress); err != nil {
		panelContractFatal(t, "configure native serving zones", err)
	}
	panelContractCheckDomains(t, routerPath, []string{"next.native.test", "spare.native.test"})
	if value := envValue(panelContractRead(t, spec.ConfigPath), "PANEL_ZONE"); value != request.Domain {
		t.Fatal("identity base zone changed")
	}
	panelContractCheckDNS(t, "next.native.test")
	if _, err := manager.Update(ctx, spec.ID, routerPath, progress); err != nil {
		panelContractFatal(t, "update actual published Panel", err)
	}
	client, csrf = panelContractLogin(t, password)
	panelContractCheckIdentity(t, client, csrf, identity)
	panelContractCheckDomains(t, routerPath, []string{"next.native.test", "spare.native.test"})
	t.Log("native zone refresh excludes identity base; same-release update preserves account/key/quota/limits: PASS")

	panelContractAPI(t, client, csrf, http.MethodPut, "/api/tunnel-domains", map[string]any{"zones": []string{"last.native.test"}, "primary": "last.native.test"})
	t.Setenv("EDITOR", "/usr/local/bin/native-panel-editor")
	if err := manager.Advanced(ctx, spec.ID, routerPath); err != nil {
		panelContractFatal(t, "Advanced native serving zones", err)
	}
	panelContractCheckDomains(t, routerPath, []string{"last.native.test"})
	panelContractCheckDNS(t, "last.native.test")

	// A newly imported native zone can conflict after the old cached preflight
	// passes. The transaction must restore both files and the running services.
	cfg, err := config.Load(routerPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Routes = append(cfg.Routes, config.Route{Name: "unrelated", Domains: []string{"occupied.native.test"}, Backend: "127.0.0.1:5360", TCPBackend: "disabled"})
	panelContractWriteConfig(t, routerPath, cfg)
	panelContractCommand(t, "systemctl", "restart", "cottenrouter")
	client, csrf = panelContractLogin(t, password)
	panelContractAPI(t, client, csrf, http.MethodPut, "/api/tunnel-domains", map[string]any{"zones": []string{"occupied.native.test"}, "primary": "occupied.native.test"})
	beforeEnv, beforeRouter := panelContractRead(t, spec.ConfigPath), panelContractRead(t, routerPath)
	if _, err := manager.Configure(ctx, request, progress); err == nil {
		t.Fatal("conflicting native zone was accepted")
	}
	if !bytes.Equal(beforeEnv, panelContractRead(t, spec.ConfigPath)) || !bytes.Equal(beforeRouter, panelContractRead(t, routerPath)) {
		t.Fatal("route conflict changed pre-transaction config files")
	}
	for _, name := range []string{"whitedns-panel", "cottenrouter"} {
		panelContractCommand(t, "systemctl", "is-active", "--quiet", name)
	}
	client, csrf = panelContractLogin(t, password)
	panelContractCheckIdentity(t, client, csrf, identity)
	t.Log("Advanced refresh and post-export conflict rollback restore config and active services: PASS")
	panelContractCommand(t, "systemctl", "stop", "whitedns-panel")
	info := panelContractCommand(t, filepath.Join(spec.WorkDir, "whitedns-panel"), "server-info", "--db", filepath.Join(spec.WorkDir, "panel.db"))
	var exported map[string]any
	if err := json.Unmarshal(info, &exported); err != nil {
		t.Fatal(err)
	}
	panelContractCommand(t, "systemctl", "start", "whitedns-panel")
	t.Log("native published Panel install/update/configure/Advanced and rollback contract: PASS")
}

func panelContractIdentity(account map[string]any) map[string]any {
	identity := make(map[string]any)
	for _, key := range []string{"id", "zone", "public_key", "name", "enabled", "engines", "quota_bytes", "rate_bytes", "stream_limit", "soar_session_limit", "cotten_session_limit", "masterdns_session_limit"} {
		identity[key] = account[key]
	}
	return identity
}

func panelContractCheckIdentity(t *testing.T, client *http.Client, csrf string, want map[string]any) {
	t.Helper()
	list := panelContractAPI(t, client, csrf, http.MethodGet, "/api/clients", nil)
	for _, value := range list["clients"].([]any) {
		account := value.(map[string]any)
		if account["id"] == want["id"] {
			if !reflect.DeepEqual(panelContractIdentity(account), want) {
				t.Fatal("native account identity/key/quota/limits changed")
			}
			return
		}
	}
	t.Fatal("native account disappeared")
}

func panelContractLogin(t *testing.T, password string) (*http.Client, string) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	response := panelContractAPI(t, client, "", http.MethodPost, "/api/login", map[string]string{"name": "admin", "password": password})
	return client, response["csrf"].(string)
}

func panelContractAPI(t *testing.T, client *http.Client, csrf, method, path string, body any) map[string]any {
	t.Helper()
	data, _ := json.Marshal(body)
	request, err := http.NewRequest(method, "http://127.0.0.1:2054"+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://127.0.0.1:2054")
	request.Header.Set("X-CSRF-Token", csrf)
	response, err := client.Do(request)
	if err != nil {
		panelContractFatal(t, "native Panel API "+path, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail, _ := io.ReadAll(response.Body)
		t.Fatalf("native API %s HTTP%d: %s", path, response.StatusCode, detail)
	}
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func panelContractCheckDomains(t *testing.T, path string, want []string) {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range cfg.Routes {
		if route.Name == "whitedns-panel" {
			if !reflect.DeepEqual(route.Domains, want) || route.TCPBackend != route.Backend {
				t.Fatalf("native Panel routes %+v, want zones%v and native TCP", route, want)
			}
			return
		}
	}
	t.Fatal("missing Panel route")
}

func panelContractCheckDNS(t *testing.T, zone string) {
	t.Helper()
	packet := make([]byte, 12)
	binary.BigEndian.PutUint16(packet[:2], 0x7163)
	binary.BigEndian.PutUint16(packet[4:6], 1)
	for _, label := range strings.Split(zone, ".") {
		packet = append(packet, byte(len(label)))
		packet = append(packet, label...)
	}
	packet = append(packet, 0, 0, 6, 0, 1)
	for _, tcp := range []bool{false, true} {
		exchange := func(port int) []byte {
			protocol := "udp"
			if tcp {
				protocol = "tcp"
			}
			conn, err := net.DialTimeout(protocol, fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			if tcp {
				frame := append([]byte{byte(len(packet) >> 8), byte(len(packet))}, packet...)
				if _, err := conn.Write(frame); err != nil {
					t.Fatal(err)
				}
				var prefix [2]byte
				if _, err := io.ReadFull(conn, prefix[:]); err != nil {
					t.Fatal(err)
				}
				reply := make([]byte, binary.BigEndian.Uint16(prefix[:]))
				if _, err := io.ReadFull(conn, reply); err != nil {
					t.Fatal(err)
				}
				return reply
			}
			if _, err := conn.Write(packet); err != nil {
				t.Fatal(err)
			}
			reply := make([]byte, 4096)
			n, err := conn.Read(reply)
			if err != nil {
				t.Fatal(err)
			}
			return reply[:n]
		}
		if !bytes.Equal(exchange(53), exchange(5307)) {
			t.Fatal("native vs routed Panel DNS changed")
		}
	}
}

func panelContractRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func panelContractWriteConfig(t *testing.T, path string, cfg config.Config) {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
}
func panelContractCommand(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		panelContractFatal(t, name+" "+strings.Join(args, " "), fmt.Errorf("%w: %s", err, data))
	}
	return data
}
func panelContractFatal(t *testing.T, stage string, err error) {
	t.Helper()
	for _, path := range []string{"/run/whitedns-panel.log", "/run/cottenrouter.log", "/run/native-systemctl.log"} {
		if data, e := os.ReadFile(path); e == nil {
			if len(data) > 12000 {
				data = data[len(data)-12000:]
			}
			t.Logf("%s:\n%s", path, data)
		}
	}
	t.Fatalf("%s: %v", stage, err)
}
