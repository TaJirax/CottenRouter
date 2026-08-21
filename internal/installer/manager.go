package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TaJirax/CottenRouter/internal/catalog"
	"github.com/TaJirax/CottenRouter/internal/config"
)

type Progress func(string)

type Runner interface {
	Run(context.Context, string, []string, string, bool) error
	Output(context.Context, string, ...string) ([]byte, error)
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, name string, args []string, dir string, interactive bool) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if interactive {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	}
	return cmd.Run()
}
func (OSRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type Manager struct {
	Client *http.Client
	Runner Runner
}

func DefaultManager() Manager {
	return Manager{Client: &http.Client{Timeout: 30 * time.Second}, Runner: OSRunner{}}
}

func (m Manager) Scan(ctx context.Context) ([]Listener, error) {
	if m.Runner == nil {
		m.Runner = OSRunner{}
	}
	data, err := m.Runner.Output(ctx, "ss", "-H", "-lntup")
	if err != nil {
		return nil, fmt.Errorf("scan listeners with ss: %w", err)
	}
	return ParseSS(string(data)), nil
}

func (m Manager) Install(ctx context.Context, request Request, progress Progress) (PortPlan, error) {
	if progress == nil {
		progress = func(string) {}
	}
	if runtime.GOOS != "linux" {
		return PortPlan{}, fmt.Errorf("server installation is supported on Linux only")
	}
	if m.Runner == nil {
		m.Runner = OSRunner{}
	}
	if m.Client == nil {
		m.Client = &http.Client{Timeout: 30 * time.Second}
	}
	uid, err := m.Runner.Output(ctx, "id", "-u")
	if err != nil || strings.TrimSpace(string(uid)) != "0" {
		return PortPlan{}, fmt.Errorf("installer must run as root")
	}
	if !supportedNativePackageManager() {
		return PortPlan{}, fmt.Errorf("project installation supports systemd servers with apt, dnf, or yum; current upstream installers do not support this distribution")
	}
	spec, err := request.Validate()
	if err != nil {
		return PortPlan{}, err
	}

	progress("Scanning occupied ports and protecting existing panels")
	listeners, err := m.Scan(ctx)
	if err != nil {
		return PortPlan{}, err
	}
	for _, listener := range listeners {
		// The backend being installed is allowed to hold port 53 here: its own
		// installer takes that port, and an earlier failed run can leave it
		// there. It is moved onto a loopback port and restarted below. Any
		// other owner is still refused rather than stopped.
		if listener.Port == 53 && !isCottenRouterListener(listener) && !listenerOwnedBySpec(listener, spec) {
			return PortPlan{}, fmt.Errorf("port 53 is owned by %s; refusing to stop or replace it", listener.Process)
		}
	}
	listeners = m.protectPanels(ctx, listeners)
	var externalListeners []Listener
	managedActive := false
	if output, err := m.Runner.Output(ctx, "systemctl", "is-active", spec.Service); err == nil && strings.TrimSpace(string(output)) == "active" {
		managedActive = true
	}
	for _, listener := range listeners {
		if !strings.Contains(strings.ToLower(listener.Process), "cottenrouter") && !(managedActive && listenerOwnedBySpec(listener, spec)) {
			externalListeners = append(externalListeners, listener)
		}
	}
	// A backend that is stopped right now still owns its port: the router
	// config routes to it. Without this, installing a second protocol could
	// hand out a port the first one takes back on its next start.
	externalListeners = append(externalListeners, reservedRouterPorts(request.RouterConfig, spec)...)
	plan := PlanPorts(externalListeners, request)
	if plan.DNSPort == 0 || (request.EnableDoT && (plan.DoTPublicPort == 0 || plan.DoTPrivatePort == 0)) || (request.EnableDoH && (plan.DoHPublicPort == 0 || plan.DoHPrivatePort == 0)) {
		return plan, fmt.Errorf("no safe private port is available")
	}
	request.PrivatePort = plan.DNSPort

	progress("Resolving and verifying the current upstream installer")
	project, err := catalog.DefaultResolver().LatestProject(ctx, request.ProjectID)
	if err != nil {
		return plan, err
	}
	for _, managedPath := range []string{spec.WorkDir, spec.ConfigPath, request.RouterConfig} {
		if err := rejectSymlinkComponents(managedPath); err != nil {
			return plan, err
		}
	}
	previous, previousErr := os.ReadFile(spec.ConfigPath)
	if previousErr != nil && !errors.Is(previousErr, os.ErrNotExist) {
		return plan, fmt.Errorf("snapshot backend config: %w", previousErr)
	}
	routerPrevious, routerPreviousErr := os.ReadFile(request.RouterConfig)
	if routerPreviousErr != nil {
		return plan, fmt.Errorf("snapshot CottenRouter config: %w", routerPreviousErr)
	}
	var previousSlipGateTLSPlan SlipGateTLSPlan
	if spec.Kind == ConfigSlipGate && previousErr == nil {
		previousSlipGateTLSPlan, err = BuildSlipGateTLSPlan(previous, SlipGateTLSOptions{Listeners: listeners, ReadFile: os.ReadFile})
		if err != nil {
			return plan, fmt.Errorf("snapshot existing SlipGate TLS integration: %w", err)
		}
	}
	var previousManagedServiceStates []managedServiceState
	if spec.Kind == ConfigSlipGate {
		previousManagedServiceStates, err = snapshotSlipGateManagedServiceStates(ctx, m.Runner)
		if err != nil {
			return plan, err
		}
	} else {
		previousManagedServiceStates = []managedServiceState{snapshotManagedServiceState(ctx, m.Runner, spec.Service)}
	}
	completed := false
	routerStopped := false
	defer func() {
		if completed {
			return
		}
		if previousErr == nil {
			_ = atomicWrite(spec.ConfigPath, previous, 0600)
		} else {
			_ = os.Remove(spec.ConfigPath)
		}
		_ = atomicWrite(request.RouterConfig, routerPrevious, 0640)
		if routerStopped {
			restoreManagedServicesExact(previousManagedServiceStates, m.Runner)
			_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", "cottenrouter"}, "/", false)
		}
	}()
	var slipGateTLSPlan SlipGateTLSPlan
	var slipGateTLSTransaction *slipGateTLSPatchTransaction
	defer func() {
		if !completed && slipGateTLSTransaction != nil {
			_ = slipGateTLSTransaction.Rollback()
		}
		if !completed && spec.Kind == ConfigSlipGate {
			stopNewSlipGateManagedServices(previousManagedServiceStates, m.Runner)
			_ = restoreSlipGateTLSArtifacts(previousSlipGateTLSPlan)
			_ = m.Runner.Run(context.Background(), "systemctl", []string{"daemon-reload"}, "/", false)
		}
	}()
	if err := os.MkdirAll(spec.WorkDir, 0750); err != nil {
		return plan, fmt.Errorf("create backend directory: %w", err)
	}
	if spec.TemplatePath != "" {
		if _, err := os.Stat(spec.ConfigPath); errors.Is(err, os.ErrNotExist) {
			templateURL := "https://raw.githubusercontent.com/" + project.RepoFullName + "/" + project.CommitSHA + "/" + spec.TemplatePath
			data, err := m.download(ctx, templateURL)
			if err != nil {
				return plan, fmt.Errorf("download current config template: %w", err)
			}
			data, err = configure(spec, request, plan, data)
			if err != nil {
				return plan, err
			}
			if err := atomicWrite(spec.ConfigPath, data, 0600); err != nil {
				return plan, err
			}
		}
	}
	installerData, err := m.download(ctx, project.InstallerURL)
	if err != nil {
		return plan, err
	}
	if err := recordUpstreamInstaller(project, installerData, false); err != nil {
		return plan, err
	}
	// Once the upstream installer has actually executed it may have left units,
	// binaries, and data behind even on failure. Keep the pending record so purge
	// can still run the exact pinned uninstaller; only discard it when nothing
	// was ever run.
	upstreamExecuted := false
	defer func() {
		if !upstreamExecuted {
			discardPendingInstallRecord(project.ID)
		}
	}()
	temp, err := os.CreateTemp("", "cottenrouter-upstream-*.sh")
	if err != nil {
		return plan, err
	}
	installerPath := temp.Name()
	defer os.Remove(installerPath)
	if _, err := temp.Write(installerData); err != nil {
		temp.Close()
		return plan, err
	}
	if err := temp.Chmod(0700); err != nil {
		temp.Close()
		return plan, err
	}
	if err := temp.Close(); err != nil {
		return plan, err
	}

	progress("Stopping CottenRouter briefly; other private backends and panels remain untouched")
	if err := m.Runner.Run(ctx, "systemctl", []string{"stop", "cottenrouter"}, "/", false); err != nil {
		return plan, fmt.Errorf("stop CottenRouter before native installer: %w", err)
	}
	routerStopped = true

	progress("Running the verified upstream installer")
	upstreamExecuted = true
	var installErr error
	// Native installers keep their complete setup surface. Command shims guard
	// common service and firewall commands; post-install listener checks still
	// fail closed if an upstream release violates CottenRouter's ownership plan.
	installErr = m.runProtectedCommand(ctx, spec, "bash", []string{installerPath}, spec.WorkDir)
	if installErr != nil {
		return plan, fmt.Errorf("upstream %s installer failed: %w", spec.Name, installErr)
	}
	if spec.Kind == ConfigSlipGate {
		progress("Opening SlipGate's native setup so every selected transport setting remains available")
		if err := m.runProtectedSlipGate(ctx, spec.WorkDir); err != nil {
			return plan, fmt.Errorf("SlipGate setup: %w", err)
		}
	}
	if spec.Kind == ConfigSlipGate {
		data, err := os.ReadFile(spec.ConfigPath)
		if err != nil {
			return plan, fmt.Errorf("read configured SlipGate TLS transports: %w", err)
		}
		currentListeners, err := m.Scan(ctx)
		if err != nil {
			return plan, err
		}
		currentListeners = m.protectPanels(ctx, currentListeners)
		slipGateTLSPlan, err = BuildSlipGateTLSPlan(data, SlipGateTLSOptions{Listeners: currentListeners, ReadFile: os.ReadFile})
		if err != nil {
			return plan, fmt.Errorf("plan SlipGate TLS integration: %w", err)
		}
		if err := validateSlipGateTLSPublicPorts(slipGateTLSPlan, currentListeners, request.RouterConfig); err != nil {
			return plan, err
		}
		slipGateTLSTransaction, err = applySlipGateTLSPatches(slipGateTLSPlan)
		if err != nil {
			return plan, err
		}
	}
	if spec.Kind != ConfigSlipGate {
		data, err := os.ReadFile(spec.ConfigPath)
		if err != nil {
			return plan, fmt.Errorf("read installed config: %w", err)
		}
		configured, err := configure(spec, request, plan, data)
		if err != nil {
			return plan, err
		}
		if err := atomicWrite(spec.ConfigPath, configured, 0600); err != nil {
			return plan, fmt.Errorf("write private backend config: %w", err)
		}
	}
	progress("Updating CottenRouter routes atomically")
	if err := updateRouterConfigWithSlipGateTLS(request.RouterConfig, spec, request, plan, slipGateTLSPlan, previousSlipGateTLSPlan); err != nil {
		return plan, err
	}
	if err := m.installContainment(ctx, spec); err != nil {
		return plan, fmt.Errorf("install backend resource safeguards: %w", err)
	}
	if spec.Kind == ConfigSlipGate {
		if err := m.enableSlipGateManagedServices(ctx, spec.ConfigPath); err != nil {
			return plan, err
		}
		if err := m.Runner.Run(ctx, "systemctl", []string{"disable", "--now", "slipgate-dnsrouter"}, "/", false); err != nil {
			return plan, fmt.Errorf("disable native SlipGate DNS router: %w", err)
		}
		if err := m.Runner.Run(ctx, "usermod", []string{"-aG", "slipgate", "cottenrouter"}, "/", false); err != nil {
			return plan, fmt.Errorf("grant CottenRouter access to SlipGate keys: %w", err)
		}
		if err := m.restartSlipGateTLSBackends(ctx, slipGateTLSPlan); err != nil {
			return plan, err
		}
	} else {
		// The upstream installers all insist on owning port 53 during their own
		// run, so the backend is left listening there when they finish. Its
		// config has just been rewritten to a loopback port, and `enable --now`
		// does nothing to an already-running unit: it has to be restarted, or it
		// keeps port 53 and the router can never take it back.
		if err := m.Runner.Run(ctx, "systemctl", []string{"enable", spec.Service}, "/", false); err != nil {
			return plan, fmt.Errorf("enable %s: %w", spec.Service, err)
		}
		if err := m.Runner.Run(ctx, "systemctl", []string{"restart", spec.Service}, "/", false); err != nil {
			return plan, fmt.Errorf("restart %s onto its private port: %w", spec.Service, err)
		}
	}
	progress("Restarting CottenRouter and verifying services")
	for _, target := range uniqueFirewallPorts(publicPorts(request, plan), slipGateTLSPublicPorts(slipGateTLSPlan)) {
		if err := openFirewall(ctx, m.Runner, target); err != nil {
			return plan, fmt.Errorf("open public %d/%s: %w", target.port, target.protocol, err)
		}
	}
	progress("Waiting for the backend to release port 53 back to CottenRouter")
	if err := m.waitForRouterPortsReleased(ctx, request.RouterConfig); err != nil {
		return plan, err
	}
	if err := m.Runner.Run(ctx, "systemctl", []string{"restart", "cottenrouter"}, "/", false); err != nil {
		return plan, err
	}
	if err := m.Runner.Run(ctx, "systemctl", []string{"is-active", "--quiet", "cottenrouter"}, "/", false); err != nil {
		return plan, fmt.Errorf("CottenRouter did not become active: %w", err)
	}
	if err := m.waitForRouterListeners(ctx, request.RouterConfig); err != nil {
		return plan, err
	}
	if spec.Kind == ConfigSlipGate {
		if err := m.waitForSlipGateTLSListeners(ctx, slipGateTLSPlan); err != nil {
			return plan, err
		}
	} else {
		if err := m.Runner.Run(ctx, "systemctl", []string{"is-active", "--quiet", spec.Service}, "/", false); err != nil {
			return plan, fmt.Errorf("%s did not become active: %w", spec.Name, err)
		}
		if err := m.waitForPrivateListeners(ctx, spec, request, plan); err != nil {
			return plan, err
		}
	}
	if err := recordUpstreamInstaller(project, installerData, true); err != nil {
		return plan, err
	}
	if slipGateTLSTransaction != nil {
		slipGateTLSTransaction.Commit()
	}
	completed = true
	return plan, nil
}

func supportedNativePackageManager() bool {
	for _, command := range []string{"apt-get", "dnf", "yum"} {
		if _, err := exec.LookPath(command); err == nil {
			return true
		}
	}
	return false
}

type expectedPrivateListener struct {
	protocol string
	port     int
}

// routerEndpoint is one protocol/port pair the router is configured to own.
type routerEndpoint struct {
	protocol string
	port     int
}

func routerEndpoints(cfg config.Config) ([]routerEndpoint, error) {
	parse := func(protocol, address string) (routerEndpoint, error) {
		_, portText, err := net.SplitHostPort(address)
		if err != nil {
			return routerEndpoint{}, err
		}
		port, err := strconv.Atoi(portText)
		return routerEndpoint{protocol: protocol, port: port}, err
	}
	endpoints := make([]routerEndpoint, 0, 3+len(cfg.TLSListeners))
	udp, err := parse("udp", cfg.ListenUDP)
	if err != nil {
		return nil, err
	}
	endpoints = append(endpoints, udp)
	if cfg.ListenTCP != "" {
		tcp, err := parse("tcp", cfg.ListenTCP)
		if err != nil {
			return nil, err
		}
		endpoints = append(endpoints, tcp)
	}
	admin, err := parse("tcp", cfg.AdminListen)
	if err != nil {
		return nil, err
	}
	endpoints = append(endpoints, admin)
	for _, listener := range cfg.TLSListeners {
		tlsEndpoint, err := parse("tcp", listener.Listen)
		if err != nil {
			return nil, err
		}
		endpoints = append(endpoints, tlsEndpoint)
	}
	return endpoints, nil
}

func isCottenRouterListener(listener Listener) bool {
	return strings.Contains(strings.ToLower(listener.Process), "cottenrouter")
}

func listenerMatches(listener Listener, target routerEndpoint) bool {
	protocol := strings.TrimSuffix(strings.ToLower(listener.Protocol), "6")
	return listener.Port == target.port && protocol == target.protocol
}

// waitForRouterPortsReleased blocks until nothing except CottenRouter holds a
// port the router is configured to own. Every upstream backend installer
// takes port 53 for itself during its run; the backend is then reconfigured
// onto a loopback port and restarted, and the router only reclaims 53 once
// that hand-back has actually happened. Without this the restart fails with
// nothing but a systemd exit code to explain why.
func (m Manager) waitForRouterPortsReleased(ctx context.Context, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	expected, err := routerEndpoints(cfg)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		listeners, scanErr := m.Scan(ctx)
		if scanErr != nil {
			return scanErr
		}
		blocker := ""
		for _, target := range expected {
			for _, listener := range listeners {
				if listenerMatches(listener, target) && !isCottenRouterListener(listener) {
					blocker = fmt.Sprintf("%d/%s is still held by %s", target.port, target.protocol, listener.Process)
					break
				}
			}
			if blocker != "" {
				break
			}
		}
		if blocker == "" {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("CottenRouter cannot reclaim its ports: %s", blocker)
		}
		select {
		case <-ctx.Done():
			// Report what is holding the port even when the wait is cut short.
			return fmt.Errorf("CottenRouter cannot reclaim its ports: %s: %w", blocker, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (m Manager) waitForRouterListeners(ctx context.Context, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	expected, err := routerEndpoints(cfg)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		listeners, scanErr := m.Scan(ctx)
		if scanErr != nil {
			return scanErr
		}
		missing := 0
		for _, target := range expected {
			found := false
			for _, listener := range listeners {
				if listenerMatches(listener, target) && isCottenRouterListener(listener) {
					found = true
					break
				}
			}
			if !found {
				missing++
			}
		}
		if missing == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("CottenRouter is active but %d configured listener(s) are missing", missing)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (m Manager) waitForPrivateListeners(ctx context.Context, spec Spec, request Request, plan PortPlan) error {
	expected := []expectedPrivateListener{{protocol: "udp", port: plan.DNSPort}}
	if request.EnableTCP {
		expected = append(expected, expectedPrivateListener{protocol: "tcp", port: plan.DNSPort})
	}
	if request.EnableDoT {
		expected = append(expected, expectedPrivateListener{protocol: "tcp", port: plan.DoTPrivatePort})
	}
	if request.EnableDoH {
		expected = append(expected, expectedPrivateListener{protocol: "tcp", port: plan.DoHPrivatePort})
	}
	deadline := time.Now().Add(5 * time.Second)
	var lastMissing []expectedPrivateListener
	for {
		listeners, err := m.Scan(ctx)
		if err != nil {
			return err
		}
		lastMissing = missingPrivateListeners(listeners, spec, expected)
		if len(lastMissing) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s did not expose all required loopback listeners: %v", spec.Name, lastMissing)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func missingPrivateListeners(listeners []Listener, spec Spec, expected []expectedPrivateListener) []expectedPrivateListener {
	missing := make([]expectedPrivateListener, 0)
	for _, target := range expected {
		found := false
		for _, listener := range listeners {
			protocol := strings.TrimSuffix(strings.ToLower(listener.Protocol), "6")
			if listener.Port == target.port && protocol == target.protocol && listenerOwnedBySpec(listener, spec) && listenerIsLoopback(listener.Address) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, target)
		}
	}
	return missing
}

func listenerIsLoopback(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (m Manager) runProtectedSlipGate(ctx context.Context, workDir string) error {
	spec, _ := FindSpec("slipgate")
	return m.runProtectedCommand(ctx, spec, "slipgate", nil, workDir)
}

func (m Manager) runProtectedCommand(ctx context.Context, spec Spec, command string, args []string, workDir string) error {
	shimDir, err := os.MkdirTemp("", "cottenrouter-slipgate-guard-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(shimDir)
	realFuser, _ := exec.LookPath("fuser")
	if realFuser == "" {
		realFuser = "/bin/false"
	}
	protected := []string{"53/udp", "53/tcp", "443/tcp", "853/tcp"}
	if listeners, scanErr := m.Scan(ctx); scanErr == nil {
		for _, listener := range listeners {
			protocol := strings.TrimSuffix(strings.ToLower(listener.Protocol), "6")
			if protocol == "tcp" || protocol == "udp" {
				protected = append(protected, fmt.Sprintf("%d/%s", listener.Port, protocol))
			}
		}
	}
	shim := protectedFuserScript(realFuser, protected)
	if err := os.WriteFile(filepath.Join(shimDir, "fuser"), []byte(shim), 0755); err != nil {
		return err
	}
	for _, tool := range []string{"iptables", "ip6tables", "nft"} {
		realTool, _ := exec.LookPath(tool)
		if realTool == "" {
			realTool = "/bin/false"
		}
		if err := os.WriteFile(filepath.Join(shimDir, tool), []byte(protectedFirewallScript(realTool)), 0755); err != nil {
			return err
		}
	}
	for _, tool := range []string{"ufw", "firewall-cmd", "iptables-save", "iptables-restore", "ip6tables-save", "ip6tables-restore", "netfilter-persistent"} {
		realTool, _ := exec.LookPath(tool)
		if realTool == "" {
			realTool = "/bin/false"
		}
		if err := os.WriteFile(filepath.Join(shimDir, tool), []byte(protectedPersistentFirewallScript(tool, realTool)), 0755); err != nil {
			return err
		}
	}
	protectedServices := []string{"cottenrouter.service", "slipgate-iptables.service"}
	if output, outputErr := m.Runner.Output(ctx, "systemctl", "list-units", "--type=service", "--state=active", "--no-legend", "--plain"); outputErr == nil {
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			name := fields[0]
			// systemd-resolved may be restarted after its already-owned stub
			// listener drop-in changes. It is checked indirectly when the router
			// reclaims :53. Persistent firewall restore remains protected.
			if name == "systemd-resolved.service" {
				continue
			}
			managed := name == spec.Service+".service" || (spec.Kind == ConfigSlipGate && strings.HasPrefix(name, "slipgate-") && name != "slipgate-iptables.service")
			if safeServiceName(strings.TrimSuffix(name, ".service")) && !managed {
				protectedServices = append(protectedServices, name)
			}
		}
	}
	realSystemctl, _ := exec.LookPath("systemctl")
	if realSystemctl == "" {
		realSystemctl = "/bin/false"
	}
	if err := os.WriteFile(filepath.Join(shimDir, "systemctl"), []byte(protectedSystemctlScript(realSystemctl, protectedServices)), 0755); err != nil {
		return err
	}
	realService, _ := exec.LookPath("service")
	if realService != "" {
		if err := os.WriteFile(filepath.Join(shimDir, "service"), []byte(protectedServiceScript(realService, protectedServices)), 0755); err != nil {
			return err
		}
	}
	protectedPath := shimDir + string(os.PathListSeparator) + os.Getenv("PATH")
	commandArgs := append([]string{"PATH=" + protectedPath, command}, args...)
	return m.Runner.Run(ctx, "env", commandArgs, workDir, true)
}

func protectedFirewallScript(realTool string) string {
	return "#!/bin/sh\ncase \"$1\" in add|delete|insert|flush) echo 'CottenRouter: native firewall mutation deferred to the router installer' >&2; exit 0 ;; esac\ncase \" $* \" in *\" -A \"*|*\" -I \"*|*\" -D \"*|*\" -R \"*|*\" -F \"*|*\" -X \"*|*\" -N \"*|*\" -P \"*) echo 'CottenRouter: native firewall mutation deferred to the router installer' >&2; exit 0 ;; esac\nexec " + strconv.Quote(realTool) + " \"$@\"\n"
}

func protectedPersistentFirewallScript(tool, realTool string) string {
	quoted := strconv.Quote(realTool)
	switch tool {
	case "ufw":
		return "#!/bin/sh\ncase \"${1:-}\" in status|show|version|--version|help|--help) exec " + quoted + " \"$@\" ;; esac\necho 'CottenRouter: native firewall mutation deferred to the router installer' >&2\nexit 0\n"
	case "firewall-cmd":
		return "#!/bin/sh\ncase \" $* \" in *\" --add-\"*|*\" --remove-\"*|*\" --reload \"*|*\" --complete-reload \"*|*\" --runtime-to-permanent \"*|*\" --permanent \"*|*\" --set-\"*) echo 'CottenRouter: native firewall mutation deferred to the router installer' >&2; exit 0 ;; esac\nexec " + quoted + " \"$@\"\n"
	default:
		return "#!/bin/sh\necho 'CottenRouter: native persistent firewall mutation deferred to the router installer' >&2\nexit 0\n"
	}
}

func protectedSystemctlScript(realTool string, protected []string) string {
	return protectedServiceManagerScript(realTool, protected, true)
}

func protectedServiceScript(realTool string, protected []string) string {
	return protectedServiceManagerScript(realTool, protected, false)
}

func protectedServiceManagerScript(realTool string, protected []string, systemctl bool) string {
	seen := map[string]bool{}
	var cases []string
	for _, service := range protected {
		service = strings.TrimSuffix(service, ".service")
		if safeServiceName(service) && !seen[service] {
			seen[service] = true
			cases = append(cases, service+"|"+service+".service")
		}
	}
	sort.Strings(cases)
	actionScan := "action=$2; target=$1"
	if systemctl {
		actionScan = "action=''; target=''\nfor arg in \"$@\"; do case \"$arg\" in stop|disable|restart|try-restart|reload|kill|mask) [ -n \"$action\" ] || action=$arg ;; -*) ;; *) [ -z \"$action\" ] || [ -n \"$target\" ] || target=$arg ;; esac; done"
	}
	return "#!/bin/sh\n" + actionScan + "\ncase \"$action\" in stop|disable|restart|try-restart|reload|kill|mask) for candidate in \"$@\"; do case \"$candidate\" in " + strings.Join(cases, "|") + ") echo \"CottenRouter: preserved active unrelated service $candidate\" >&2; exit 0 ;; esac; done ;; esac\nexec " + strconv.Quote(realTool) + " \"$@\"\n"
}

func protectedFuserScript(realFuser string, protected []string) string {
	seen := map[string]bool{}
	patterns := make([]string, 0, len(protected))
	for _, value := range protected {
		if regexp.MustCompile(`^[0-9]+/(tcp|udp)$`).MatchString(value) && !seen[value] {
			seen[value] = true
			patterns = append(patterns, `*" `+value+` "*`)
		}
	}
	sort.Strings(patterns)
	return "#!/bin/sh\ncase \" $* \" in " + strings.Join(patterns, "|") + ") exit 1 ;; esac\nexec " + strconv.Quote(realFuser) + " \"$@\"\n"
}

// reservedRouterPorts reports the loopback backend ports and public TLS
// ports the router already routes to, excluding the backend being installed.
// They are treated as occupied even when nothing is listening on them.
func reservedRouterPorts(routerConfigPath string, spec Spec) []Listener {
	cfg, err := config.Load(routerConfigPath)
	if err != nil {
		return nil
	}
	var reserved []Listener
	reserve := func(owner, address string) {
		if address == "" || address == "disabled" {
			return
		}
		_, portText, err := net.SplitHostPort(address)
		if err != nil {
			return
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			return
		}
		reserved = append(reserved, Listener{Port: port, Address: address, Process: "reserved by route " + owner + " in the CottenRouter config"})
	}
	for _, route := range cfg.Routes {
		if route.Name == spec.ID || (spec.Kind == ConfigSlipGate && strings.HasPrefix(route.Name, "slipgate:")) {
			continue
		}
		reserve(route.Name, route.Backend)
		reserve(route.Name, route.TCPBackend)
	}
	for _, listener := range cfg.TLSListeners {
		reserve(listener.Name, listener.Listen)
		for _, route := range listener.Routes {
			if strings.HasPrefix(route.Name, spec.ID+"-") {
				continue
			}
			reserve(route.Name, route.Backend)
		}
	}
	return reserved
}

func listenerOwnedBySpec(listener Listener, spec Spec) bool {
	process := strings.ToLower(listener.Process)
	for _, candidate := range []string{spec.ID, spec.Service, strings.ToLower(spec.Name)} {
		if candidate != "" && strings.Contains(process, strings.ToLower(candidate)) {
			return true
		}
	}
	return false
}

func (m Manager) protectPanels(ctx context.Context, listeners []Listener) []Listener {
	for _, panelService := range []string{"x-ui", "3x-ui", "hiddify-panel", "marzban", "xray"} {
		if output, err := m.Runner.Output(ctx, "systemctl", "is-active", panelService); err == nil && strings.TrimSpace(string(output)) == "active" {
			already := false
			for _, listener := range listeners {
				if listener.Port == 443 && strings.Contains(strings.ToLower(listener.Process), strings.ToLower(panelService)) {
					already = true
				}
			}
			if !already {
				listeners = append(listeners, Listener{Port: 443, Protocol: "tcp", Process: panelService + " (protected panel)", Address: "0.0.0.0:443"})
			}
		}
	}
	return listeners
}

// installContainment gives managed backends boot ordering and one bounded
// resource slice. Wants (rather than Requires/PartOf) deliberately lets
// loopback backends and their sessions survive a router restart.
func (m Manager) installContainment(ctx context.Context, spec Spec) error {
	const slice = `[Unit]
Description=CottenRouter managed backend resource boundary

[Slice]
MemoryAccounting=yes
MemoryHigh=1G
MemoryMax=1536M
CPUAccounting=yes
CPUQuota=300%
TasksMax=4096
`
	if err := atomicWrite("/etc/systemd/system/cottenrouter-backends.slice", []byte(slice), 0644); err != nil {
		return err
	}
	services := []string{spec.Service}
	if spec.Kind == ConfigSlipGate {
		if err := forceSlipGateLoopback(spec.ConfigPath); err != nil {
			return err
		}
		services = nil
		output, _ := m.Runner.Output(ctx, "systemctl", "list-unit-files", "--no-legend", "slipgate-*.service")
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			name := strings.TrimSuffix(fields[0], ".service")
			if safeServiceName(name) && name != "slipgate-dnsrouter" {
				services = append(services, name)
			}
		}
		// This condition is deliberately false unless an administrator creates
		// the marker. It survives SlipGate upgrades and stops it reclaiming :53.
		blocker := "[Unit]\nConditionPathExists=/run/cottenrouter/allow-native-slipgate-dnsrouter\n"
		if err := writeDropIn("slipgate-dnsrouter", blocker); err != nil {
			return err
		}
	}
	const managed = `[Unit]
Wants=cottenrouter.service
After=cottenrouter.service

[Service]
Slice=cottenrouter-backends.slice
MemoryAccounting=yes
CPUAccounting=yes
TasksMax=2048
`
	for _, service := range services {
		if !safeServiceName(service) {
			return fmt.Errorf("unsafe systemd service name %q", service)
		}
		if err := writeDropIn(service, managed); err != nil {
			return err
		}
	}
	if err := m.Runner.Run(ctx, "systemctl", []string{"daemon-reload"}, "/", false); err != nil {
		return err
	}
	if spec.Kind == ConfigSlipGate {
		// Unit/Caddy listener rewrites only affect running tunnel processes
		// after a restart. try-restart preserves tunnels that were disabled.
		for _, service := range services {
			if err := m.Runner.Run(ctx, "systemctl", []string{"try-restart", service}, "/", false); err != nil {
				return fmt.Errorf("apply private listener to %s: %w", service, err)
			}
		}
	}
	return nil
}

func forceSlipGateLoopback(configPath string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var document struct {
		Tunnels []struct {
			Tag, Transport string
			Port           int  `json:"port"`
			Enabled        bool `json:"enabled"`
		} `json:"tunnels"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return err
	}
	for _, tunnel := range document.Tunnels {
		if !tunnel.Enabled || tunnel.Port == 0 || !slipGateDNSTransport(tunnel.Transport) {
			continue
		}
		if !safeServiceName("slipgate-" + tunnel.Tag) {
			return fmt.Errorf("unsafe SlipGate tag %q", tunnel.Tag)
		}
		path := filepath.Join("/etc/systemd/system", "slipgate-"+tunnel.Tag+".service")
		unit, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		patched, changed := privateSlipGateUnit(unit, tunnel.Port)
		if bytes.Equal(unit, patched) && tunnel.Transport != "slipstream" {
			return fmt.Errorf("SlipGate unit %s does not expose an expected listen address", path)
		}
		if changed {
			if err := atomicWrite(path, patched, 0644); err != nil {
				return err
			}
		}
	}
	return nil
}

func privateSlipGateUnit(unit []byte, port int) ([]byte, bool) {
	public := []byte(fmt.Sprintf("0.0.0.0:%d", port))
	private := []byte(fmt.Sprintf("127.0.0.1:%d", port))
	patched := bytes.ReplaceAll(unit, public, private)
	return patched, !bytes.Equal(unit, patched)
}

func slipGateDNSTransport(value string) bool {
	switch value {
	case "dnstt", "slipstream", "vaydns":
		return true
	}
	return false
}

func safeServiceName(name string) bool {
	return regexp.MustCompile(`^[A-Za-z0-9_.@-]+$`).MatchString(name)
}

func writeDropIn(service, contents string) error {
	path := filepath.Join("/etc/systemd/system", service+".service.d", "cottenrouter.conf")
	return atomicWrite(path, []byte(contents), 0644)
}

func (m Manager) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 8<<20 {
		return nil, fmt.Errorf("download %s exceeds 8 MiB safety limit", url)
	}
	return data, nil
}

func configure(spec Spec, request Request, plan PortPlan, data []byte) ([]byte, error) {
	switch spec.Kind {
	case ConfigTOML:
		initialCottenProfile := spec.ID == "cottendns" && tomlValue(data, "UDP_HOST") != "127.0.0.1"
		domains := appendCSV([]string{request.Domain}, request.ExtraDomains, request.DoTDomain, request.DoHDomain)
		quoted := make([]string, 0, len(domains))
		for _, domain := range domains {
			quoted = append(quoted, strconv.Quote(domain))
		}
		data = setTOML(data, "DOMAIN", "["+strings.Join(quoted, ", ")+"]")
		data = setTOML(data, "UDP_HOST", "\"127.0.0.1\"")
		data = setTOML(data, "UDP_PORT", strconv.Itoa(request.PrivatePort))
		if spec.ID == "cottendns" {
			// Preserve native/advanced tuning. Missing keys get conservative
			// defaults, while the router and systemd slice provide outer bounds.
			for key, value := range map[string]string{
				"TCP_MAX_CONNS": "512", "TCP_MAX_CONNS_PER_IP": "32",
				"TCP_MAX_QUERIES_PER_CONN": "1024", "ENCRYPTED_MAX_CONNS": "192",
				"DOH_MAX_INFLIGHT": "128", "DOH_MAX_INFLIGHT_BYTES": "33554432",
				"DOH_REQUESTS_PER_SECOND_PER_IP": "512", "DOH_REQUEST_BURST_PER_IP": "1024",
				"MAX_CONCURRENT_REQUESTS": "4096", "MAX_INGRESS_QUEUE_BYTES": "33554432",
				"MAX_PACKET_SIZE": "16384", "DEFERRED_SESSION_QUEUE_LIMIT": "1024",
				"MAX_STREAMS_PER_SESSION": "512", "MAX_ACTIVE_SESSIONS": "1024",
				"DNS_CACHE_MAX_RECORDS": "25000", "MAX_DNS_RESPONSE_BYTES": "16384",
			} {
				data = seedTOML(data, key, value)
			}
			// A newly adopted/public CottenDNS is moved to the requested 16 KiB
			// ceiling once. Later common edits never overwrite advanced tuning.
			if initialCottenProfile {
				data = setTOML(data, "MAX_PACKET_SIZE", "16384")
				data = setTOML(data, "MAX_DNS_RESPONSE_BYTES", "16384")
			}
			data = setTOML(data, "TCP_LISTENER_ENABLED", strconv.FormatBool(request.EnableTCP))
			data = setTOML(data, "TCP_IPV6_HOST", "\"::1\"")
			data = setTOML(data, "DOT_LISTENER_ENABLED", strconv.FormatBool(request.EnableDoT))
			data = setTOML(data, "DOT_LISTEN_HOST", "\"127.0.0.1\"")
			data = setTOML(data, "DOT_LISTEN_PORT", strconv.Itoa(plan.DoTPrivatePort))
			data = setTOML(data, "DOH_LISTENER_ENABLED", strconv.FormatBool(request.EnableDoH))
			data = setTOML(data, "DOH_LISTEN_HOST", "\"127.0.0.1\"")
			if plan.DoHMode == "behind-panel" {
				data = setTOML(data, "DOH_COEXIST_MODE", "\"behind\"")
				data = setTOML(data, "DOH_TLS_ENABLED", "false")
				data = setTOML(data, "DOH_BEHIND_PORT", strconv.Itoa(plan.DoHPrivatePort))
			} else {
				data = setTOML(data, "DOH_COEXIST_MODE", "\"front\"")
				data = setTOML(data, "DOH_TLS_ENABLED", "true")
				data = setTOML(data, "DOH_LISTEN_PORT", strconv.Itoa(plan.DoHPrivatePort))
				if request.EnableDoH {
					var tlsErr error
					data, tlsErr = configureCottenRouterFrontDoHTLS(data, plan)
					if tlsErr != nil {
						return nil, tlsErr
					}
				}
			}
		}
	case ConfigEnv:
		data = setEnv(data, "THEFEED_DOMAIN", request.Domain)
		data = setEnv(data, "THEFEED_EXTRA_DOMAINS", request.ExtraDomains)
		data = setEnv(data, "THEFEED_CHAT_DOMAINS", request.ChatDomains)
		data = setEnv(data, "THEFEED_LISTEN", fmt.Sprintf("127.0.0.1:%d", request.PrivatePort))
		// thefeed natively caps live sessions and messages; also bound the
		// persistent account set so hostile registrations cannot grow forever.
		data = seedEnv(data, "THEFEED_CHAT_MAX_ACCOUNTS", "50000")
	}
	return data, nil
}

func configureCottenRouterFrontDoHTLS(data []byte, plan PortPlan) ([]byte, error) {
	if plan.DoHPublicPort == 443 && regexp.MustCompile(`(?m)^\s*ACME_EXTERNAL_PORT\s*=`).Match(data) {
		data = setTOML(data, "ACME_EXTERNAL_PORT", "443")
	}
	if err := validateCottenRouterFrontDoHTLS(data, plan); err != nil {
		return nil, err
	}
	return data, nil
}

func validateCottenRouterFrontDoHTLS(data []byte, plan PortPlan) error {
	certFile, keyFile := tomlValue(data, "TLS_CERT_FILE"), tomlValue(data, "TLS_KEY_FILE")
	if (certFile == "") != (keyFile == "") {
		return fmt.Errorf("CottenDNS router-front DoH requires both TLS_CERT_FILE and TLS_KEY_FILE")
	}
	if certFile != "" {
		return nil
	}
	if plan.DoHPublicPort == 443 && tomlValue(data, "ACME_EXTERNAL_PORT") == "443" {
		return nil
	}
	if plan.DoHPublicPort != 443 {
		return fmt.Errorf("CottenDNS DoH public port %d cannot use ACME; configure TLS_CERT_FILE/TLS_KEY_FILE in Advanced before enabling router-front DoH", plan.DoHPublicPort)
	}
	return fmt.Errorf("current CottenDNS upstream only enables ACME when its private listener owns :443; CottenRouter uses private port %d, so configure TLS_CERT_FILE/TLS_KEY_FILE in Advanced before enabling router-front DoH", plan.DoHPrivatePort)
}

func setTOML(data []byte, key, value string) []byte {
	return setLine(data, `(?m)^\s*`+regexp.QuoteMeta(key)+`\s*=.*$`, key+" = "+value)
}
func seedTOML(data []byte, key, value string) []byte {
	if regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `\s*=`).Match(data) {
		return data
	}
	return setTOML(data, key, value)
}
func setEnv(data []byte, key, value string) []byte {
	return setLine(data, `(?m)^`+regexp.QuoteMeta(key)+`=.*$`, key+"="+value)
}
func seedEnv(data []byte, key, value string) []byte {
	if regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `=`).Match(data) {
		return data
	}
	return setEnv(data, key, value)
}
func setLine(data []byte, pattern, line string) []byte {
	re := regexp.MustCompile(pattern)
	if re.Match(data) {
		return re.ReplaceAll(data, []byte(line))
	}
	return append(bytes.TrimRight(data, "\r\n"), []byte("\n"+line+"\n")...)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := rejectSymlinkComponents(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	if err := rejectSymlinkComponents(filepath.Dir(path)); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".cottenrouter-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if err := preserveFileOwnership(name, path); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func updateRouterConfig(path string, spec Spec, request Request, plan PortPlan) error {
	return updateRouterConfigInternal(path, spec, request, plan, nil, nil)
}

func updateRouterConfigWithSlipGateTLS(path string, spec Spec, request Request, plan PortPlan, current, previous SlipGateTLSPlan) error {
	return updateRouterConfigInternal(path, spec, request, plan, &current, &previous)
}

func updateRouterConfigInternal(path string, spec Spec, request Request, plan PortPlan, currentSlipGateTLS, previousSlipGateTLS *SlipGateTLSPlan) error {
	var cfg config.Config
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("decode router config: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Seed the defaults on a fresh config, but never overwrite settings an
	// operator already chose: installing a second backend used to move a
	// custom admin port back to 9088 and force DNS-over-TCP back on.
	if cfg.ListenUDP == "" {
		cfg.ListenUDP = "0.0.0.0:53"
	}
	if cfg.AdminListen == "" {
		cfg.AdminListen = "127.0.0.1:9088"
	}
	if cfg.ListenTCP == "" && (request.EnableTCP || spec.Kind == ConfigSlipGate) {
		// A TCP backend is unreachable without the router listening on TCP.
		cfg.ListenTCP = "0.0.0.0:53"
	}
	if cfg.MaxPacketSize == 0 {
		cfg.MaxPacketSize = 16 * 1024
	}
	cfg.Routes = removeRoute(cfg.Routes, "bootstrap-placeholder")
	if spec.Kind == ConfigSlipGate {
		routes, err := config.LoadSlipGateRoutes(spec.ConfigPath)
		if err != nil {
			return err
		}
		for _, route := range routes {
			cfg.Routes = upsertRoute(cfg.Routes, route)
		}
		if currentSlipGateTLS != nil {
			previous := SlipGateTLSPlan{}
			if previousSlipGateTLS != nil {
				previous = *previousSlipGateTLS
			}
			cfg.TLSListeners, err = mergeSlipGateTLSListeners(cfg.TLSListeners, *currentSlipGateTLS, previous)
			if err != nil {
				return fmt.Errorf("merge SlipGate TLS routes: %w", err)
			}
		}
	} else {
		domains := appendCSV([]string{request.Domain}, request.ExtraDomains)
		if spec.Kind == ConfigEnv {
			domains = appendCSV(domains, request.ChatDomains)
		}
		tcpBackend := "disabled"
		if request.EnableTCP {
			tcpBackend = fmt.Sprintf("127.0.0.1:%d", request.PrivatePort)
		}
		cfg.Routes = upsertRoute(cfg.Routes, config.Route{Name: spec.ID, Domains: domains, Backend: fmt.Sprintf("127.0.0.1:%d", request.PrivatePort), TCPBackend: tcpBackend})
	}
	// These routes belong to the backend that supports the transport. Without
	// the guard, installing any other protocol deleted the encrypted routes of
	// a backend it has nothing to do with. Removal is already spec-scoped.
	if spec.SupportsDoT {
		if request.EnableDoT {
			cfg.TLSListeners = upsertTLS(cfg.TLSListeners, "dot", plan.DoTPublicPort, "cottendns-dot", request.DoTDomain, plan.DoTPrivatePort)
		} else {
			cfg.TLSListeners = removeTLSRoute(cfg.TLSListeners, "cottendns-dot")
		}
	}
	if spec.SupportsDoH {
		if request.EnableDoH && plan.DoHMode == "router-front" {
			cfg.TLSListeners = upsertTLS(cfg.TLSListeners, "https", plan.DoHPublicPort, "cottendns-doh", request.DoHDomain, plan.DoHPrivatePort)
		} else {
			cfg.TLSListeners = removeTLSRoute(cfg.TLSListeners, "cottendns-doh")
		}
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("generated router config: %w", err)
	}
	encoded, _ := json.MarshalIndent(cfg, "", "  ")
	return atomicWrite(path, append(encoded, '\n'), 0640)
}

type firewallPort struct {
	port     int
	protocol string
}

func publicPorts(request Request, plan PortPlan) []firewallPort {
	ports := []firewallPort{{53, "udp"}, {53, "tcp"}}
	if request.EnableDoT {
		ports = append(ports, firewallPort{plan.DoTPublicPort, "tcp"})
	}
	if request.EnableDoH && plan.DoHMode == "router-front" {
		ports = append(ports, firewallPort{plan.DoHPublicPort, "tcp"})
	}
	return ports
}
func openFirewall(ctx context.Context, runner Runner, target firewallPort) error {
	if _, err := runner.Output(ctx, "ufw", "status"); err == nil {
		return runner.Run(ctx, "ufw", []string{"allow", fmt.Sprintf("%d/%s", target.port, target.protocol)}, "/", false)
	}
	if output, err := runner.Output(ctx, "systemctl", "is-active", "firewalld"); err == nil && strings.TrimSpace(string(output)) == "active" {
		if err := runner.Run(ctx, "firewall-cmd", []string{"--permanent", fmt.Sprintf("--add-port=%d/%s", target.port, target.protocol)}, "/", false); err != nil {
			return err
		}
		return runner.Run(ctx, "firewall-cmd", []string{"--reload"}, "/", false)
	}
	return nil
}

func upsertRoute(routes []config.Route, route config.Route) []config.Route {
	for i := range routes {
		if routes[i].Name == route.Name {
			routes[i] = route
			return routes
		}
	}
	return append(routes, route)
}
func removeRoute(routes []config.Route, name string) []config.Route {
	result := routes[:0]
	for _, route := range routes {
		if route.Name != name {
			result = append(result, route)
		}
	}
	return result
}
func upsertTLS(listeners []config.TLSListener, name string, publicPort int, routeName, domain string, privatePort int) []config.TLSListener {
	if domain == "" {
		return listeners
	}
	route := config.TLSRoute{Name: routeName, ServerNames: []string{domain}, Backend: fmt.Sprintf("127.0.0.1:%d", privatePort)}
	// A TLS socket is shareable by SNI, so identity is its listen address rather
	// than the display name. Remove the old managed route first in case its
	// public port changed, then merge into any listener already owning the new
	// address (including a SlipGate-created listener).
	listeners = removeTLSRoute(listeners, routeName)
	target, _ := canonicalTLSListen(fmt.Sprintf("0.0.0.0:%d", publicPort))
	for i := range listeners {
		listen, err := canonicalTLSListen(listeners[i].Listen)
		if err == nil && listen == target {
			listeners[i].Routes = append(listeners[i].Routes, route)
			return listeners
		}
	}
	return append(listeners, config.TLSListener{Name: name, Listen: fmt.Sprintf("0.0.0.0:%d", publicPort), Routes: []config.TLSRoute{route}})
}
func removeTLSRoute(listeners []config.TLSListener, routeName string) []config.TLSListener {
	result := listeners[:0]
	for _, listener := range listeners {
		routes := listener.Routes[:0]
		for _, route := range listener.Routes {
			if route.Name != routeName {
				routes = append(routes, route)
			}
		}
		listener.Routes = routes
		if len(listener.Routes) > 0 || listener.DefaultBackend != "" {
			result = append(result, listener)
		}
	}
	return result
}
func appendCSV(base []string, values ...string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range append(base, values...) {
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			if item != "" && !seen[item] {
				seen[item] = true
				result = append(result, item)
			}
		}
	}
	return result
}
