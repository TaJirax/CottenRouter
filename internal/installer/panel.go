package installer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/TaJirax/CottenRouter/internal/catalog"
)

// This opt-in beta stays on its CI channel. It is never republished as a stable
// Panel release or selected by automatic stable release discovery.
const panelBetaDigest = "59f64f2de6a5fbbc652bd93e0cd9e2ab4675816b9cbf27a1324f66eceab8f90b"
const panelBetaBuild = "https://github.com/WhiteDNS/Soar-Server/actions/runs/39619905951/artifacts/12354244178"

const (
	maxPanelArchiveBytes = 96 << 20
	maxPanelBundleBytes  = 256 << 20
	maxPanelMemberBytes  = 96 << 20
)

var panelTagRE = regexp.MustCompile(`^panel-v([0-9]+)\.([0-9]+)\.([0-9]+)$`)
var panelAssetRE = regexp.MustCompile(`^soar-panel-([0-9]+\.[0-9]+\.[0-9]+)-linux-(amd64|arm64)\.tar\.gz$`)

func panelBetaProject() catalog.Project {
	for _, project := range catalog.Projects() {
		if project.ID == "whitedns-panel" {
			project.ReleaseTag = "panel-v0.5.1"
			project.CommitSHA = "50c695b99dbc9a6d4cc912e5bc99c92fc103a58c"
			return project
		}
	}
	panic("WhiteDNS Panel is absent from catalog")
}

func readPanelBetaBundle(name string) ([]byte, []byte, error) {
	if err := rejectSymlinkComponents(name); err != nil {
		return nil, nil, err
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 32<<20 {
		return nil, nil, fmt.Errorf("Panel beta bundle must be a regular archive of at most 32 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, (32<<20)+1))
	if err != nil {
		return nil, nil, err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != panelBetaDigest {
		return nil, nil, fmt.Errorf("Panel beta archive differs from tested CI build; download and unzip %s", panelBetaBuild)
	}
	return data, []byte(panelBetaDigest + "  soar-panel-0.5.1-linux-amd64.tar.gz\n"), nil
}

func panelAsset(tag string) (string, error) {
	return panelAssetForArch(tag, runtime.GOARCH)
}

func panelAssetForArch(tag, arch string) (string, error) {
	if !panelTagRE.MatchString(tag) {
		return "", fmt.Errorf("invalid panel release tag")
	}
	if arch != "amd64" && arch != "arm64" {
		return "", fmt.Errorf("WhiteDNS Panel releases require Linux amd64 or arm64")
	}
	return "soar-panel-" + strings.TrimPrefix(tag, "panel-v") + "-linux-" + arch + ".tar.gz", nil
}

func verifyPanelBundle(archive, checksum []byte, asset string) (map[string][]byte, error) {
	match := panelAssetRE.FindStringSubmatch(asset)
	if len(match) != 3 {
		return nil, fmt.Errorf("invalid panel archive name")
	}
	version, arch := match[1], match[2]
	if len(archive) > maxPanelArchiveBytes {
		return nil, fmt.Errorf("panel archive exceeds its size limit")
	}
	digest := sha256.Sum256(archive)
	fields := strings.Fields(string(checksum))
	if len(fields) != 2 || !strings.EqualFold(fields[0], hex.EncodeToString(digest[:])) || strings.TrimPrefix(fields[1], "*") != asset {
		return nil, fmt.Errorf("WhiteDNS Panel archive checksum failed")
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: gz, N: maxPanelBundleBytes + 1}
	tr := tar.NewReader(limited)
	prefix := strings.TrimSuffix(asset, ".tar.gz") + "/"
	files := map[string][]byte{}
	seen := map[string]bool{}
	for count := 0; ; count++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if count >= 256 || !strings.HasPrefix(name+"/", prefix) || path.Clean(name) != name || strings.ContainsAny(name, `\:`) || seen[name] {
			return nil, fmt.Errorf("unsafe panel archive path %q", name)
		}
		seen[name] = true
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > maxPanelMemberBytes {
			return nil, fmt.Errorf("unsupported panel archive member %q", name)
		}
		data, err := io.ReadAll(tr)
		if err != nil || int64(len(data)) != h.Size {
			return nil, fmt.Errorf("incomplete panel archive")
		}
		files[strings.TrimPrefix(name, prefix)] = data
	}
	if _, err := io.Copy(io.Discard, limited); err != nil || limited.N == 0 {
		return nil, fmt.Errorf("panel archive exceeds its unpacked size limit")
	}
	verified := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(files["SHA256SUMS"])), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, fmt.Errorf("invalid panel manifest")
		}
		name := strings.TrimPrefix(f[1], "*")
		data, ok := files[name]
		sum := sha256.Sum256(data)
		if !ok || name == "SHA256SUMS" || verified[name] || !strings.EqualFold(f[0], hex.EncodeToString(sum[:])) {
			return nil, fmt.Errorf("panel manifest checksum failed for %s", name)
		}
		verified[name] = true
	}
	for name := range files {
		if name != "SHA256SUMS" && !verified[name] {
			return nil, fmt.Errorf("panel archive member %s is absent from manifest", name)
		}
	}
	binDir := "bin/linux-" + arch + "/"
	for _, name := range []string{"panel-install.sh", "panel-security.sh", binDir + "soar-panel", binDir + "soar-panel-ops", binDir + "soar-check", "SOURCE-VERSIONS.txt"} {
		if len(files[name]) == 0 || !verified[name] {
			return nil, fmt.Errorf("panel bundle is missing %s", name)
		}
	}
	if bundleArch, ok := files["ARCH"]; ok {
		if strings.TrimSpace(string(bundleArch)) != arch {
			return nil, fmt.Errorf("panel archive architecture differs from selected platform %s", arch)
		}
	} else if modernPanelCLI("panel-v" + version) {
		return nil, fmt.Errorf("current panel archive is missing its architecture declaration")
	}
	for _, name := range []string{"soar-panel", "soar-panel-ops", "soar-check"} {
		if err := verifyPanelELF(files[binDir+name], arch); err != nil {
			return nil, fmt.Errorf("panel archive %s: %w", name, err)
		}
	}
	return files, nil
}

func verifyPanelELF(data []byte, arch string) error {
	machine := uint16(62)
	if arch == "arm64" {
		machine = 183
	}
	if len(data) < 20 || !bytes.Equal(data[:6], []byte{0x7f, 'E', 'L', 'F', 2, 1}) || binary.LittleEndian.Uint16(data[18:20]) != machine {
		return fmt.Errorf("executable is not Linux %s ELF", arch)
	}
	return nil
}

// 0.14.4 is the first modern CLI verified by this adapter. It no longer has
// the beta's capabilities subcommand; earlier versions retain that safety gate.
func modernPanelCLI(tag string) bool {
	parts := panelTagRE.FindStringSubmatch(tag)
	if len(parts) != 4 {
		return false
	}
	minimum := []uint64{0, 14, 4}
	for index, part := range parts[1:] {
		n, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return false
		}
		if n != minimum[index] {
			return n > minimum[index]
		}
	}
	return true
}

func (m Manager) verifyPanelRuntime(ctx context.Context, candidate, tag string) error {
	version, err := m.Runner.Output(ctx, candidate, "version")
	if err != nil || strings.TrimSpace(string(version)) != strings.TrimPrefix(tag, "panel-v") {
		return fmt.Errorf("panel binary version differs from selected release %s", tag)
	}
	if !modernPanelCLI(tag) {
		features, err := m.Runner.Output(ctx, candidate, "capabilities")
		found := false
		for _, feature := range strings.Fields(string(features)) {
			found = found || feature == "account-session-limits"
		}
		if err == nil && found {
			return nil
		}
		return fmt.Errorf("the selected Panel lacks the required account-session-limits capability; no service was stopped. For explicit legacy beta testing, download and unzip %s and pass --panel-beta-bundle /path/to/soar-panel-0.5.1-linux-amd64.tar.gz", panelBetaBuild)
	}
	for _, contract := range []struct {
		command string
		flags   []string
	}{
		{"run", []string{"db", "dns-listen", "listen", "ops-socket"}},
		{"init", []string{"db", "zone", "admin", "password-file"}},
		{"health", []string{"db", "listen"}},
		{"server-info", []string{"db"}},
	} {
		// Go's flag help exits nonzero through the native main function. Inspect
		// its explicit usage/options rather than treating that exit as failure.
		help, _ := m.Runner.Output(ctx, candidate, contract.command, "--help")
		if !strings.Contains(string(help), "Usage of "+contract.command+":") {
			return fmt.Errorf("Panel %s does not expose the required %s CLI contract; no service was stopped", tag, contract.command)
		}
		for _, name := range contract.flags {
			if !regexp.MustCompile(`(?m)^\s+-` + regexp.QuoteMeta(name) + `(?:\s|$)`).Match(help) {
				return fmt.Errorf("Panel %s is missing required %s option --%s; no service was stopped", tag, contract.command, name)
			}
		}
	}
	return nil
}

func configurePanel(request Request, data []byte) ([]byte, error) {
	if old := envValue(data, "PANEL_ZONE"); old != "" && old != request.Domain {
		return nil, fmt.Errorf("preserve the panel base zone %s; changing it invalidates account profiles", old)
	}
	data = setEnv(data, "PANEL_ZONE", request.Domain)
	data = setEnv(data, "PANEL_DNS_LISTEN", fmt.Sprintf("127.0.0.1:%d", request.PrivatePort))
	data = seedEnv(data, "PANEL_HTTP_LISTEN", "127.0.0.1:2054")
	data = seedEnv(data, "PANEL_ADMIN", "admin")
	if err := validatePanelConfig(data); err != nil {
		return nil, err
	}
	return data, nil
}

func validatePanelConfig(data []byte) error {
	if err := validatePanelZone(envValue(data, "PANEL_ZONE")); err != nil {
		return err
	}
	for _, key := range []string{"PANEL_DNS_LISTEN", "PANEL_HTTP_LISTEN"} {
		host, p, err := net.SplitHostPort(envValue(data, key))
		n, e := strconv.Atoi(p)
		if err != nil || e != nil || host != "127.0.0.1" || n < 1024 || n > 65535 {
			return fmt.Errorf("%s must use a private loopback port between 1024 and 65535", key)
		}
	}
	_, err := panelRouteDomains(data)
	return err
}

func validatePanelZone(zone string) error {
	if len(zone) < 3 || len(zone) > 24 || !strings.Contains(zone, ".") {
		return fmt.Errorf("WhiteDNS Panel zones must be ASCII domains of 3-24 characters")
	}
	for _, label := range strings.Split(zone, ".") {
		if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("invalid WhiteDNS Panel zone %q", zone)
		}
		for _, character := range label {
			if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-') {
				return fmt.Errorf("invalid WhiteDNS Panel zone %q", zone)
			}
		}
	}
	return nil
}

// panelRouteDomains is a cached native serving-zone list. The identity base
// remains in PANEL_ZONE even when the operator stops serving that delegation.
func panelRouteDomains(data []byte) ([]string, error) {
	base := envValue(data, "PANEL_ZONE")
	if err := validatePanelZone(base); err != nil {
		return nil, err
	}
	cached := envValue(data, "PANEL_TUNNEL_DOMAINS")
	domains := strings.Split(cached, ",")
	if cached == "" {
		domains = []string{base}
	}
	if len(domains) > 8 {
		return nil, fmt.Errorf("WhiteDNS Panel supports at most eight serving zones")
	}
	seen := make(map[string]bool)
	for index, domain := range domains {
		domain = strings.TrimSpace(domain)
		domains[index] = domain
		if err := validatePanelZone(domain); err != nil {
			return nil, err
		}
		if seen[domain] {
			return nil, fmt.Errorf("duplicate WhiteDNS Panel serving zone %q", domain)
		}
		for previous := range seen {
			if strings.HasSuffix(previous, "."+domain) || strings.HasSuffix(domain, "."+previous) {
				return nil, fmt.Errorf("WhiteDNS Panel serving zones cannot be nested")
			}
		}
		seen[domain] = true
	}
	return domains, nil
}

// refreshPanelTunnelDomains uses the native non-secret export. Callers must
// stop the service first: the CLI opens its database exclusively. No database
// format, account settings or keys are interpreted or changed by CottenRouter.
func (m Manager) refreshPanelTunnelDomains(ctx context.Context, spec Spec, data []byte) ([]byte, error) {
	binary := filepath.Join(spec.WorkDir, "whitedns-panel")
	version, err := m.Runner.Output(ctx, binary, "version")
	if err != nil {
		return nil, fmt.Errorf("read installed Panel version before refreshing zones: %w", err)
	}
	if !modernPanelCLI("panel-v" + strings.TrimSpace(string(version))) {
		return setEnv(data, "PANEL_TUNNEL_DOMAINS", envValue(data, "PANEL_ZONE")), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	exported, err := m.Runner.Output(ctx, binary, "server-info", "--db", filepath.Join(spec.WorkDir, "panel.db"))
	if err != nil {
		return nil, fmt.Errorf("read native Panel serving zones while its service is stopped: %w", err)
	}
	return panelConfigWithNativeDomains(data, exported)
}

func panelConfigWithNativeDomains(data, exported []byte) ([]byte, error) {
	if len(exported) > 1<<20 {
		return nil, fmt.Errorf("native Panel server-info export exceeds its size limit")
	}
	var document struct {
		Settings struct {
			BaseZone    string   `json:"base_zone"`
			TunnelZones []string `json:"tunnel_zones"`
			PrimaryZone string   `json:"primary_zone"`
		} `json:"settings"`
	}
	decoder := json.NewDecoder(bytes.NewReader(exported))
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode native Panel serving zones: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("native Panel server-info contains trailing data")
	}
	if document.Settings.BaseZone != envValue(data, "PANEL_ZONE") {
		return nil, fmt.Errorf("native Panel identity base zone differs from PANEL_ZONE; preserve its existing identity")
	}
	values := document.Settings.TunnelZones
	if len(values) == 0 {
		values = []string{document.Settings.BaseZone}
	}
	configured := setEnv(data, "PANEL_TUNNEL_DOMAINS", strings.Join(values, ","))
	domains, err := panelRouteDomains(configured)
	if err != nil {
		return nil, err
	}
	if primary := document.Settings.PrimaryZone; primary != "" {
		found := false
		for _, domain := range domains {
			found = found || domain == primary
		}
		if !found {
			return nil, fmt.Errorf("native Panel primary zone is absent from its serving zones")
		}
	}
	return configured, nil
}

func panelUnit(spec Spec) string {
	return fmt.Sprintf(`[Unit]
Description=WhiteDNS Panel managed by CottenRouter
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=0
[Service]
Type=simple
WorkingDirectory=%s
EnvironmentFile=%s
ExecStart=%s run --db %s --dns-listen ${PANEL_DNS_LISTEN} --listen ${PANEL_HTTP_LISTEN} --ops-socket %s
Restart=on-failure
RestartSec=3
KillMode=control-group
TimeoutStopSec=15
[Install]
WantedBy=multi-user.target
`, spec.WorkDir, spec.ConfigPath, filepath.Join(spec.WorkDir, "whitedns-panel"), filepath.Join(spec.WorkDir, "panel.db"), filepath.Join(spec.WorkDir, "router-managed-ops.sock"))
}

func requirePanelOwnership(spec Spec) error {
	if spec.Kind != ConfigPanel {
		return nil
	}
	unit, err := os.ReadFile("/etc/systemd/system/" + spec.Service + ".service")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Contains(unit, []byte("Description=WhiteDNS Panel managed by CottenRouter")) || !bytes.Contains(unit, []byte("WorkingDirectory="+spec.WorkDir+"\n")) {
		return fmt.Errorf("refusing to replace a panel unit not owned by CottenRouter")
	}
	return nil
}

func (m Manager) installPanel(ctx context.Context, spec Spec, request Request, plan PortPlan, project catalog.Project, progress Progress) (result error) {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return fmt.Errorf("WhiteDNS Panel releases require Linux amd64 or arm64")
	}
	if request.PanelBetaBundle != "" && runtime.GOARCH != "amd64" {
		return fmt.Errorf("the pinned WhiteDNS Panel 0.5.1 beta bundle requires Linux amd64; use the current stable release on arm64")
	}
	if err := requirePanelOwnership(spec); err != nil {
		return err
	}
	if project.ID != "whitedns-panel" || project.RepoFullName != "WhiteDNS/Soar-Server" {
		return fmt.Errorf("untrusted panel project")
	}
	unitPath := "/etc/systemd/system/" + spec.Service + ".service"
	for _, name := range []string{spec.WorkDir, spec.ConfigPath, request.RouterConfig, unitPath} {
		if err := rejectSymlinkComponents(name); err != nil {
			return err
		}
	}
	previous, err := os.ReadFile(spec.ConfigPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	configured, err := configurePanel(request, previous)
	if err != nil {
		return err
	}
	if len(previous) == 0 {
		listeners, err := m.Scan(ctx)
		if err != nil {
			return err
		}
		used := map[int]bool{}
		for _, l := range listeners {
			if strings.HasPrefix(strings.ToLower(l.Protocol), "tcp") {
				used[l.Port] = true
			}
		}
		port := 2054
		for used[port] && port < 2154 {
			port++
		}
		if used[port] {
			return fmt.Errorf("no private panel administration port available")
		}
		configured = setEnv(configured, "PANEL_HTTP_LISTEN", fmt.Sprintf("127.0.0.1:%d", port))
	}
	if err := os.MkdirAll(spec.WorkDir, 0750); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(spec.WorkDir, ".panel-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	asset, err := panelAsset(project.ReleaseTag)
	if err != nil {
		return err
	}
	var archive, checksum []byte
	channel := "stable"
	if request.PanelBetaBundle != "" {
		channel = "beta"
		progress("Explicit Panel 0.5.1 beta selected; verifying the pinned CI archive")
		archive, checksum, err = readPanelBetaBundle(request.PanelBetaBundle)
	} else {
		base := "https://github.com/WhiteDNS/Soar-Server/releases/download/" + project.ReleaseTag + "/"
		progress("Downloading current stable WhiteDNS Panel " + project.ReleaseTag + " and verifying archive/runtime checksums")
		archive, err = m.downloadLimited(ctx, base+asset, maxPanelArchiveBytes)
		if err == nil {
			checksum, err = m.download(ctx, base+asset+".sha256")
		}
	}
	if err != nil {
		return err
	}
	files, err := verifyPanelBundle(archive, checksum, asset)
	if err != nil {
		return err
	}
	candidate := filepath.Join(staging, "whitedns-panel")
	panelBinary := files["bin/linux-"+runtime.GOARCH+"/soar-panel"]
	if err := os.WriteFile(candidate, panelBinary, 0755); err != nil {
		return err
	}
	if err := m.verifyPanelRuntime(ctx, candidate, project.ReleaseTag); err != nil {
		return err
	}
	db := filepath.Join(spec.WorkDir, "panel.db")
	if err := rejectSymlinkComponents(db); err != nil {
		return err
	}
	state := snapshotManagedServiceState(ctx, m.Runner, spec.Service)
	if err := m.Runner.Run(ctx, "systemctl", []string{"stop", spec.Service}, "/", false); err != nil && state.active == "active" {
		return err
	}
	type snapshot struct {
		path   string
		data   []byte
		mode   os.FileMode
		exists bool
	}
	var snapshots []snapshot
	mutated := false
	defer func() {
		if result == nil {
			return
		}
		if mutated {
			_ = m.Runner.Run(context.Background(), "systemctl", []string{"stop", spec.Service}, "/", false)
			for _, s := range snapshots {
				if s.exists {
					result = errors.Join(result, atomicWrite(s.path, s.data, s.mode))
				} else if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
					result = errors.Join(result, err)
				}
			}
			_ = m.Runner.Run(context.Background(), "systemctl", []string{"daemon-reload"}, "/", false)
		}
		restoreManagedServicesExact([]managedServiceState{state}, m.Runner)
		if mutated {
			_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", "cottenrouter"}, "/", false)
		}
	}()
	for _, name := range []string{spec.ConfigPath, request.RouterConfig, unitPath, db, filepath.Join(spec.WorkDir, "access.json"), filepath.Join(spec.WorkDir, "whitedns-panel"), filepath.Join(spec.WorkDir, "upstream.json"), "/etc/systemd/system/cottenrouter-backends.slice", filepath.Join("/etc/systemd/system", spec.Service+".service.d", "cottenrouter.conf")} {
		if err := rejectSymlinkComponents(name); err != nil {
			return err
		}
		s := snapshot{path: name, mode: 0600}
		info, e := os.Stat(name)
		if e == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("panel path is not a regular file: %s", name)
			}
			s.exists = true
			s.mode = info.Mode().Perm()
			s.data, err = os.ReadFile(name)
			if err != nil {
				return err
			}
		} else if !os.IsNotExist(e) {
			return e
		}
		snapshots = append(snapshots, s)
	}
	mutated = true
	binary := filepath.Join(spec.WorkDir, "whitedns-panel")
	if err := atomicWrite(binary, panelBinary, 0755); err != nil {
		return err
	}
	if _, err := os.Stat(db); os.IsNotExist(err) {
		progress("Creating the private panel database; choose the administrator password in the terminal")
		args := []string{"init", "--db", db, "--zone", request.Domain, "--admin", "admin"}
		if request.PanelPasswordFile != "" {
			if err := rejectSymlinkComponents(request.PanelPasswordFile); err != nil {
				return err
			}
			args = append(args, "--password-file", request.PanelPasswordFile)
			if err := m.Runner.Run(ctx, binary, args, spec.WorkDir, true); err != nil {
				return err
			}
		} else {
			const prompt = `set -euo pipefail
test -t 0 || { echo 'A terminal or --panel-password-file is required for initial administrator setup.' >&2; exit 1; }
umask 077
password_file=$(mktemp /run/cottenrouter-panel-password.XXXXXX)
trap 'rm -f -- "$password_file"; unset password confirmation' EXIT
while :; do
IFS= read -r -s -p 'WhiteDNS Panel administrator password (12-72 characters): ' password; printf '\n'
IFS= read -r -s -p 'Repeat password: ' confirmation; printf '\n'
[[ "$password" == "$confirmation" && ${#password} -ge 12 && ${#password} -le 72 ]] && break
echo 'Passwords must match and be 12-72 characters.'
done
printf '%s' "$password" > "$password_file"
"$1" init --db "$2" --zone "$3" --admin admin --password-file "$password_file"
`
			if err := m.Runner.Run(ctx, "bash", []string{"-c", prompt, "bash", binary, db, request.Domain}, spec.WorkDir, true); err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	configured, err = m.refreshPanelTunnelDomains(ctx, spec, configured)
	if err != nil {
		return err
	}
	if err := atomicWrite(spec.ConfigPath, configured, 0600); err != nil {
		return err
	}
	if err := atomicWrite(unitPath, []byte(panelUnit(spec)), 0644); err != nil {
		return err
	}
	if err := m.installContainment(ctx, spec); err != nil {
		return err
	}
	if err := updateRouterConfig(request.RouterConfig, spec, request, plan); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", spec.Service}, {"restart", spec.Service}} {
		if err := m.Runner.Run(ctx, "systemctl", args, "/", false); err != nil {
			return err
		}
	}
	if err := m.requireActive(ctx, spec.Service, spec.Name); err != nil {
		return err
	}
	if err := m.waitForPrivateListeners(ctx, spec, request, plan); err != nil {
		return err
	}
	if err := m.waitForPanelHealth(ctx, binary, db, envValue(configured, "PANEL_HTTP_LISTEN")); err != nil {
		return fmt.Errorf("private panel health check failed: %w", err)
	}
	if err := m.Runner.Run(ctx, "systemctl", []string{"restart", "cottenrouter"}, "/", false); err != nil {
		return err
	}
	if err := m.requireActive(ctx, "cottenrouter", "CottenRouter"); err != nil {
		return err
	}
	if err := m.waitForRouterListeners(ctx, request.RouterConfig); err != nil {
		return err
	}
	for _, target := range publicPorts(request, plan) {
		if err := openFirewall(ctx, m.Runner, target); err != nil {
			return err
		}
	}
	sum := sha256.Sum256(archive)
	record, _ := json.MarshalIndent(map[string]string{"repository": project.Repository, "commit_sha": project.CommitSHA, "release_tag": project.ReleaseTag, "release_channel": channel, "archive_sha256": hex.EncodeToString(sum[:])}, "", "  ")
	if err := atomicWrite(filepath.Join(spec.WorkDir, "upstream.json"), append(record, '\n'), 0600); err != nil {
		return err
	}
	progress("WhiteDNS Panel " + project.ReleaseTag + " (" + channel + ") is ready on " + envValue(configured, "PANEL_HTTP_LISTEN") + "; use SSH forwarding. Accounts and keys are preserved.")
	return nil
}

func (m Manager) waitForPanelHealth(ctx context.Context, binary, db, listen string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var last error
	for {
		if _, last = m.Runner.Output(ctx, binary, "health", "--db", db, "--listen", listen); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), last)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
