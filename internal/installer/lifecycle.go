package installer

import (
	"context"
	"encoding/json"
	"fmt"
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

func Discover(routerConfig string) ([]ProjectState, error) {
	integrated := map[string]bool{}
	tcpEnabled := map[string]bool{}
	tlsDomains := map[string]string{}
	if data, err := os.ReadFile(routerConfig); err == nil {
		var cfg config.Config
		if json.Unmarshal(data, &cfg) == nil {
			for _, route := range cfg.Routes {
				integrated[route.Name] = true
				tcpEnabled[route.Name] = route.TCPBackend != "" && route.TCPBackend != "disabled"
			}
			for _, listener := range cfg.TLSListeners {
				for _, route := range listener.Routes {
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
	request.PrivatePort = plan.DNSPort
	configured, err := configure(spec, request, plan, data)
	if err != nil {
		return plan, err
	}
	previousRouter, routerErr := os.ReadFile(request.RouterConfig)
	if err := atomicWrite(spec.ConfigPath, configured, 0600); err != nil {
		return plan, err
	}
	if err := updateRouterConfig(request.RouterConfig, spec, request, plan); err != nil {
		_ = atomicWrite(spec.ConfigPath, data, 0600)
		return plan, err
	}
	if spec.Kind == ConfigSlipGate {
		if err := m.installContainment(ctx, spec); err != nil {
			_ = atomicWrite(spec.ConfigPath, data, 0600)
			if routerErr == nil {
				_ = atomicWrite(request.RouterConfig, previousRouter, 0644)
			}
			return plan, err
		}
		_ = m.Runner.Run(ctx, "systemctl", []string{"disable", "--now", "slipgate-dnsrouter"}, "/", false)
	} else if err := m.Runner.Run(ctx, "systemctl", []string{"restart", spec.Service}, "/", false); err != nil {
		_ = atomicWrite(spec.ConfigPath, data, 0600)
		if routerErr == nil {
			_ = atomicWrite(request.RouterConfig, previousRouter, 0644)
		}
		return plan, err
	}
	if err := m.Runner.Run(ctx, "systemctl", []string{"restart", "cottenrouter"}, "/", false); err != nil {
		_ = atomicWrite(spec.ConfigPath, data, 0600)
		if routerErr == nil {
			_ = atomicWrite(request.RouterConfig, previousRouter, 0644)
		}
		if spec.Kind != ConfigSlipGate {
			_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", spec.Service}, "/", false)
		}
		_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", "cottenrouter"}, "/", false)
		return plan, err
	}
	progress("Configuration updated without rerunning the upstream installer")
	return plan, nil
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
	if err := removeProjectConfig(routerConfig, spec); err != nil {
		return err
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
	for _, service := range services {
		if safeServiceName(service) {
			_ = m.Runner.Run(ctx, "systemctl", []string{"disable", "--now", service}, "/", false)
		}
	}
	if err := m.Runner.Run(ctx, "systemctl", []string{"restart", "cottenrouter"}, "/", false); err != nil {
		_ = atomicWrite(routerConfig, routerPrevious, 0644)
		for _, service := range services {
			if safeServiceName(service) && service != "slipgate-dnsrouter" {
				_ = m.Runner.Run(context.Background(), "systemctl", []string{"enable", "--now", service}, "/", false)
			}
		}
		_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", "cottenrouter"}, "/", false)
		return err
	}
	if purge {
		clean := filepath.Clean(spec.WorkDir)
		if clean == "/" || clean == "/opt" || clean == "/etc" {
			return fmt.Errorf("refusing unsafe purge path %q", clean)
		}
		if err := os.RemoveAll(clean); err != nil {
			return err
		}
	}
	return nil
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
	if spec.Kind == ConfigSlipGate {
		_ = m.Runner.Run(ctx, "systemctl", []string{"stop", "cottenrouter"}, "/", false)
		if err := m.runProtectedSlipGate(ctx, spec.WorkDir); err != nil {
			_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", "cottenrouter"}, "/", false)
			return err
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
	rollback := func() {
		_ = atomicWrite(spec.ConfigPath, backendPrevious, 0600)
		_ = atomicWrite(routerConfig, routerPrevious, 0644)
		if spec.Kind == ConfigSlipGate {
			_ = m.Runner.Run(context.Background(), "systemctl", []string{"disable", "--now", "slipgate-dnsrouter"}, "/", false)
		} else {
			_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", spec.Service}, "/", false)
		}
		_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", "cottenrouter"}, "/", false)
	}
	if err := syncProjectRoute(routerConfig, spec); err != nil {
		rollback()
		return err
	}
	if spec.Kind == ConfigSlipGate {
		if err := m.installContainment(ctx, spec); err != nil {
			rollback()
			return err
		}
		_ = m.Runner.Run(ctx, "systemctl", []string{"disable", "--now", "slipgate-dnsrouter"}, "/", false)
	} else if err := m.Runner.Run(ctx, "systemctl", []string{"restart", spec.Service}, "/", false); err != nil {
		rollback()
		return err
	}
	if err := m.Runner.Run(ctx, "systemctl", []string{"restart", "cottenrouter"}, "/", false); err != nil {
		rollback()
		return err
	}
	return nil
}

func syncProjectRoute(path string, spec Spec) error {
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
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	encoded, _ := json.MarshalIndent(cfg, "", "  ")
	return atomicWrite(path, append(encoded, '\n'), 0644)
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
	return atomicWrite(path, append(encoded, '\n'), 0644)
}
