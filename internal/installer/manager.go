package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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
		if listener.Port == 53 && !strings.Contains(strings.ToLower(listener.Process), "cottenrouter") {
			return PortPlan{}, fmt.Errorf("port 53 is owned by %s; refusing to stop or replace it", listener.Process)
		}
	}
	for _, panelService := range []string{"x-ui", "3x-ui", "hiddify-panel"} {
		if output, err := m.Runner.Output(ctx, "systemctl", "is-active", panelService); err == nil && strings.TrimSpace(string(output)) == "active" {
			listeners = append(listeners, Listener{Port: 443, Protocol: "tcp", Process: panelService + " (protected panel)", Address: "0.0.0.0:443"})
		}
	}
	var externalListeners []Listener
	managedActive := false
	if output, err := m.Runner.Output(ctx, "systemctl", "is-active", spec.Service); err == nil && strings.TrimSpace(string(output)) == "active" {
		managedActive = true
	}
	for _, listener := range listeners {
		if !strings.Contains(strings.ToLower(listener.Process), "cottenrouter") && !(managedActive && listener.Port == request.PrivatePort) {
			externalListeners = append(externalListeners, listener)
		}
	}
	plan := PlanPorts(externalListeners, request)
	if plan.DNSPort == 0 || (request.EnableDoT && (plan.DoTPublicPort == 0 || plan.DoTPrivatePort == 0)) || (request.EnableDoH && plan.DoHPrivatePort == 0) {
		return plan, fmt.Errorf("no safe private port is available")
	}
	request.PrivatePort = plan.DNSPort

	progress("Resolving and verifying the current upstream installer")
	projects, err := catalog.DefaultResolver().Latest(ctx)
	if err != nil {
		return plan, err
	}
	var project *catalog.Project
	for i := range projects {
		if projects[i].ID == request.ProjectID {
			project = &projects[i]
			break
		}
	}
	if project == nil {
		return plan, fmt.Errorf("project %q missing from live catalog", request.ProjectID)
	}
	if err := os.MkdirAll(spec.WorkDir, 0750); err != nil {
		return plan, fmt.Errorf("create backend directory: %w", err)
	}
	if spec.TemplatePath != "" {
		if _, err := os.Stat(spec.ConfigPath); errors.Is(err, os.ErrNotExist) {
			templateURL := "https://raw.githubusercontent.com/" + project.RepoFullName + "/" + project.DefaultBranch + "/" + spec.TemplatePath
			data, err := m.download(ctx, templateURL)
			if err != nil {
				return plan, fmt.Errorf("download current config template: %w", err)
			}
			data, err = configure(spec, request, plan, data)
			if err != nil {
				return plan, err
			}
			if err := os.WriteFile(spec.ConfigPath, data, 0600); err != nil {
				return plan, err
			}
		}
	}
	installerData, err := m.download(ctx, project.InstallerURL)
	if err != nil {
		return plan, err
	}
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

	previous, previousErr := os.ReadFile(spec.ConfigPath)
	routerPrevious, routerPreviousErr := os.ReadFile(request.RouterConfig)
	progress("Stopping CottenRouter briefly; other private backends and panels remain untouched")
	_ = m.Runner.Run(ctx, "systemctl", []string{"stop", "cottenrouter"}, "/", false)
	completed := false
	defer func() {
		if !completed {
			if previousErr == nil {
				_ = atomicWrite(spec.ConfigPath, previous, 0600)
			}
			if routerPreviousErr == nil {
				_ = atomicWrite(request.RouterConfig, routerPrevious, 0644)
			}
			if spec.Kind != ConfigSlipGate {
				_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", spec.Service}, "/", false)
			}
			_ = m.Runner.Run(context.Background(), "systemctl", []string{"restart", "cottenrouter"}, "/", false)
		}
	}()

	progress("Running the verified upstream installer")
	if err := m.Runner.Run(ctx, "bash", []string{installerPath}, spec.WorkDir, true); err != nil {
		return plan, fmt.Errorf("upstream %s installer failed: %w", spec.Name, err)
	}
	if spec.Kind == ConfigSlipGate {
		progress("Opening SlipGate's native setup so every selected transport setting remains available")
		if err := m.Runner.Run(ctx, "slipgate", nil, spec.WorkDir, true); err != nil {
			return plan, fmt.Errorf("SlipGate setup: %w", err)
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
	if err := updateRouterConfig(request.RouterConfig, spec, request, plan); err != nil {
		return plan, err
	}
	if err := m.installContainment(ctx, spec); err != nil {
		return plan, fmt.Errorf("install backend resource safeguards: %w", err)
	}
	if spec.Kind == ConfigSlipGate {
		_ = m.Runner.Run(ctx, "systemctl", []string{"disable", "--now", "slipgate-dnsrouter"}, "/", false)
		_ = m.Runner.Run(ctx, "usermod", []string{"-aG", "slipgate", "cottenrouter"}, "/", false)
	} else if err := m.Runner.Run(ctx, "systemctl", []string{"restart", spec.Service}, "/", false); err != nil {
		return plan, fmt.Errorf("restart %s: %w", spec.Service, err)
	}
	progress("Restarting CottenRouter and verifying services")
	for _, target := range publicPorts(request, plan) {
		_ = openFirewall(ctx, m.Runner, target)
	}
	if err := m.Runner.Run(ctx, "systemctl", []string{"restart", "cottenrouter"}, "/", false); err != nil {
		return plan, err
	}
	completed = true
	return plan, nil
}

// installContainment makes CottenRouter the lifecycle parent of managed
// backends and places them in one bounded slice. A backend failure cannot take
// the public router down, while a router restart pauses public-facing work
// before the loopback backend is restarted.
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
Requires=cottenrouter.service
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
	return m.Runner.Run(ctx, "systemctl", []string{"daemon-reload"}, "/", false)
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
		data = setTOML(data, "DOMAIN", "[\""+request.Domain+"\"]")
		data = setTOML(data, "UDP_HOST", "\"127.0.0.1\"")
		data = setTOML(data, "UDP_PORT", strconv.Itoa(request.PrivatePort))
		if spec.ID == "cottendns" {
			// CottenRouter adds an outer rate limit; these caps keep the backend
			// bounded even when it is reached by another local process.
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
				data = setTOML(data, key, value)
			}
			data = setTOML(data, "TCP_LISTENER_ENABLED", strconv.FormatBool(request.EnableTCP))
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
			}
		}
	case ConfigEnv:
		data = setEnv(data, "THEFEED_DOMAIN", request.Domain)
		data = setEnv(data, "THEFEED_EXTRA_DOMAINS", request.ExtraDomains)
		data = setEnv(data, "THEFEED_CHAT_DOMAINS", request.ChatDomains)
		data = setEnv(data, "THEFEED_LISTEN", fmt.Sprintf("127.0.0.1:%d", request.PrivatePort))
		// thefeed natively caps live sessions and messages; also bound the
		// persistent account set so hostile registrations cannot grow forever.
		data = setEnv(data, "THEFEED_CHAT_MAX_ACCOUNTS", "50000")
	}
	return data, nil
}

func setTOML(data []byte, key, value string) []byte {
	return setLine(data, `(?m)^\s*`+regexp.QuoteMeta(key)+`\s*=.*$`, key+" = "+value)
}
func setEnv(data []byte, key, value string) []byte {
	return setLine(data, `(?m)^`+regexp.QuoteMeta(key)+`=.*$`, key+"="+value)
}
func setLine(data []byte, pattern, line string) []byte {
	re := regexp.MustCompile(pattern)
	if re.Match(data) {
		return re.ReplaceAll(data, []byte(line))
	}
	return append(bytes.TrimRight(data, "\r\n"), []byte("\n"+line+"\n")...)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
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
	var cfg config.Config
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("decode router config: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cfg.ListenUDP, cfg.ListenTCP, cfg.AdminListen = "0.0.0.0:53", "0.0.0.0:53", "127.0.0.1:9088"
	cfg.MaxPacketSize = 16 * 1024
	cfg.Routes = removeRoute(cfg.Routes, "bootstrap-placeholder")
	if spec.Kind == ConfigSlipGate {
		routes, err := config.LoadSlipGateRoutes(spec.ConfigPath)
		if err != nil {
			return err
		}
		for _, route := range routes {
			cfg.Routes = upsertRoute(cfg.Routes, route)
		}
	} else {
		domains := []string{request.Domain}
		if spec.Kind == ConfigEnv {
			domains = appendCSV(domains, request.ExtraDomains, request.ChatDomains)
		}
		tcpBackend := "disabled"
		if request.EnableTCP {
			tcpBackend = fmt.Sprintf("127.0.0.1:%d", request.PrivatePort)
		}
		cfg.Routes = upsertRoute(cfg.Routes, config.Route{Name: spec.ID, Domains: domains, Backend: fmt.Sprintf("127.0.0.1:%d", request.PrivatePort), TCPBackend: tcpBackend})
	}
	if request.EnableDoT {
		cfg.TLSListeners = upsertTLS(cfg.TLSListeners, "dot", plan.DoTPublicPort, "cottendns-dot", request.DoTDomain, plan.DoTPrivatePort)
	} else {
		cfg.TLSListeners = removeTLSRoute(cfg.TLSListeners, "cottendns-dot")
	}
	if request.EnableDoH && plan.DoHMode == "router-front" {
		cfg.TLSListeners = upsertTLS(cfg.TLSListeners, "https", 443, "cottendns-doh", request.DoHDomain, plan.DoHPrivatePort)
	} else {
		cfg.TLSListeners = removeTLSRoute(cfg.TLSListeners, "cottendns-doh")
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("generated router config: %w", err)
	}
	encoded, _ := json.MarshalIndent(cfg, "", "  ")
	return atomicWrite(path, append(encoded, '\n'), 0644)
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
		ports = append(ports, firewallPort{443, "tcp"})
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
	for i := range listeners {
		if listeners[i].Name == name {
			listeners[i].Listen = fmt.Sprintf("0.0.0.0:%d", publicPort)
			for j := range listeners[i].Routes {
				if listeners[i].Routes[j].Name == routeName {
					listeners[i].Routes[j] = route
					return listeners
				}
			}
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
