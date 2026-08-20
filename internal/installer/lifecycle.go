package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/TaJirax/CottenRouter/internal/config"
)

type ProjectState struct {
	ID, Name, Service, ConfigPath     string
	Installed, Integrated             bool
	Domain, ExtraDomains, ChatDomains string
	DoTDomain, DoHDomain              string
	EnableTCP, EnableDoT, EnableDoH   bool
	PrivatePort                       int
}

type Credential struct {
	Label, Path, Value string
	Sensitive          bool
}

type managedServiceState struct {
	name, active, enabled string
}

func Discover(routerConfig string) ([]ProjectState, error) {
	integrated := map[string]bool{}
	tcpEnabled := map[string]bool{}
	tlsDomains := map[string]string{}
	var routerTLSListeners []config.TLSListener
	if data, err := os.ReadFile(routerConfig); err == nil {
		var cfg config.Config
		if json.Unmarshal(data, &cfg) == nil {
			routerTLSListeners = cfg.TLSListeners
			for _, route := range cfg.Routes {
				integrated[route.Name] = true
				tcpEnabled[route.Name] = route.TCPBackend != "" && route.TCPBackend != "disabled"
			}
			for _, listener := range cfg.TLSListeners {
				for _, route := range listener.Routes {
					if managedSlipGateTLSRoute(route.Name) {
						integrated["slipgate"] = true
					}
					if len(route.ServerNames) > 0 {
						tlsDomains[route.Name] = route.ServerNames[0]
					}
				}
			}
		}
	}
	states := make([]ProjectState, 0, len(Specs()))
	for _, spec := range Specs() {
		state := ProjectState{ID: spec.ID, Name: spec.Name, Service: spec.Service, ConfigPath: spec.ConfigPath}
		data, err := os.ReadFile(spec.ConfigPath)
		state.Installed = err == nil
		state.Integrated = integrated[spec.ID]
		state.EnableTCP = tcpEnabled[spec.ID]
		if spec.ID == "cottendns" {
			state.DoTDomain, state.DoHDomain = tlsDomains["cottendns-dot"], tlsDomains["cottendns-doh"]
			state.EnableDoT, state.EnableDoH = state.DoTDomain != "", state.DoHDomain != ""
		}
		if spec.Kind == ConfigSlipGate {
			for name := range integrated {
				state.Integrated = state.Integrated || strings.HasPrefix(name, "slipgate:")
			}
			if err == nil {
				if plan, planErr := BuildSlipGateTLSPlan(data, SlipGateTLSOptions{ReadFile: os.ReadFile}); planErr == nil {
					state.Integrated = state.Integrated || slipGateTLSPlanIntegrated(routerTLSListeners, plan)
				}
			}
		}
		if err == nil {
			state.Domain, state.ExtraDomains, state.PrivatePort = configSummary(spec, data)
			if spec.Kind == ConfigEnv {
				state.ChatDomains = envValue(data, "THEFEED_CHAT_DOMAINS")
			}
		}
		states = append(states, state)
	}
	return states, nil
}

func configSummary(spec Spec, data []byte) (domain, extra string, port int) {
	if spec.Kind == ConfigEnv {
		domain = envValue(data, "THEFEED_DOMAIN")
		extra = envValue(data, "THEFEED_EXTRA_DOMAINS")
		_, _ = fmt.Sscanf(envValue(data, "THEFEED_LISTEN"), "127.0.0.1:%d", &port)
		return
	}
	if spec.Kind == ConfigTOML {
		domains := tomlDomains(data)
		if len(domains) > 0 {
			domain = domains[0]
			extra = strings.Join(domains[1:], ",")
		}
		port, _ = strconv.Atoi(tomlValue(data, "UDP_PORT"))
	}
	return
}

func envValue(data []byte, key string) string {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `=(.*)$`)
	match := re.FindSubmatch(data)
	if len(match) != 2 {
		return ""
	}
	return strings.Trim(strings.TrimSpace(string(match[1])), `"'`)
}

func tomlValue(data []byte, key string) string {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `\s*=\s*(.*?)\s*$`)
	match := re.FindSubmatch(data)
	if len(match) != 2 {
		return ""
	}
	return strings.Trim(strings.TrimSpace(string(match[1])), `"'`)
}

func firstTOMLDomain(data []byte) string {
	domains := tomlDomains(data)
	if len(domains) > 0 {
		return domains[0]
	}
	return ""
}

func tomlDomains(data []byte) []string {
	line := regexp.MustCompile(`(?m)^\s*DOMAIN\s*=\s*\[(.*?)\]\s*$`).FindSubmatch(data)
	if len(line) != 2 {
		return nil
	}
	values := regexp.MustCompile(`["']([^"']+)["']`).FindAllSubmatch(line[1], -1)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if len(value) == 2 {
			result = append(result, string(value[1]))
		}
	}
	return result
}

func Credentials(projectID string, reveal bool) ([]Credential, error) {
	spec, ok := FindSpec(projectID)
	if !ok {
		return nil, fmt.Errorf("unknown project %q", projectID)
	}
	var credentials []Credential
	addSecretFile := func(label, path string) {
		item := Credential{Label: label, Path: path, Sensitive: true, Value: "hidden (use --show-secrets)"}
		if reveal {
			if data, err := readSmallFile(path); err == nil {
				item.Value = strings.TrimSpace(string(data))
			}
		}
		if _, err := os.Stat(path); err == nil {
			credentials = append(credentials, item)
		}
	}
	switch spec.Kind {
	case ConfigTOML:
		keyPath := filepath.Join(spec.WorkDir, "encrypt_key.txt")
		if data, err := readSmallFile(spec.ConfigPath); err == nil {
			if configured := tomlValue(data, "ENCRYPTION_KEY_FILE"); configured != "" {
				keyPath = configured
				if !filepath.IsAbs(keyPath) {
					keyPath = filepath.Join(filepath.Dir(spec.ConfigPath), keyPath)
				}
			}
		}
		addSecretFile("Shared encryption key", keyPath)
	case ConfigEnv:
		if data, err := readSmallFile(spec.ConfigPath); err == nil {
			value := "hidden (use --show-secrets)"
			if reveal {
				value = envValue(data, "THEFEED_KEY")
			}
			credentials = append(credentials, Credential{Label: "thefeed client key", Path: spec.ConfigPath + ":THEFEED_KEY", Value: value, Sensitive: true})
		}
	case ConfigSlipGate:
		data, err := readSmallFile(spec.ConfigPath)
		if err == nil {
			var document struct {
				Tunnels []struct {
					Tag, Transport string
					DNSTT          *struct {
						PublicKey string `json:"public_key"`
					} `json:"dnstt"`
					VayDNS *struct {
						PublicKey string `json:"public_key"`
					} `json:"vaydns"`
				} `json:"tunnels"`
			}
			if json.Unmarshal(data, &document) == nil {
				for _, tunnel := range document.Tunnels {
					value := ""
					if tunnel.DNSTT != nil {
						value = tunnel.DNSTT.PublicKey
					}
					if tunnel.VayDNS != nil {
						value = tunnel.VayDNS.PublicKey
					}
					if value != "" {
						credentials = append(credentials, Credential{Label: tunnel.Tag + " public key", Path: spec.ConfigPath, Value: value})
					}
				}
			}
		}
	}
	if len(credentials) == 0 {
		return nil, fmt.Errorf("no client keys found; start the service once or use the project's native setup")
	}
	return credentials, nil
}

func readSmallFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > 1<<20 {
		return nil, fmt.Errorf("refusing to read oversized credential file")
	}
	return os.ReadFile(path)
}

func (m Manager) Configure(ctx context.Context, request Request, progress Progress) (PortPlan, error) {
	if progress == nil {
		progress = func(string) {}
	}
	if m.Runner == nil {
		m.Runner = OSRunner{}
	}
	if err := ensureServerHost(ctx, m.Runner); err != nil {
		return PortPlan{}, err
	}
	spec, err := request.Validate()
	if err != nil {
		return PortPlan{}, err
	}
	if spec.Kind == ConfigSlipGate {
		return PortPlan{}, fmt.Errorf("SlipGate uses multiple native tunnels; use Advanced to edit and re-integrate it")
	}
	data, err := os.ReadFile(spec.ConfigPath)
	if err != nil {
		return PortPlan{}, fmt.Errorf("%s is not installed: %w", spec.Name, err)
	}
	listeners, err := m.Scan(ctx)
	if err != nil {
		return PortPlan{}, err
	}
	listeners = m.protectPanels(ctx, listeners)
	filtered := listeners[:0]
	for _, listener := range listeners {
		if !strings.Contains(strings.ToLower(listener.Process), "cottenrouter") && !listenerOwnedBySpec(listener, spec) {
			filtered = append(filtered, listener)
		}
	}
	plan := PlanPorts(filtered, request)
	if plan.DNSPort == 0 || (request.EnableDoT && (plan.DoTPublicPort == 0 || plan.DoTPrivatePort == 0)) || (request.EnableDoH && (plan.DoHPublicPort == 0 || plan.DoHPrivatePort == 0)) {
		return plan, fmt.Errorf("no safe private port is available")
	}
	request.PrivatePort = plan.DNSPort
	configured, err := configure(spec, request, plan, data)
	if err != nil {
		return plan, err
	}
	previousRouter, err := os.ReadFile(request.RouterConfig)
	if err != nil {
		return plan, fmt.Errorf("read CottenRouter config: %w", err)
	}
	serviceState := managedServiceState{name: spec.Service}
	if spec.Kind != ConfigSlipGate {
		if output, outputErr := m.Runner.Output(ctx, "systemctl", "is-active", spec.Service); outputErr == nil || len(output) > 0 {
			serviceState.active = strings.TrimSpace(string(output))
		}
		if output, outputErr := m.Runner.Output(ctx, "systemctl", "is-enabled", spec.Service); outputErr == nil || len(output) > 0 {
			serviceState.enabled = strings.TrimSpace(string(output))
		}
	}
	mutated := false
	rollback := func() {
		if !mutated {
			return
		}
		_ = atomicWrite(spec.ConfigPath, data, 0600)
		_ = atomicWrite(request.RouterConfig, previousRouter, 0640)
		if spec.Kind != ConfigSlipGate {
			restoreManagedServicesExact([]managedServiceState{serviceState}, m.Runner)
		}
		_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", "cottenrouter"}, "/", false)
	}
	if err := atomicWrite(spec.ConfigPath, configured, 0600); err != nil {
		return plan, err
	}
	mutated = true
	if err := updateRouterConfig(request.RouterConfig, spec, request, plan); err != nil {
		rollback()
		return plan, err
	}
	if spec.Kind == ConfigSlipGate {
		if err := m.installContainment(ctx, spec); err != nil {
			rollback()
			return plan, err
		}
		if err := m.Runner.Run(ctx, "systemctl", []string{"disable", "--now", "slipgate-dnsrouter"}, "/", false); err != nil {
			rollback()
			return plan, err
		}
	} else if err := m.Runner.Run(ctx, "systemctl", []string{"enable", "--now", spec.Service}, "/", false); err != nil {
		rollback()
		return plan, err
	}
	if err := m.Runner.Run(ctx, "systemctl", []string{"restart", "cottenrouter"}, "/", false); err != nil {
		rollback()
		return plan, err
	}
	if err := m.Runner.Run(ctx, "systemctl", []string{"is-active", "--quiet", "cottenrouter"}, "/", false); err != nil {
		rollback()
		return plan, fmt.Errorf("CottenRouter did not become active: %w", err)
	}
	if err := m.waitForRouterListeners(ctx, request.RouterConfig); err != nil {
		rollback()
		return plan, err
	}
	if spec.Kind != ConfigSlipGate {
		if err := m.Runner.Run(ctx, "systemctl", []string{"is-active", "--quiet", spec.Service}, "/", false); err != nil {
			rollback()
			return plan, fmt.Errorf("%s did not become active: %w", spec.Name, err)
		}
		if err := m.waitForPrivateListeners(ctx, spec, request, plan); err != nil {
			rollback()
			return plan, err
		}
	}
	for _, target := range publicPorts(request, plan) {
		if err := openFirewall(ctx, m.Runner, target); err != nil {
			rollback()
			return plan, fmt.Errorf("open public %d/%s: %w", target.port, target.protocol, err)
		}
	}
	mutated = false
	progress("Configuration updated without rerunning the upstream installer")
	return plan, nil
}

func restoreManagedServicesExact(states []managedServiceState, runner Runner) {
	for _, state := range states {
		if state.name == "" || state.name == "slipgate-dnsrouter" {
			continue
		}
		switch state.enabled {
		case "enabled", "enabled-runtime", "linked", "linked-runtime", "alias":
			_ = runner.Run(context.Background(), "systemctl", []string{"enable", state.name}, "/", false)
		default:
			_ = runner.Run(context.Background(), "systemctl", []string{"disable", state.name}, "/", false)
		}
		if state.active == "active" || state.active == "activating" {
			_ = runner.Run(context.Background(), "systemctl", []string{"start", state.name}, "/", false)
		} else {
			_ = runner.Run(context.Background(), "systemctl", []string{"stop", state.name}, "/", false)
		}
	}
}

func (m Manager) Remove(ctx context.Context, projectID, routerConfig string, purge bool) error {
	if m.Runner == nil {
		m.Runner = OSRunner{}
	}
	if err := ensureServerHost(ctx, m.Runner); err != nil {
		return err
	}
	spec, ok := FindSpec(projectID)
	if !ok {
		return fmt.Errorf("unknown project %q", projectID)
	}
	if routerConfig == "" {
		routerConfig = "/etc/cottenrouter/config.json"
	}
	routerPrevious, err := os.ReadFile(routerConfig)
	if err != nil {
		return err
	}
	var slipGateTLSPlan SlipGateTLSPlan
	if spec.Kind == ConfigSlipGate {
		backendData, readErr := os.ReadFile(spec.ConfigPath)
		if readErr != nil {
			return fmt.Errorf("read SlipGate config before detach: %w", readErr)
		}
		slipGateTLSPlan, err = BuildSlipGateTLSPlan(backendData, SlipGateTLSOptions{ReadFile: os.ReadFile})
		if err != nil {
			return fmt.Errorf("snapshot SlipGate TLS routes before detach: %w", err)
		}
	}
	services := []string{spec.Service}
	if spec.Kind == ConfigSlipGate {
		services = nil
		output, _ := m.Runner.Output(ctx, "systemctl", "list-unit-files", "--no-legend", "slipgate-*.service")
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				services = append(services, strings.TrimSuffix(fields[0], ".service"))
			}
		}
	}
	states := make([]managedServiceState, 0, len(services))
	for _, service := range services {
		if !safeServiceName(service) {
			return fmt.Errorf("unsafe systemd service name %q", service)
		}
		state := managedServiceState{name: service}
		if output, _ := m.Runner.Output(ctx, "systemctl", "is-active", service); len(output) > 0 {
			state.active = strings.TrimSpace(string(output))
		}
		if output, _ := m.Runner.Output(ctx, "systemctl", "is-enabled", service); len(output) > 0 {
			state.enabled = strings.TrimSpace(string(output))
		}
		states = append(states, state)
		if state.active == "active" || state.enabled == "enabled" {
			if err := m.Runner.Run(ctx, "systemctl", []string{"disable", "--now", service}, "/", false); err != nil {
				restoreManagedServices(states, m.Runner)
				return fmt.Errorf("stop %s: %w", service, err)
			}
		}
	}
	if err := removeProjectConfigWithSlipGateTLS(routerConfig, spec, slipGateTLSPlan); err != nil {
		restoreManagedServices(states, m.Runner)
		return err
	}
	if err := m.Runner.Run(ctx, "systemctl", []string{"restart", "cottenrouter"}, "/", false); err != nil {
		_ = atomicWrite(routerConfig, routerPrevious, 0640)
		restoreManagedServices(states, m.Runner)
		_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", "cottenrouter"}, "/", false)
		return err
	}
	if err := m.Runner.Run(ctx, "systemctl", []string{"is-active", "--quiet", "cottenrouter"}, "/", false); err != nil {
		_ = atomicWrite(routerConfig, routerPrevious, 0644)
		restoreManagedServices(states, m.Runner)
		_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", "cottenrouter"}, "/", false)
		return fmt.Errorf("CottenRouter did not become active after detach: %w", err)
	}
	if err := m.waitForRouterListeners(ctx, routerConfig); err != nil {
		_ = atomicWrite(routerConfig, routerPrevious, 0644)
		restoreManagedServices(states, m.Runner)
		_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", "cottenrouter"}, "/", false)
		return err
	}
	if purge {
		if err := m.runNativeUninstall(ctx, spec); err != nil {
			_ = atomicWrite(routerConfig, routerPrevious, 0640)
			restoreManagedServices(states, m.Runner)
			_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", "cottenrouter"}, "/", false)
			return fmt.Errorf("native %s uninstall failed; configuration restored: %w", spec.Name, err)
		}
		for _, service := range services {
			dropIn := filepath.Join("/etc/systemd/system", service+".service.d", "cottenrouter.conf")
			if err := os.Remove(dropIn); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove %s integration: %w", service, err)
			}
			_ = os.Remove(filepath.Dir(dropIn))
			unit := filepath.Join("/etc/systemd/system", service+".service")
			if err := os.Remove(unit); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove leftover %s unit: %w", service, err)
			}
		}
		clean := filepath.Clean(spec.WorkDir)
		if clean == "/" || clean == "/opt" || clean == "/etc" {
			return fmt.Errorf("refusing unsafe purge path %q", clean)
		}
		if err := rejectSymlinkComponents(clean); err != nil {
			return err
		}
		if err := os.RemoveAll(clean); err != nil {
			return err
		}
		if err := removeInstallRecord(spec.ID); err != nil {
			return err
		}
		if err := m.Runner.Run(ctx, "systemctl", []string{"daemon-reload"}, "/", false); err != nil {
			return err
		}
	}
	return nil
}

func restoreManagedServices(states []managedServiceState, runner Runner) {
	for _, state := range states {
		if state.name == "slipgate-dnsrouter" {
			continue
		}
		switch {
		case state.enabled == "enabled" && state.active == "active":
			_ = runner.Run(context.Background(), "systemctl", []string{"enable", "--now", state.name}, "/", false)
		case state.enabled == "enabled":
			_ = runner.Run(context.Background(), "systemctl", []string{"enable", state.name}, "/", false)
		case state.active == "active":
			_ = runner.Run(context.Background(), "systemctl", []string{"start", state.name}, "/", false)
		}
	}
}

// Advanced opens the upstream-native configuration surface, then validates
// service startup and synchronizes only CottenRouter's DNS routes. Panel files
// and unrelated listeners are never edited.
func (m Manager) Advanced(ctx context.Context, projectID, routerConfig string) error {
	if m.Runner == nil {
		m.Runner = OSRunner{}
	}
	if err := ensureServerHost(ctx, m.Runner); err != nil {
		return err
	}
	spec, ok := FindSpec(projectID)
	if !ok {
		return fmt.Errorf("unknown project %q", projectID)
	}
	if routerConfig == "" {
		routerConfig = "/etc/cottenrouter/config.json"
	}
	backendPrevious, err := os.ReadFile(spec.ConfigPath)
	if err != nil {
		return err
	}
	routerPrevious, err := os.ReadFile(routerConfig)
	if err != nil {
		return err
	}
	var previousSlipGateTLSPlan, slipGateTLSPlan SlipGateTLSPlan
	var previousSlipGateTLSServiceStates []managedServiceState
	if spec.Kind == ConfigSlipGate {
		listeners, err := m.Scan(ctx)
		if err != nil {
			return err
		}
		previousSlipGateTLSPlan, err = BuildSlipGateTLSPlan(backendPrevious, SlipGateTLSOptions{Listeners: listeners, ReadFile: os.ReadFile})
		if err != nil {
			return fmt.Errorf("snapshot existing SlipGate TLS integration: %w", err)
		}
		previousSlipGateTLSServiceStates, err = snapshotSlipGateManagedServiceStates(ctx, m.Runner)
		if err != nil {
			return err
		}
	}
	var slipGateTLSTransaction *slipGateTLSPatchTransaction
	rollback := func() error {
		var result error
		if slipGateTLSTransaction != nil {
			result = errors.Join(result, slipGateTLSTransaction.Rollback())
		}
		if spec.Kind == ConfigSlipGate {
			stopNewSlipGateManagedServices(previousSlipGateTLSServiceStates, m.Runner)
			result = errors.Join(result, restoreSlipGateTLSArtifacts(previousSlipGateTLSPlan))
		}
		if writeErr := atomicWrite(spec.ConfigPath, backendPrevious, 0600); writeErr != nil {
			result = errors.Join(result, fmt.Errorf("restore backend config: %w", writeErr))
		}
		if writeErr := atomicWrite(routerConfig, routerPrevious, 0640); writeErr != nil {
			result = errors.Join(result, fmt.Errorf("restore router config: %w", writeErr))
		}
		if spec.Kind == ConfigSlipGate {
			_ = m.Runner.Run(context.Background(), "systemctl", []string{"daemon-reload"}, "/", false)
			restoreManagedServicesExact(previousSlipGateTLSServiceStates, m.Runner)
			_ = m.Runner.Run(context.Background(), "systemctl", []string{"disable", "--now", "slipgate-dnsrouter"}, "/", false)
		} else {
			_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", spec.Service}, "/", false)
		}
		_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", "cottenrouter"}, "/", false)
		return result
	}
	fail := func(cause error) error { return errors.Join(cause, rollback()) }
	if spec.Kind == ConfigSlipGate {
		if err := m.Runner.Run(ctx, "systemctl", []string{"stop", "cottenrouter"}, "/", false); err != nil {
			return fmt.Errorf("stop CottenRouter before SlipGate Advanced: %w", err)
		}
		if err := m.runProtectedSlipGate(ctx, spec.WorkDir); err != nil {
			return fail(err)
		}
		configured, err := os.ReadFile(spec.ConfigPath)
		if err != nil {
			return fail(fmt.Errorf("read configured SlipGate TLS transports: %w", err))
		}
		listeners, err := m.Scan(ctx)
		if err != nil {
			return fail(err)
		}
		listeners = m.protectPanels(ctx, listeners)
		slipGateTLSPlan, err = BuildSlipGateTLSPlan(configured, SlipGateTLSOptions{Listeners: listeners, ReadFile: os.ReadFile})
		if err != nil {
			return fail(fmt.Errorf("plan SlipGate TLS integration: %w", err))
		}
		if err := validateSlipGateTLSPublicPorts(slipGateTLSPlan, listeners, routerConfig); err != nil {
			return fail(err)
		}
		slipGateTLSTransaction, err = applySlipGateTLSPatches(slipGateTLSPlan)
		if err != nil {
			return fail(err)
		}
	} else {
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		if strings.ContainsAny(editor, " \t\r\n") {
			return fmt.Errorf("EDITOR must be one executable path without arguments")
		}
		if err := m.Runner.Run(ctx, editor, []string{spec.ConfigPath}, spec.WorkDir, true); err != nil {
			return err
		}
	}
	if err := syncProjectRouteWithSlipGateTLS(routerConfig, spec, slipGateTLSPlan, previousSlipGateTLSPlan); err != nil {
		return fail(err)
	}
	if spec.Kind == ConfigSlipGate {
		if err := m.installContainment(ctx, spec); err != nil {
			return fail(err)
		}
		if err := m.enableSlipGateManagedServices(ctx, spec.ConfigPath); err != nil {
			return fail(err)
		}
		if err := m.Runner.Run(ctx, "systemctl", []string{"disable", "--now", "slipgate-dnsrouter"}, "/", false); err != nil {
			return fail(err)
		}
		if err := m.restartSlipGateTLSBackends(ctx, slipGateTLSPlan); err != nil {
			return fail(err)
		}
		for _, target := range slipGateTLSPublicPorts(slipGateTLSPlan) {
			if err := openFirewall(ctx, m.Runner, target); err != nil {
				return fail(fmt.Errorf("open public %d/%s: %w", target.port, target.protocol, err))
			}
		}
	} else if err := m.Runner.Run(ctx, "systemctl", []string{"restart", spec.Service}, "/", false); err != nil {
		return fail(err)
	}
	if err := m.Runner.Run(ctx, "systemctl", []string{"restart", "cottenrouter"}, "/", false); err != nil {
		return fail(err)
	}
	if err := m.Runner.Run(ctx, "systemctl", []string{"is-active", "--quiet", "cottenrouter"}, "/", false); err != nil {
		return fail(fmt.Errorf("CottenRouter did not become active: %w", err))
	}
	if err := m.waitForRouterListeners(ctx, routerConfig); err != nil {
		return fail(err)
	}
	if spec.Kind == ConfigSlipGate {
		if err := m.waitForSlipGateTLSListeners(ctx, slipGateTLSPlan); err != nil {
			return fail(err)
		}
	} else {
		if err := m.Runner.Run(ctx, "systemctl", []string{"is-active", "--quiet", spec.Service}, "/", false); err != nil {
			return fail(fmt.Errorf("%s did not become active: %w", spec.Name, err))
		}
		request, plan, err := privateListenerExpectation(spec, spec.ConfigPath)
		if err != nil {
			return fail(err)
		}
		if err := m.waitForPrivateListeners(ctx, spec, request, plan); err != nil {
			return fail(err)
		}
	}
	if slipGateTLSTransaction != nil {
		slipGateTLSTransaction.Commit()
	}
	return nil
}

func privateListenerExpectation(spec Spec, configPath string) (Request, PortPlan, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return Request{}, PortPlan{}, err
	}
	_, _, port := configSummary(spec, data)
	if port == 0 {
		return Request{}, PortPlan{}, fmt.Errorf("%s config has no private DNS port", spec.Name)
	}
	request := Request{ProjectID: spec.ID, PrivatePort: port}
	plan := PortPlan{DNSPort: port}
	if spec.ID == "cottendns" {
		request.EnableTCP = strings.EqualFold(tomlValue(data, "TCP_LISTENER_ENABLED"), "true")
		request.EnableDoT = strings.EqualFold(tomlValue(data, "DOT_LISTENER_ENABLED"), "true")
		request.EnableDoH = strings.EqualFold(tomlValue(data, "DOH_LISTENER_ENABLED"), "true")
		plan.DoTPrivatePort, _ = strconv.Atoi(tomlValue(data, "DOT_LISTEN_PORT"))
		if strings.EqualFold(tomlValue(data, "DOH_COEXIST_MODE"), "behind") {
			plan.DoHPrivatePort, _ = strconv.Atoi(tomlValue(data, "DOH_BEHIND_PORT"))
		} else {
			plan.DoHPrivatePort, _ = strconv.Atoi(tomlValue(data, "DOH_LISTEN_PORT"))
		}
		if request.EnableDoT && plan.DoTPrivatePort == 0 {
			return Request{}, PortPlan{}, fmt.Errorf("CottenDNS has DoT enabled without a private port")
		}
		if request.EnableDoH && plan.DoHPrivatePort == 0 {
			return Request{}, PortPlan{}, fmt.Errorf("CottenDNS has DoH enabled without a private port")
		}
	}
	return request, plan, nil
}

func syncProjectRoute(path string, spec Spec) error {
	return syncProjectRouteInternal(path, spec, nil, nil)
}

func syncProjectRouteWithSlipGateTLS(path string, spec Spec, current, previous SlipGateTLSPlan) error {
	return syncProjectRouteInternal(path, spec, &current, &previous)
}

func syncProjectRouteInternal(path string, spec Spec, currentSlipGateTLS, previousSlipGateTLS *SlipGateTLSPlan) error {
	routerData, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg config.Config
	if err := json.Unmarshal(routerData, &cfg); err != nil {
		return err
	}
	if spec.Kind == ConfigSlipGate {
		routes, err := config.LoadSlipGateRoutes(spec.ConfigPath)
		if err != nil {
			return err
		}
		kept := cfg.Routes[:0]
		for _, route := range cfg.Routes {
			if !strings.HasPrefix(route.Name, "slipgate:") {
				kept = append(kept, route)
			}
		}
		cfg.Routes = append(kept, routes...)
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
		backendData, err := os.ReadFile(spec.ConfigPath)
		if err != nil {
			return err
		}
		domain, extra, port := configSummary(spec, backendData)
		if domain == "" || port == 0 {
			return fmt.Errorf("advanced config must retain a domain and loopback DNS port")
		}
		if spec.Kind == ConfigTOML && tomlValue(backendData, "UDP_HOST") != "127.0.0.1" {
			return fmt.Errorf("UDP_HOST must remain 127.0.0.1 so public traffic cannot bypass CottenRouter")
		}
		if spec.ID == "cottendns" {
			if strings.EqualFold(tomlValue(backendData, "TCP_IPV6_ENABLED"), "true") && tomlValue(backendData, "TCP_IPV6_HOST") != "::1" {
				return fmt.Errorf("TCP_IPV6_HOST must remain ::1 so plain DNS-over-TCP cannot bypass CottenRouter")
			}
			if strings.EqualFold(tomlValue(backendData, "DOT_LISTENER_ENABLED"), "true") && tomlValue(backendData, "DOT_LISTEN_HOST") != "127.0.0.1" {
				return fmt.Errorf("DOT_LISTEN_HOST must remain 127.0.0.1 so DoT cannot bypass CottenRouter")
			}
			if strings.EqualFold(tomlValue(backendData, "DOH_LISTENER_ENABLED"), "true") && tomlValue(backendData, "DOH_LISTEN_HOST") != "127.0.0.1" {
				return fmt.Errorf("DOH_LISTEN_HOST must remain 127.0.0.1 so DoH cannot bypass CottenRouter or claim a panel port")
			}
		}
		if spec.Kind == ConfigEnv && !strings.HasPrefix(envValue(backendData, "THEFEED_LISTEN"), "127.0.0.1:") {
			return fmt.Errorf("THEFEED_LISTEN must remain on 127.0.0.1 so public traffic cannot bypass CottenRouter")
		}
		domains := appendCSV([]string{domain}, extra)
		found := false
		for i := range cfg.Routes {
			if cfg.Routes[i].Name == spec.ID {
				cfg.Routes[i].Domains, cfg.Routes[i].Backend = domains, fmt.Sprintf("127.0.0.1:%d", port)
				if cfg.Routes[i].TCPBackend != "" && cfg.Routes[i].TCPBackend != "disabled" {
					cfg.Routes[i].TCPBackend = fmt.Sprintf("127.0.0.1:%d", port)
				}
				found = true
			}
		}
		if !found {
			cfg.Routes = append(cfg.Routes, config.Route{Name: spec.ID, Domains: domains, Backend: fmt.Sprintf("127.0.0.1:%d", port), TCPBackend: "disabled"})
			found = true
		}
		if spec.ID == "cottendns" {
			for i := range cfg.Routes {
				if cfg.Routes[i].Name == spec.ID {
					if strings.EqualFold(tomlValue(backendData, "TCP_LISTENER_ENABLED"), "true") {
						cfg.Routes[i].TCPBackend = fmt.Sprintf("127.0.0.1:%d", port)
					} else {
						cfg.Routes[i].TCPBackend = "disabled"
					}
				}
			}
			var syncErr error
			cfg.TLSListeners, syncErr = syncCottenEncryptedRoutes(cfg.TLSListeners, backendData, domains)
			if syncErr != nil {
				return syncErr
			}
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	encoded, _ := json.MarshalIndent(cfg, "", "  ")
	return atomicWrite(path, append(encoded, '\n'), 0640)
}

func syncCottenEncryptedRoutes(listeners []config.TLSListener, backendData []byte, domains []string) ([]config.TLSListener, error) {
	containsDomain := func(value string) bool {
		for _, domain := range domains {
			if domain == value {
				return true
			}
		}
		return false
	}
	syncRoute := func(routeName, enabledKey, hostKey, portKey string, enabled bool) error {
		if !enabled {
			listeners = removeTLSRoute(listeners, routeName)
			return nil
		}
		if tomlValue(backendData, hostKey) != "127.0.0.1" {
			return fmt.Errorf("%s must remain 127.0.0.1", hostKey)
		}
		port, err := strconv.Atoi(tomlValue(backendData, portKey))
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("%s must contain a valid private port", portKey)
		}
		found := false
		for i := range listeners {
			for j := range listeners[i].Routes {
				if listeners[i].Routes[j].Name != routeName {
					continue
				}
				for _, serverName := range listeners[i].Routes[j].ServerNames {
					if !containsDomain(serverName) {
						return fmt.Errorf("%s SNI %q is missing from DOMAIN; use the common editor to change encrypted hostnames", routeName, serverName)
					}
				}
				listeners[i].Routes[j].Backend = fmt.Sprintf("127.0.0.1:%d", port)
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s was enabled in native config; use the common editor once to choose its public hostname", enabledKey)
		}
		return nil
	}
	dotEnabled := strings.EqualFold(tomlValue(backendData, "DOT_LISTENER_ENABLED"), "true")
	if err := syncRoute("cottendns-dot", "DOT_LISTENER_ENABLED", "DOT_LISTEN_HOST", "DOT_LISTEN_PORT", dotEnabled); err != nil {
		return nil, err
	}
	dohEnabled := strings.EqualFold(tomlValue(backendData, "DOH_LISTENER_ENABLED"), "true")
	if strings.EqualFold(tomlValue(backendData, "DOH_COEXIST_MODE"), "behind") {
		dohEnabled = false
	}
	if dohEnabled && !strings.EqualFold(tomlValue(backendData, "DOH_TLS_ENABLED"), "true") {
		return nil, fmt.Errorf("DOH_TLS_ENABLED must stay true while CottenRouter performs TLS passthrough")
	}
	if dohEnabled {
		publicPort := 0
		for _, listener := range listeners {
			for _, route := range listener.Routes {
				if route.Name != "cottendns-doh" {
					continue
				}
				_, portText, err := net.SplitHostPort(listener.Listen)
				if err != nil {
					return nil, fmt.Errorf("CottenDNS DoH public listener %q: %w", listener.Listen, err)
				}
				publicPort, err = strconv.Atoi(portText)
				if err != nil {
					return nil, fmt.Errorf("CottenDNS DoH public listener has invalid port %q", portText)
				}
			}
		}
		privatePort, _ := strconv.Atoi(tomlValue(backendData, "DOH_LISTEN_PORT"))
		if err := validateCottenRouterFrontDoHTLS(backendData, PortPlan{DoHPublicPort: publicPort, DoHPrivatePort: privatePort}); err != nil {
			return nil, err
		}
	}
	if err := syncRoute("cottendns-doh", "DOH_LISTENER_ENABLED", "DOH_LISTEN_HOST", "DOH_LISTEN_PORT", dohEnabled); err != nil {
		return nil, err
	}
	return listeners, nil
}

func (m Manager) Service(ctx context.Context, projectID, action string) error {
	if m.Runner == nil {
		m.Runner = OSRunner{}
	}
	if err := ensureServerHost(ctx, m.Runner); err != nil {
		return err
	}
	spec, ok := FindSpec(projectID)
	if !ok {
		return fmt.Errorf("unknown project %q", projectID)
	}
	if action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("invalid service action %q", action)
	}
	if spec.Kind == ConfigSlipGate {
		return fmt.Errorf("manage SlipGate's individual tunnels through Advanced settings")
	}
	return m.Runner.Run(ctx, "systemctl", []string{action, spec.Service}, "/", false)
}

func ensureServerHost(ctx context.Context, runner Runner) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("server management is supported on Linux only")
	}
	uid, err := runner.Output(ctx, "id", "-u")
	if err != nil || strings.TrimSpace(string(uid)) != "0" {
		return fmt.Errorf("server management must run as root")
	}
	return nil
}

func removeProjectConfig(path string, spec Spec) error {
	return removeProjectConfigInternal(path, spec, nil)
}

func removeProjectConfigWithSlipGateTLS(path string, spec Spec, plan SlipGateTLSPlan) error {
	return removeProjectConfigInternal(path, spec, &plan)
}

func removeProjectConfigInternal(path string, spec Spec, slipGateTLSPlan *SlipGateTLSPlan) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg config.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	routes := cfg.Routes[:0]
	for _, route := range cfg.Routes {
		if route.Name != spec.ID && !(spec.Kind == ConfigSlipGate && strings.HasPrefix(route.Name, "slipgate:")) {
			routes = append(routes, route)
		}
	}
	cfg.Routes = routes
	if spec.Kind == ConfigSlipGate {
		previous := SlipGateTLSPlan{}
		if slipGateTLSPlan != nil {
			previous = *slipGateTLSPlan
		}
		cfg.TLSListeners, err = mergeSlipGateTLSListeners(cfg.TLSListeners, SlipGateTLSPlan{}, previous)
		if err != nil {
			return fmt.Errorf("remove SlipGate TLS routes: %w", err)
		}
	}
	if spec.ID == "cottendns" {
		cfg.TLSListeners = removeTLSRoute(removeTLSRoute(cfg.TLSListeners, "cottendns-dot"), "cottendns-doh")
	}
	if len(cfg.Routes) == 0 && len(cfg.TLSListeners) == 0 {
		cfg.Routes = []config.Route{{Name: "bootstrap-placeholder", Domains: []string{"disabled.invalid"}, Backend: "127.0.0.1:5399", TCPBackend: "disabled"}}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	encoded, _ := json.MarshalIndent(cfg, "", "  ")
	return atomicWrite(path, append(encoded, '\n'), 0640)
}
