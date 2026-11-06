package installer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func panelBundleFixture(t *testing.T, version, arch string, edit func(map[string][]byte)) ([]byte, []byte, string) {
	t.Helper()
	asset, err := panelAssetForArch("panel-v"+version, arch)
	if err != nil {
		t.Fatal(err)
	}
	elf := make([]byte, 20)
	copy(elf, []byte{0x7f, 'E', 'L', 'F', 2, 1})
	machine := uint16(62)
	if arch == "arm64" {
		machine = 183
	}
	binary.LittleEndian.PutUint16(elf[18:20], machine)
	files := map[string][]byte{
		"ARCH": []byte(arch + "\n"), "panel-install.sh": []byte("installer"),
		"panel-security.sh": []byte("security"), "SOURCE-VERSIONS.txt": []byte("source provenance"),
	}
	for _, name := range []string{"soar-panel", "soar-panel-ops", "soar-check"} {
		files["bin/linux-"+arch+"/"+name] = append([]byte(nil), elf...)
	}
	if edit != nil {
		edit(files)
	}
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var manifest strings.Builder
	for _, name := range names {
		fmt.Fprintf(&manifest, "%x  %s\n", sha256.Sum256(files[name]), name)
	}
	files["SHA256SUMS"] = []byte(manifest.String())
	names = append(names, "SHA256SUMS")
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	archive := tar.NewWriter(gz)
	for _, name := range names {
		body := files[name]
		if err := archive.WriteHeader(&tar.Header{Name: strings.TrimSuffix(asset, ".tar.gz") + "/" + name, Mode: 0644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes(), []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(raw.Bytes()), asset)), asset
}

func TestPanelCurrentArchivesSupportBothNativeArchitectures(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			archive, checksum, asset := panelBundleFixture(t, "0.14.4", arch, nil)
			files, err := verifyPanelBundle(archive, checksum, asset)
			if err != nil {
				t.Fatal(err)
			}
			if len(files["bin/linux-"+arch+"/soar-panel"]) == 0 {
				t.Fatal("selected platform executable is absent")
			}
		})
	}
}

func TestPanelArchiveRejectsWrongDeclaredOrExecutableArchitecture(t *testing.T) {
	for name, edit := range map[string]func(map[string][]byte){
		"wrong ARCH":   func(files map[string][]byte) { files["ARCH"] = []byte("arm64\n") },
		"missing ARCH": func(files map[string][]byte) { delete(files, "ARCH") },
		"wrong executable": func(files map[string][]byte) {
			binary.LittleEndian.PutUint16(files["bin/linux-amd64/soar-panel"][18:20], 183)
		},
		"missing executable": func(files map[string][]byte) { delete(files, "bin/linux-amd64/soar-panel-ops") },
	} {
		t.Run(name, func(t *testing.T) {
			archive, checksum, asset := panelBundleFixture(t, "0.14.4", "amd64", edit)
			if _, err := verifyPanelBundle(archive, checksum, asset); err == nil {
				t.Fatal("incompatible platform bundle accepted")
			}
		})
	}
	archive, checksum, asset := panelBundleFixture(t, "0.5.1", "amd64", func(files map[string][]byte) { delete(files, "ARCH") })
	if _, err := verifyPanelBundle(archive, checksum, asset); err != nil {
		t.Fatalf("legacy explicitly selected bundle lost compatibility: %v", err)
	}
	checksum[0] = 'x'
	if _, err := verifyPanelBundle(archive, checksum, asset); err == nil {
		t.Fatal("archive checksum failure accepted")
	}
}

type panelContractRunner struct {
	version      string
	capabilities string
	help         map[string]string
	calls        []string
}

func (r *panelContractRunner) Run(context.Context, string, []string, string, bool) error {
	return errors.New("runtime verification must never mutate services")
}

func (r *panelContractRunner) Output(_ context.Context, _ string, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	r.calls = append(r.calls, key)
	if key == "version" {
		return []byte(r.version + "\n"), nil
	}
	if key == "capabilities" {
		if r.capabilities == "" {
			return []byte("unknown command"), errors.New("exit status 1")
		}
		return []byte(r.capabilities), nil
	}
	if result, ok := r.help[key]; ok {
		return []byte(result), errors.New("native main exits on flag help")
	}
	return nil, errors.New("unexpected command " + key)
}

func modernPanelRunner() *panelContractRunner {
	return &panelContractRunner{version: "0.14.4", help: map[string]string{
		"run --help":         "Usage of run:\n  -db string\n  -dns-listen string\n  -listen string\n  -ops-socket string\n",
		"init --help":        "Usage of init:\n  -db string\n  -zone string\n  -admin string\n  -password-file string\n",
		"health --help":      "Usage of health:\n  -db string\n  -listen string\n",
		"server-info --help": "Usage of server-info:\n  -db string\n",
	}}
}

func TestPanelModernRuntimeUsesCurrentCLIWithoutLegacyCapabilityCommand(t *testing.T) {
	runner := modernPanelRunner()
	if err := (Manager{Runner: runner}).verifyPanelRuntime(context.Background(), "candidate", "panel-v0.14.4"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runner.calls, []string{"version", "run --help", "init --help", "health --help", "server-info --help"}) {
		t.Fatalf("unexpected runtime commands: %v", runner.calls)
	}
}

func TestPanelRuntimeFailsClosedOnVersionOrCLIContractChanges(t *testing.T) {
	for _, kind := range []string{"version", "run", "init", "health", "server-info"} {
		t.Run(kind, func(t *testing.T) {
			runner := modernPanelRunner()
			if kind == "version" {
				runner.version = "0.14.3"
			} else {
				runner.help[kind+" --help"] = "Usage of " + kind + ":\n  -unrelated string\n"
			}
			if err := (Manager{Runner: runner}).verifyPanelRuntime(context.Background(), "candidate", "panel-v0.14.4"); err == nil {
				t.Fatal("incompatible native runtime accepted")
			}
		})
	}
	for _, capability := range []string{"", "not-account-session-limits", "account-session-limits"} {
		runner := &panelContractRunner{version: "0.5.1", capabilities: capability}
		err := (Manager{Runner: runner}).verifyPanelRuntime(context.Background(), "candidate", "panel-v0.5.1")
		if (err == nil) != (capability == "account-session-limits") {
			t.Fatalf("legacy capability %q result: %v", capability, err)
		}
	}
}

func TestPanelNativeServingZonesPreserveIdentityAndUnrelatedSettings(t *testing.T) {
	data := []byte("PANEL_ZONE=base.example\nPANEL_DNS_LISTEN=127.0.0.1:5307\nPANEL_HTTP_LISTEN=127.0.0.1:2054\nPANEL_NATIVE_TUNING=keep\n")
	exported := []byte(`{"administrator":"admin","accounts":12,"settings":{"base_zone":"base.example","tunnel_zones":["alias.example","backup.example"],"primary_zone":"backup.example","server_stream_limit":9999,"future_settings":{"preserve":true}}}`)
	configured, err := panelConfigWithNativeDomains(data, exported)
	if err != nil {
		t.Fatal(err)
	}
	domains, err := panelRouteDomains(configured)
	if err != nil || !reflect.DeepEqual(domains, []string{"alias.example", "backup.example"}) {
		t.Fatalf("native serving zones = %v, %v", domains, err)
	}
	if envValue(configured, "PANEL_ZONE") != "base.example" || envValue(configured, "PANEL_NATIVE_TUNING") != "keep" || strings.Contains(string(configured), "server_stream_limit") {
		t.Fatalf("identity or unrelated native configuration changed: %s", configured)
	}
	reset, err := panelConfigWithNativeDomains(configured, []byte(`{"settings":{"base_zone":"base.example"}}`))
	if err != nil {
		t.Fatal(err)
	}
	domains, err = panelRouteDomains(reset)
	if err != nil || !reflect.DeepEqual(domains, []string{"base.example"}) {
		t.Fatalf("removed serving zones remained cached: %v, %v", domains, err)
	}
}

func TestPanelNativeServingZoneExportValidation(t *testing.T) {
	data := []byte("PANEL_ZONE=base.example\n")
	for _, exported := range []string{
		`{"settings":{"base_zone":"changed.example"}}`,
		`{"settings":{"base_zone":"base.example","tunnel_zones":["same.example","same.example"]}}`,
		`{"settings":{"base_zone":"base.example","tunnel_zones":["a.example","nested.a.example"]}}`,
		`{"settings":{"base_zone":"base.example","tunnel_zones":["https://bad.example"]}}`,
		`{"settings":{"base_zone":"base.example","tunnel_zones":["alias.example"],"primary_zone":"missing.example"}}`,
		`{"settings":{"base_zone":"base.example"}} {}`,
	} {
		if _, err := panelConfigWithNativeDomains(data, []byte(exported)); err == nil {
			t.Fatalf("invalid native export accepted: %s", exported)
		}
	}
}

func TestPanelRequestRetainsNativeDomainLimits(t *testing.T) {
	for _, domain := range []string{"https://panel.example", "bad zone.example", "-bad.example", "toolongfornative.example.com"} {
		request := Request{ProjectID: "whitedns-panel", Domain: domain}
		if _, err := request.Validate(); err == nil {
			t.Fatalf("unsupported native Panel zone accepted: %s", domain)
		}
	}
	request := Request{ProjectID: "whitedns-panel", Domain: "BASE.Example.", EnableTCP: true}
	if _, err := request.Validate(); err != nil || request.Domain != "base.example" || request.PrivatePort != 5307 {
		t.Fatalf("valid native Panel request = %+v, %v", request, err)
	}
}
