package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TaJirax/CottenRouter/internal/config"
	"github.com/TaJirax/CottenRouter/internal/dnswire"
)

const slipGateTLSRoutePrefix = "slipgate:naive:"

// slipGateTLSPatchTransaction records only files actually changed by the
// integration layer. Rollback restores their byte-for-byte snapshots in
// reverse order; Commit makes a later deferred rollback a no-op.
type slipGateTLSPatchTransaction struct {
	applied    []SlipGateTLSFilePatch
	committed  bool
	rolledBack bool
}

func applySlipGateTLSPatches(plan SlipGateTLSPlan) (*slipGateTLSPatchTransaction, error) {
	tx := &slipGateTLSPatchTransaction{}
	for _, patch := range plan.Patches {
		current, err := os.ReadFile(patch.Path)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("re-read SlipGate TLS artifact %q: %w", patch.Path, err), tx.Rollback())
		}
		if !bytes.Equal(current, patch.Before) {
			return nil, errors.Join(fmt.Errorf("SlipGate TLS artifact %q changed after planning; refusing a stale write", patch.Path), tx.Rollback())
		}
		if bytes.Equal(patch.Before, patch.After) {
			continue
		}
		info, err := os.Stat(patch.Path)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("stat SlipGate TLS artifact %q: %w", patch.Path, err), tx.Rollback())
		}
		if err := atomicWrite(patch.Path, patch.After, patch.Mode); err != nil {
			return nil, errors.Join(fmt.Errorf("atomically patch SlipGate TLS artifact %q: %w", patch.Path, err), tx.Rollback())
		}
		restorePatch := patch
		restorePatch.Mode = info.Mode().Perm()
		tx.applied = append(tx.applied, restorePatch)
	}
	return tx, nil
}

func (tx *slipGateTLSPatchTransaction) Commit() {
	if tx != nil {
		tx.committed = true
	}
}

func (tx *slipGateTLSPatchTransaction) Rollback() error {
	if tx == nil || tx.committed || tx.rolledBack {
		return nil
	}
	tx.rolledBack = true
	var result error
	for index := len(tx.applied) - 1; index >= 0; index-- {
		patch := tx.applied[index]
		if err := atomicWrite(patch.Path, patch.Before, patch.Mode); err != nil {
			result = errors.Join(result, fmt.Errorf("restore SlipGate TLS artifact %q: %w", patch.Path, err))
		}
	}
	return result
}

func restoreSlipGateTLSArtifacts(plan SlipGateTLSPlan) error {
	var result error
	for index := len(plan.Patches) - 1; index >= 0; index-- {
		patch := plan.Patches[index]
		if err := atomicWrite(patch.Path, patch.Before, patch.Mode); err != nil {
			result = errors.Join(result, fmt.Errorf("restore prior SlipGate TLS artifact %q: %w", patch.Path, err))
		}
	}
	return result
}

func snapshotManagedServiceState(ctx context.Context, runner Runner, service string) managedServiceState {
	state := managedServiceState{name: service}
	if output, _ := runner.Output(ctx, "systemctl", "is-active", service); len(output) > 0 {
		state.active = strings.TrimSpace(string(output))
	}
	if output, _ := runner.Output(ctx, "systemctl", "is-enabled", service); len(output) > 0 {
		state.enabled = strings.TrimSpace(string(output))
	}
	return state
}

func slipGateServiceNamesFromUnitList(unitData []byte) ([]string, error) {
	seen := make(map[string]bool)
	var services []string
	for _, line := range strings.Split(string(unitData), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "slipgate-") || !strings.HasSuffix(fields[0], ".service") {
			continue
		}
		service := strings.TrimSuffix(fields[0], ".service")
		if !safeServiceName(service) {
			return nil, fmt.Errorf("unsafe SlipGate systemd service name %q", service)
		}
		if !seen[service] {
			seen[service] = true
			services = append(services, service)
		}
	}
	sort.Strings(services)
	return services, nil
}

func snapshotSlipGateManagedServiceStates(ctx context.Context, runner Runner) ([]managedServiceState, error) {
	units, err := runner.Output(ctx, "systemctl", "list-unit-files", "--no-legend", "slipgate-*.service")
	if err != nil {
		return nil, fmt.Errorf("snapshot SlipGate services: %w", err)
	}
	services, err := slipGateServiceNamesFromUnitList(units)
	if err != nil {
		return nil, err
	}
	states := make([]managedServiceState, 0, len(services))
	for _, service := range services {
		states = append(states, snapshotManagedServiceState(ctx, runner, service))
	}
	return states, nil
}

func stopNewSlipGateManagedServices(previous []managedServiceState, runner Runner) {
	old := make(map[string]bool)
	for _, state := range previous {
		old[state.name] = true
	}
	units, err := runner.Output(context.Background(), "systemctl", "list-unit-files", "--no-legend", "slipgate-*.service")
	if err != nil {
		return
	}
	services, err := slipGateServiceNamesFromUnitList(units)
	if err != nil {
		return
	}
	for _, service := range services {
		if !old[service] {
			_ = runner.Run(context.Background(), "systemctl", []string{"disable", "--now", service}, "/", false)
		}
	}
}

func enabledSlipGateManagedServices(configData, unitData []byte) ([]string, error) {
	var document slipGateTLSConfigDocument
	if err := decodeStrictJSON(configData, &document); err != nil {
		return nil, fmt.Errorf("decode SlipGate config while selecting enabled services: %w", err)
	}
	if document.Tunnels == nil {
		return nil, fmt.Errorf("decode SlipGate config while selecting enabled services: tunnels field is required")
	}
	available := make(map[string]bool)
	for _, line := range strings.Split(string(unitData), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		service := strings.TrimSuffix(fields[0], ".service")
		if strings.HasPrefix(service, "slipgate-") && safeServiceName(service) {
			available[service] = true
		}
	}
	selected := make(map[string]bool)
	for index, raw := range document.Tunnels {
		var tunnel slipGateTLSTunnelEnvelope
		if err := decodeStrictJSON(raw, &tunnel); err != nil {
			return nil, fmt.Errorf("decode SlipGate tunnel %d while selecting enabled services: %w", index, err)
		}
		if !tunnel.Enabled {
			continue
		}
		if !slipGateTLSTagRE.MatchString(tunnel.Tag) || tunnel.Tag == "." || tunnel.Tag == ".." {
			return nil, fmt.Errorf("enabled SlipGate tunnel %d has unsafe tag %q", index, tunnel.Tag)
		}
		service := "slipgate-" + tunnel.Tag
		if available[service] && service != "slipgate-dnsrouter" && !strings.Contains(service, "iptables") {
			selected[service] = true
		}
	}
	if available["slipgate-socks5"] {
		selected["slipgate-socks5"] = true
	}
	services := make([]string, 0, len(selected))
	for service := range selected {
		services = append(services, service)
	}
	sort.Strings(services)
	return services, nil
}

func (m Manager) enableSlipGateManagedServices(ctx context.Context, configPath string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	units, err := m.Runner.Output(ctx, "systemctl", "list-unit-files", "--no-legend", "slipgate-*.service")
	if err != nil {
		return fmt.Errorf("list SlipGate services: %w", err)
	}
	services, err := enabledSlipGateManagedServices(data, units)
	if err != nil {
		return err
	}
	for _, service := range services {
		if err := m.Runner.Run(ctx, "systemctl", []string{"enable", service}, "/", false); err != nil {
			return fmt.Errorf("enable managed SlipGate service %s: %w", service, err)
		}
		// SlipGate's own installer starts these on port 53. `--now` would
		// leave an already-running unit on the config it was started with,
		// so restart it onto the loopback port CottenRouter just wrote.
		if err := m.Runner.Run(ctx, "systemctl", []string{"restart", service}, "/", false); err != nil {
			return fmt.Errorf("restart managed SlipGate service %s onto its private port: %w", service, err)
		}
	}
	return nil
}

// mergeSlipGateTLSListeners replaces only CottenRouter's stable SlipGate TLS
// entries. An old plan is required to prove ownership of a no-SNI default;
// unrelated defaults and SNI routes are always preserved or rejected on a
// collision. DNS routes live in Config.Routes and are not touched here.
func mergeSlipGateTLSListeners(existing []config.TLSListener, current, previous SlipGateTLSPlan) ([]config.TLSListener, error) {
	type defaultMapping struct{ backend, routeName string }
	previousDefaults := make(map[string]defaultMapping)
	for _, listener := range previous.Listeners {
		if listener.DefaultBackend == "" {
			continue
		}
		key, err := canonicalTLSListen(listener.Listen)
		if err != nil {
			return nil, fmt.Errorf("previous SlipGate TLS listener %q: %w", listener.Listen, err)
		}
		previousDefaults[key] = defaultMapping{backend: listener.DefaultBackend, routeName: listener.DefaultRouteName}
	}

	result := make([]config.TLSListener, 0, len(existing)+len(current.Listeners))
	indexes := make(map[string]int)
	for _, original := range existing {
		listener := cloneTLSListener(original)
		key, err := canonicalTLSListen(listener.Listen)
		if err != nil {
			return nil, fmt.Errorf("existing TLS listener %q: %w", listener.Listen, err)
		}
		if _, duplicate := indexes[key]; duplicate {
			return nil, fmt.Errorf("multiple existing TLS listeners bind %s", key)
		}
		kept := listener.Routes[:0]
		for _, route := range listener.Routes {
			if !managedSlipGateTLSRoute(route.Name) {
				kept = append(kept, route)
			}
		}
		listener.Routes = kept
		if oldDefault, managed := previousDefaults[key]; managed && listener.DefaultBackend == oldDefault.backend && listener.DefaultRouteName == oldDefault.routeName {
			listener.DefaultBackend = ""
			listener.DefaultRouteName = ""
		}
		indexes[key] = len(result)
		result = append(result, listener)
	}

	for _, fragment := range current.Listeners {
		key, err := canonicalTLSListen(fragment.Listen)
		if err != nil {
			return nil, fmt.Errorf("planned SlipGate TLS listener %q: %w", fragment.Listen, err)
		}
		index, found := indexes[key]
		if !found {
			indexes[key] = len(result)
			result = append(result, config.TLSListener{Name: fragment.Name, Listen: fragment.Listen})
			index = len(result) - 1
		}
		listener := &result[index]
		if fragment.DefaultBackend != "" {
			if (listener.DefaultBackend != "" || listener.DefaultRouteName != "") && (listener.DefaultBackend != fragment.DefaultBackend || listener.DefaultRouteName != fragment.DefaultRouteName) {
				return nil, fmt.Errorf("TLS listener %s already has unrelated default_backend route %q -> %s; cannot add no-SNI SlipGate StunTLS", key, listener.DefaultRouteName, listener.DefaultBackend)
			}
			listener.DefaultBackend = fragment.DefaultBackend
			listener.DefaultRouteName = fragment.DefaultRouteName
		}
		for _, plannedRoute := range fragment.Routes {
			for _, existingRoute := range listener.Routes {
				collision, err := tlsRoutesShareServerName(existingRoute, plannedRoute)
				if err != nil {
					return nil, err
				}
				if collision {
					return nil, fmt.Errorf("TLS listener %s SNI for SlipGate route %q conflicts with unrelated route %q", key, plannedRoute.Name, existingRoute.Name)
				}
			}
			listener.Routes = append(listener.Routes, plannedRoute)
		}
	}

	compacted := result[:0]
	for _, listener := range result {
		if len(listener.Routes) == 0 && listener.DefaultBackend == "" && listener.DefaultRouteName == "" {
			continue
		}
		sort.SliceStable(listener.Routes, func(i, j int) bool { return listener.Routes[i].Name < listener.Routes[j].Name })
		compacted = append(compacted, listener)
	}
	return compacted, nil
}

func slipGateTLSPlanIntegrated(existing []config.TLSListener, plan SlipGateTLSPlan) bool {
	if len(plan.Backends) == 0 {
		return false
	}
	for _, planned := range plan.Listeners {
		plannedListen, err := canonicalTLSListen(planned.Listen)
		if err != nil {
			continue
		}
		foundListener := false
		for _, listener := range existing {
			listen, err := canonicalTLSListen(listener.Listen)
			if err != nil || listen != plannedListen {
				continue
			}
			if planned.DefaultBackend != "" && (listener.DefaultBackend != planned.DefaultBackend || listener.DefaultRouteName != planned.DefaultRouteName) {
				continue
			}
			allRoutes := true
			for _, wanted := range planned.Routes {
				foundRoute := false
				for _, route := range listener.Routes {
					if route.Name == wanted.Name && route.Backend == wanted.Backend {
						foundRoute = true
						break
					}
				}
				if !foundRoute {
					allRoutes = false
					break
				}
			}
			if allRoutes {
				foundListener = true
				break
			}
		}
		if !foundListener {
			return false
		}
	}
	return true
}

func managedSlipGateTLSRoute(name string) bool {
	return strings.HasPrefix(name, slipGateTLSRoutePrefix)
}

func cloneTLSListener(listener config.TLSListener) config.TLSListener {
	clone := listener
	clone.Routes = append([]config.TLSRoute(nil), listener.Routes...)
	for index := range clone.Routes {
		clone.Routes[index].ServerNames = append([]string(nil), clone.Routes[index].ServerNames...)
	}
	return clone
}

func canonicalTLSListen(value string) (string, error) {
	address, err := net.ResolveTCPAddr("tcp", value)
	if err != nil {
		return "", err
	}
	host := ""
	if address.IP != nil {
		host = address.IP.String()
	}
	if host == "" {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, strconv.Itoa(address.Port)), nil
}

func tlsRoutesShareServerName(left, right config.TLSRoute) (bool, error) {
	names := make(map[string]bool)
	for _, value := range left.ServerNames {
		name, err := normalizeTLSServerName(value)
		if err != nil {
			return false, fmt.Errorf("TLS route %q has invalid SNI %q: %w", left.Name, value, err)
		}
		names[name] = true
	}
	for _, value := range right.ServerNames {
		name, err := normalizeTLSServerName(value)
		if err != nil {
			return false, fmt.Errorf("TLS route %q has invalid SNI %q: %w", right.Name, value, err)
		}
		if names[name] {
			return true, nil
		}
	}
	return false, nil
}

func normalizeTLSServerName(value string) (string, error) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "*.")
	return dnswire.NormalizeDomain(value)
}

// validateSlipGateTLSPublicPorts permits sharing an existing CottenRouter TLS
// listener, but never an unrelated OS listener/panel or the router's DNS/admin
// TCP sockets. Native SlipGate listeners represented by this plan are allowed
// because their artifacts are about to be moved to private loopback ports.
func validateSlipGateTLSPublicPorts(plan SlipGateTLSPlan, listeners []Listener, routerConfigPath string) error {
	reserved := map[int]string{53: "CottenRouter DNS", 9088: "CottenRouter admin"}
	if data, err := os.ReadFile(routerConfigPath); err == nil {
		var cfg config.Config
		if err := jsonUnmarshalConfig(data, &cfg); err != nil {
			return fmt.Errorf("decode CottenRouter config before SlipGate TLS merge: %w", err)
		}
		for label, value := range map[string]string{"DNS-over-TCP": cfg.ListenTCP, "admin": cfg.AdminListen} {
			if value == "" {
				continue
			}
			_, portText, splitErr := net.SplitHostPort(value)
			port, convErr := strconv.Atoi(portText)
			if splitErr != nil || convErr != nil {
				return fmt.Errorf("invalid existing CottenRouter %s listener %q", label, value)
			}
			reserved[port] = "CottenRouter " + label
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	backendsByPublic := make(map[int][]SlipGateTLSBackend)
	for _, backend := range plan.Backends {
		if owner := reserved[backend.PublicPort]; owner != "" {
			return fmt.Errorf("SlipGate TLS public port %d is reserved by %s", backend.PublicPort, owner)
		}
		backendsByPublic[backend.PublicPort] = append(backendsByPublic[backend.PublicPort], backend)
	}
	for _, listener := range listeners {
		protocol := strings.TrimSuffix(strings.ToLower(listener.Protocol), "6")
		planned := backendsByPublic[listener.Port]
		if protocol != "tcp" || len(planned) == 0 {
			continue
		}
		process := strings.ToLower(listener.Process)
		if strings.Contains(process, "cottenrouter") {
			continue
		}
		owned := false
		for _, backend := range planned {
			if slipGateTLSListenerOwnedByBackend(listener, backend) {
				owned = true
				break
			}
		}
		if !owned {
			return fmt.Errorf("SlipGate TLS public port %d is already owned by unrelated listener %s", listener.Port, listener.Process)
		}
	}
	return nil
}

// Kept behind a tiny wrapper so tests exercise the same permissive JSON
// decoding used by existing installer config mutation helpers.
func jsonUnmarshalConfig(data []byte, cfg *config.Config) error {
	return json.Unmarshal(data, cfg)
}

func slipGateTLSPublicPorts(plan SlipGateTLSPlan) []firewallPort {
	seen := make(map[int]bool)
	ports := make([]firewallPort, 0, len(plan.Listeners))
	for _, backend := range plan.Backends {
		if !seen[backend.PublicPort] {
			seen[backend.PublicPort] = true
			ports = append(ports, firewallPort{port: backend.PublicPort, protocol: "tcp"})
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].port < ports[j].port })
	return ports
}

func uniqueFirewallPorts(groups ...[]firewallPort) []firewallPort {
	seen := make(map[string]bool)
	var result []firewallPort
	for _, group := range groups {
		for _, item := range group {
			key := fmt.Sprintf("%d/%s", item.port, item.protocol)
			if !seen[key] {
				seen[key] = true
				result = append(result, item)
			}
		}
	}
	return result
}

func (m Manager) restartSlipGateTLSBackends(ctx context.Context, plan SlipGateTLSPlan) error {
	seen := make(map[string]bool)
	for _, backend := range plan.Backends {
		if !safeServiceName(backend.Service) {
			return fmt.Errorf("unsafe SlipGate TLS service name %q", backend.Service)
		}
		if seen[backend.Service] {
			continue
		}
		seen[backend.Service] = true
		if err := m.Runner.Run(ctx, "systemctl", []string{"restart", backend.Service}, "/", false); err != nil {
			return fmt.Errorf("restart SlipGate TLS service %s: %w", backend.Service, err)
		}
	}
	return nil
}

func (m Manager) waitForSlipGateTLSListeners(ctx context.Context, plan SlipGateTLSPlan) error {
	if len(plan.Backends) == 0 {
		return nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		listeners, err := m.Scan(ctx)
		if err != nil {
			return err
		}
		private, public := missingSlipGateTLSListeners(listeners, plan)
		if len(private) == 0 && len(public) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("SlipGate TLS integration incomplete: missing loopback backends %v; router-owned public TCP ports %v", private, public)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func missingSlipGateTLSListeners(listeners []Listener, plan SlipGateTLSPlan) (private []string, public []int) {
	for _, backend := range plan.Backends {
		found := false
		for _, listener := range listeners {
			protocol := strings.TrimSuffix(strings.ToLower(listener.Protocol), "6")
			if protocol == "tcp" && listener.Port == backend.PrivatePort && listenerIsLoopback(listener.Address) && slipGateTLSListenerOwnedByBackend(listener, backend) {
				found = true
				break
			}
		}
		if !found {
			private = append(private, fmt.Sprintf("%s@127.0.0.1:%d", backend.Service, backend.PrivatePort))
		}
	}
	seenPublic := make(map[int]bool)
	for _, backend := range plan.Backends {
		if seenPublic[backend.PublicPort] {
			continue
		}
		seenPublic[backend.PublicPort] = true
		found := false
		for _, listener := range listeners {
			protocol := strings.TrimSuffix(strings.ToLower(listener.Protocol), "6")
			if protocol == "tcp" && listener.Port == backend.PublicPort && strings.Contains(strings.ToLower(listener.Process), "cottenrouter") {
				found = true
				break
			}
		}
		if !found {
			public = append(public, backend.PublicPort)
		}
	}
	sort.Strings(private)
	sort.Ints(public)
	return private, public
}

func slipGateTLSListenerOwnedByBackend(listener Listener, backend SlipGateTLSBackend) bool {
	process := strings.ToLower(listener.Process)
	service := strings.ToLower(backend.Service)
	switch backend.Transport {
	case "naive":
		return strings.Contains(process, "caddy-naive") || strings.Contains(process, service)
	case "stuntls":
		return strings.Contains(process, service) || strings.Contains(process, "slipgate")
	default:
		return false
	}
}

// disableNativeSlipGateDNSRouter takes port 53 away from SlipGate's own DNS
// router. SlipGate only creates that unit when at least one DNS tunnel exists,
// so a NaiveProxy/StunTLS-only install has none and `disable --now` on it is a
// hard error that used to abort the whole installation. What matters is the
// end state, not the command's exit code.
func disableNativeSlipGateDNSRouter(ctx context.Context, runner Runner) error {
	_ = runner.Run(ctx, "systemctl", []string{"disable", "--now", "slipgate-dnsrouter"}, "/", false)
	state := snapshotManagedServiceState(ctx, runner, "slipgate-dnsrouter")
	if state.active == "active" || state.enabled == "enabled" {
		return fmt.Errorf("native SlipGate DNS router is still %s/%s; it would fight CottenRouter for port 53", state.active, state.enabled)
	}
	return nil
}
