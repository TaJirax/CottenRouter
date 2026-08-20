package installer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/TaJirax/CottenRouter/internal/config"
	"github.com/TaJirax/CottenRouter/internal/dnswire"
)

const (
	DefaultSlipGateTLSPrivatePortStart = 9443
	DefaultSlipGateTLSPrivatePortEnd   = 9499
)

// SlipGateTLSOptions describes the host state used while moving SlipGate TLS
// transports behind CottenRouter. ReadFile is injectable so planning stays
// side-effect free and the installer can snapshot every file before writing.
type SlipGateTLSOptions struct {
	PrivatePortStart int
	PrivatePortEnd   int
	Listeners        []Listener
	ReadFile         func(string) ([]byte, error)
}

// SlipGateTLSFilePatch is a transactional file mutation. The caller writes
// After atomically and can restore Before if any service or router check fails.
type SlipGateTLSFilePatch struct {
	Path          string
	Before, After []byte
	Mode          os.FileMode
}

// SlipGateTLSBackend records the public-to-private mapping for one native
// SlipGate TLS transport. PublicPort remains in SlipGate's JSON/client output;
// only the generated service artifact is moved to PrivatePort.
type SlipGateTLSBackend struct {
	Tag, Transport, Domain  string
	Service, RouteName      string
	PublicPort, PrivatePort int
	ArtifactPath            string
}

// SlipGateTLSPlan contains no filesystem side effects. Listeners are merge
// fragments: the caller must merge them into an existing listener by Listen,
// preserving unrelated SNI routes and rejecting an existing default backend.
type SlipGateTLSPlan struct {
	Backends  []SlipGateTLSBackend
	Patches   []SlipGateTLSFilePatch
	Listeners []config.TLSListener
}

type slipGateTLSTunnel struct {
	Tag, Transport, Domain string
	PublicPort             int
	artifactPath           string
	before                 []byte
	naive                  *naiveCaddyShape
	stun                   *stunTLSUnitShape
	privatePort            int
}

type slipGateTLSConfigDocument struct {
	Listen   json.RawMessage   `json:"listen"`
	Tunnels  []json.RawMessage `json:"tunnels"`
	Backends json.RawMessage   `json:"backends"`
	Users    json.RawMessage   `json:"users,omitempty"`
	Route    json.RawMessage   `json:"route"`
	Warp     json.RawMessage   `json:"warp,omitempty"`
}

type slipGateTLSTunnelEnvelope struct {
	Tag        string          `json:"tag"`
	Transport  string          `json:"transport"`
	Backend    string          `json:"backend"`
	Domain     string          `json:"domain"`
	Port       int             `json:"port,omitempty"`
	Enabled    bool            `json:"enabled"`
	DNSTT      json.RawMessage `json:"dnstt,omitempty"`
	Slipstream json.RawMessage `json:"slipstream,omitempty"`
	VayDNS     json.RawMessage `json:"vaydns,omitempty"`
	Naive      json.RawMessage `json:"naive,omitempty"`
	StunTLS    json.RawMessage `json:"stuntls,omitempty"`
}

type slipGateNaiveConfig struct {
	Email    string `json:"email"`
	DecoyURL string `json:"decoy_url"`
	Port     int    `json:"port"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
}

type slipGateStunTLSConfig struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
	Port int    `json:"port"`
}

var slipGateTLSTagRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// BuildSlipGateTLSPlan parses current SlipGate JSON, verifies the exact native
// artifact shapes, chooses distinct loopback ports, patches artifacts in
// memory, and returns CottenRouter TLS listener merge fragments.
func BuildSlipGateTLSPlan(configData []byte, options SlipGateTLSOptions) (SlipGateTLSPlan, error) {
	if options.PrivatePortStart == 0 {
		options.PrivatePortStart = DefaultSlipGateTLSPrivatePortStart
	}
	if options.PrivatePortEnd == 0 {
		options.PrivatePortEnd = DefaultSlipGateTLSPrivatePortEnd
	}
	if options.PrivatePortStart < 1024 || options.PrivatePortEnd > 65535 || options.PrivatePortStart > options.PrivatePortEnd {
		return SlipGateTLSPlan{}, fmt.Errorf("invalid SlipGate TLS private port range %d-%d", options.PrivatePortStart, options.PrivatePortEnd)
	}
	tunnels, err := parseSlipGateTLSTunnels(configData)
	if err != nil {
		return SlipGateTLSPlan{}, err
	}
	if len(tunnels) == 0 {
		return SlipGateTLSPlan{}, nil
	}
	if options.ReadFile == nil {
		return SlipGateTLSPlan{}, fmt.Errorf("SlipGate TLS artifact reader is required")
	}

	for i := range tunnels {
		tunnel := &tunnels[i]
		data, readErr := options.ReadFile(tunnel.artifactPath)
		if readErr != nil {
			return SlipGateTLSPlan{}, fmt.Errorf("read SlipGate %s artifact %q: %w", tunnel.Transport, tunnel.artifactPath, readErr)
		}
		tunnel.before = append([]byte(nil), data...)
		switch tunnel.Transport {
		case "naive":
			tunnel.naive, err = inspectNaiveCaddyfile(data, tunnel.Domain, tunnel.PublicPort, options.PrivatePortStart, options.PrivatePortEnd)
		case "stuntls":
			tunnel.stun, err = inspectStunTLSUnit(data, tunnel.PublicPort, options.PrivatePortStart, options.PrivatePortEnd)
		}
		if err != nil {
			return SlipGateTLSPlan{}, fmt.Errorf("validate SlipGate %s tunnel %q: %w", tunnel.Transport, tunnel.Tag, err)
		}
	}

	if err := allocateSlipGateTLSPrivatePorts(tunnels, options); err != nil {
		return SlipGateTLSPlan{}, err
	}
	return buildSlipGateTLSResult(tunnels)
}

func parseSlipGateTLSTunnels(data []byte) ([]slipGateTLSTunnel, error) {
	var document slipGateTLSConfigDocument
	if err := decodeStrictJSON(data, &document); err != nil {
		return nil, fmt.Errorf("decode current SlipGate config schema: %w", err)
	}
	if document.Tunnels == nil {
		return nil, fmt.Errorf("decode current SlipGate config schema: tunnels field is required")
	}
	for _, field := range []struct {
		name   string
		value  json.RawMessage
		prefix byte
	}{{"listen", document.Listen, '{'}, {"backends", document.Backends, '['}, {"route", document.Route, '{'}} {
		if err := requireJSONKind(field.value, field.prefix); err != nil {
			return nil, fmt.Errorf("decode current SlipGate config schema: %s %w", field.name, err)
		}
	}
	if len(document.Users) > 0 {
		if err := requireJSONKind(document.Users, '['); err != nil {
			return nil, fmt.Errorf("decode current SlipGate config schema: users %w", err)
		}
	}
	if len(document.Warp) > 0 {
		if err := requireJSONKind(document.Warp, '{'); err != nil {
			return nil, fmt.Errorf("decode current SlipGate config schema: warp %w", err)
		}
	}

	seenTags := make(map[string]bool)
	var result []slipGateTLSTunnel
	for index, raw := range document.Tunnels {
		var tunnel slipGateTLSTunnelEnvelope
		if err := decodeStrictJSON(raw, &tunnel); err != nil {
			return nil, fmt.Errorf("decode SlipGate tunnel %d: %w", index, err)
		}
		if !tunnel.Enabled || (tunnel.Transport != "naive" && tunnel.Transport != "stuntls") {
			continue
		}
		if !slipGateTLSTagRE.MatchString(tunnel.Tag) || tunnel.Tag == "." || tunnel.Tag == ".." {
			return nil, fmt.Errorf("SlipGate TLS tunnel %d has unsafe tag %q", index, tunnel.Tag)
		}
		if seenTags[tunnel.Tag] {
			return nil, fmt.Errorf("duplicate enabled SlipGate TLS tag %q", tunnel.Tag)
		}
		seenTags[tunnel.Tag] = true

		planned := slipGateTLSTunnel{Tag: tunnel.Tag, Transport: tunnel.Transport}
		switch tunnel.Transport {
		case "naive":
			if isNullJSON(tunnel.Naive) {
				return nil, fmt.Errorf("enabled naive tunnel %q is missing naive settings", tunnel.Tag)
			}
			var naive slipGateNaiveConfig
			if err := decodeStrictJSON(tunnel.Naive, &naive); err != nil {
				return nil, fmt.Errorf("decode naive settings for %q: %w", tunnel.Tag, err)
			}
			domain, err := dnswire.NormalizeDomain(tunnel.Domain)
			if err != nil {
				return nil, fmt.Errorf("naive tunnel %q domain: %w", tunnel.Tag, err)
			}
			if err := validatePublicTLSPort(naive.Port, tunnel.Tag); err != nil {
				return nil, err
			}
			planned.Domain, planned.PublicPort = domain, naive.Port
			planned.artifactPath = path.Join("/etc/slipgate/tunnels", tunnel.Tag, "Caddyfile")
		case "stuntls":
			if isNullJSON(tunnel.StunTLS) {
				return nil, fmt.Errorf("enabled stuntls tunnel %q is missing stuntls settings", tunnel.Tag)
			}
			var stun slipGateStunTLSConfig
			if err := decodeStrictJSON(tunnel.StunTLS, &stun); err != nil {
				return nil, fmt.Errorf("decode stuntls settings for %q: %w", tunnel.Tag, err)
			}
			if strings.TrimSpace(stun.Cert) == "" || strings.TrimSpace(stun.Key) == "" {
				return nil, fmt.Errorf("stuntls tunnel %q requires certificate and key paths", tunnel.Tag)
			}
			if err := validatePublicTLSPort(stun.Port, tunnel.Tag); err != nil {
				return nil, err
			}
			planned.PublicPort = stun.Port
			planned.artifactPath = path.Join("/etc/systemd/system", "slipgate-"+tunnel.Tag+".service")
		}
		result = append(result, planned)
	}
	return result, nil
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func isNullJSON(data json.RawMessage) bool {
	return len(bytes.TrimSpace(data)) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}

func requireJSONKind(data json.RawMessage, prefix byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != prefix {
		return fmt.Errorf("field has unexpected or missing JSON shape")
	}
	return nil
}

func validatePublicTLSPort(port int, tag string) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("SlipGate TLS tunnel %q has invalid public port %d", tag, port)
	}
	return nil
}

func allocateSlipGateTLSPrivatePorts(tunnels []slipGateTLSTunnel, options SlipGateTLSOptions) error {
	occupied := make(map[int][]Listener)
	blocked := make(map[int]bool)
	publicPorts := make(map[int]bool)
	for _, listener := range options.Listeners {
		if strings.HasPrefix(strings.ToLower(listener.Protocol), "tcp") {
			occupied[listener.Port] = append(occupied[listener.Port], listener)
			blocked[listener.Port] = true
		}
	}
	// A loopback backend cannot use a public port that CottenRouter will bind on
	// all addresses, even if that public listener is temporarily stopped.
	for _, tunnel := range tunnels {
		blocked[tunnel.PublicPort] = true
		publicPorts[tunnel.PublicPort] = true
	}

	reserved := make(map[int]bool)
	for i := range tunnels {
		candidate := existingSlipGateTLSPrivatePort(&tunnels[i])
		occupiedByOther := blocked[candidate] && !slipGateTLSListenersOwned(occupied[candidate], tunnels[i])
		if candidate < options.PrivatePortStart || candidate > options.PrivatePortEnd || publicPorts[candidate] || occupiedByOther || reserved[candidate] {
			continue
		}
		tunnels[i].privatePort = candidate
		reserved[candidate] = true
	}

	for i := range tunnels {
		if tunnels[i].privatePort != 0 {
			continue
		}
		for port := options.PrivatePortStart; port <= options.PrivatePortEnd; port++ {
			if !blocked[port] && !reserved[port] {
				tunnels[i].privatePort = port
				reserved[port] = true
				break
			}
		}
		if tunnels[i].privatePort == 0 {
			return fmt.Errorf("no distinct loopback port is available for SlipGate TLS tunnel %q in %d-%d", tunnels[i].Tag, options.PrivatePortStart, options.PrivatePortEnd)
		}
	}
	return nil
}

func existingSlipGateTLSPrivatePort(tunnel *slipGateTLSTunnel) int {
	if tunnel.naive != nil {
		return tunnel.naive.existingPrivatePort
	}
	if tunnel.stun != nil {
		return tunnel.stun.existingPrivatePort
	}
	return 0
}

func slipGateTLSListenersOwned(listeners []Listener, tunnel slipGateTLSTunnel) bool {
	if len(listeners) == 0 {
		return true
	}
	for _, listener := range listeners {
		process := strings.ToLower(listener.Process)
		switch tunnel.Transport {
		case "naive":
			if !strings.Contains(process, "caddy-naive") && !strings.Contains(process, "slipgate-"+strings.ToLower(tunnel.Tag)) {
				return false
			}
		case "stuntls":
			if !strings.Contains(process, "slipgate") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func buildSlipGateTLSResult(tunnels []slipGateTLSTunnel) (SlipGateTLSPlan, error) {
	plan := SlipGateTLSPlan{}
	listeners := make(map[int]*config.TLSListener)
	defaultOwners := make(map[int]string)
	sniOwners := make(map[int]map[string]string)

	for i := range tunnels {
		tunnel := &tunnels[i]
		var after []byte
		var err error
		switch tunnel.Transport {
		case "naive":
			after, err = tunnel.naive.patch(tunnel.Domain, tunnel.privatePort)
		case "stuntls":
			after, err = tunnel.stun.patch(tunnel.privatePort)
		}
		if err != nil {
			return SlipGateTLSPlan{}, fmt.Errorf("patch SlipGate %s tunnel %q: %w", tunnel.Transport, tunnel.Tag, err)
		}

		routeName := "slipgate:" + tunnel.Transport + ":" + tunnel.Tag
		backend := fmt.Sprintf("127.0.0.1:%d", tunnel.privatePort)
		listener := listeners[tunnel.PublicPort]
		if listener == nil {
			listener = &config.TLSListener{Name: fmt.Sprintf("slipgate-tls-%d", tunnel.PublicPort), Listen: fmt.Sprintf("0.0.0.0:%d", tunnel.PublicPort)}
			listeners[tunnel.PublicPort] = listener
		}
		if tunnel.Transport == "naive" {
			if sniOwners[tunnel.PublicPort] == nil {
				sniOwners[tunnel.PublicPort] = make(map[string]string)
			}
			if owner := sniOwners[tunnel.PublicPort][tunnel.Domain]; owner != "" {
				return SlipGateTLSPlan{}, fmt.Errorf("SlipGate naive SNI %q on public port %d is shared by %q and %q", tunnel.Domain, tunnel.PublicPort, owner, tunnel.Tag)
			}
			sniOwners[tunnel.PublicPort][tunnel.Domain] = tunnel.Tag
			listener.Routes = append(listener.Routes, config.TLSRoute{Name: routeName, ServerNames: []string{tunnel.Domain}, Backend: backend})
		} else {
			if owner := defaultOwners[tunnel.PublicPort]; owner != "" {
				return SlipGateTLSPlan{}, fmt.Errorf("no-SNI StunTLS tunnels %q and %q cannot share public port %d", owner, tunnel.Tag, tunnel.PublicPort)
			}
			defaultOwners[tunnel.PublicPort] = tunnel.Tag
			listener.DefaultBackend = backend
			listener.DefaultRouteName = routeName
		}

		plan.Backends = append(plan.Backends, SlipGateTLSBackend{
			Tag: tunnel.Tag, Transport: tunnel.Transport, Domain: tunnel.Domain,
			Service: "slipgate-" + tunnel.Tag, RouteName: routeName,
			PublicPort: tunnel.PublicPort, PrivatePort: tunnel.privatePort, ArtifactPath: tunnel.artifactPath,
		})
		plan.Patches = append(plan.Patches, SlipGateTLSFilePatch{Path: tunnel.artifactPath, Before: append([]byte(nil), tunnel.before...), After: after, Mode: 0644})
	}

	ports := make([]int, 0, len(listeners))
	for port := range listeners {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	for _, port := range ports {
		listener := listeners[port]
		sort.Slice(listener.Routes, func(i, j int) bool { return listener.Routes[i].Name < listener.Routes[j].Name })
		plan.Listeners = append(plan.Listeners, *listener)
	}
	sort.Slice(plan.Patches, func(i, j int) bool { return plan.Patches[i].Path < plan.Patches[j].Path })
	return plan, nil
}

type naiveCaddyShape struct {
	lines                  []string
	newline                string
	trailing               bool
	headerIndex, bindIndex int
	headerIndent           string
	existingPrivatePort    int
}

var (
	naivePublicHeaderRE  = regexp.MustCompile(`^(?:(0\.0\.0\.0|\[::\]))?:([0-9]{1,5}),[ \t]*([^,{}[:space:]]+)[ \t]*\{$`)
	naivePrivateHeaderRE = regexp.MustCompile(`^https://([^/:{}[:space:]]+):([0-9]{1,5})[ \t]*\{$`)
	naiveBindRE          = regexp.MustCompile(`^[ \t]*bind[ \t]+([^#[:space:]]+)[ \t]*(?:#.*)?$`)
	naiveAnyBindRE       = regexp.MustCompile(`^[ \t]*bind(?:[ \t]|$)`)
)

func inspectNaiveCaddyfile(data []byte, domain string, publicPort, privateStart, privateEnd int) (*naiveCaddyShape, error) {
	lines, newline, trailing, err := splitStrictText(data)
	if err != nil {
		return nil, err
	}
	shape := &naiveCaddyShape{lines: lines, newline: newline, trailing: trailing, headerIndex: -1, bindIndex: -1}
	depth, globalBlocks, siteEnd := 0, 0, -1
	currentPort, headerLoopback := 0, false

	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		before := depth
		if before == 0 && trimmed != "" && !strings.HasPrefix(trimmed, "#") && !strings.Contains(trimmed, "{") {
			return nil, fmt.Errorf("unexpected top-level Caddyfile statement %q", trimmed)
		}
		if before == 0 && !strings.HasPrefix(trimmed, "#") && strings.Contains(trimmed, "{") {
			if trimmed == "{" {
				globalBlocks++
				if globalBlocks > 1 {
					return nil, fmt.Errorf("Caddyfile has multiple global option blocks")
				}
			} else {
				if shape.headerIndex >= 0 {
					return nil, fmt.Errorf("Caddyfile has more than one site block")
				}
				matchedDomain := ""
				if match := naivePublicHeaderRE.FindStringSubmatch(trimmed); match != nil {
					currentPort, _ = strconv.Atoi(match[2])
					matchedDomain = match[3]
					if currentPort != publicPort {
						return nil, fmt.Errorf("Caddyfile public port %d does not match config port %d", currentPort, publicPort)
					}
				} else if match := naivePrivateHeaderRE.FindStringSubmatch(trimmed); match != nil {
					matchedDomain = match[1]
					currentPort, _ = strconv.Atoi(match[2])
					headerLoopback = true
				} else {
					return nil, fmt.Errorf("unexpected top-level Caddy site address %q", trimmed)
				}
				normalized, normalizeErr := dnswire.NormalizeDomain(matchedDomain)
				if normalizeErr != nil || normalized != domain {
					return nil, fmt.Errorf("Caddyfile TLS identity %q does not match config domain %q", matchedDomain, domain)
				}
				if currentPort < 1 || currentPort > 65535 {
					return nil, fmt.Errorf("Caddyfile has invalid listen port %d", currentPort)
				}
				shape.headerIndex = index
				shape.headerIndent = line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			}
		}

		depth += caddyBraceDelta(line)
		if depth < 0 {
			return nil, fmt.Errorf("Caddyfile has unbalanced braces")
		}
		if shape.headerIndex >= 0 && index > shape.headerIndex && before == 1 {
			if naiveAnyBindRE.MatchString(line) {
				if !naiveBindRE.MatchString(line) {
					return nil, fmt.Errorf("Caddyfile site has an unsupported bind directive %q", strings.TrimSpace(line))
				}
				if shape.bindIndex >= 0 {
					return nil, fmt.Errorf("Caddyfile site has multiple bind directives")
				}
				shape.bindIndex = index
			}
		}
		if shape.headerIndex >= 0 && index >= shape.headerIndex && depth == 0 {
			siteEnd = index
		}
	}
	if depth != 0 || shape.headerIndex < 0 || siteEnd <= shape.headerIndex {
		return nil, fmt.Errorf("Caddyfile does not contain one complete NaiveProxy site block")
	}
	if headerLoopback && currentPort >= privateStart && currentPort <= privateEnd && shape.bindIndex >= 0 {
		match := naiveBindRE.FindStringSubmatch(lines[shape.bindIndex])
		if match != nil && isLoopbackLiteral(match[1]) {
			shape.existingPrivatePort = currentPort
		}
	}
	return shape, nil
}

func (shape *naiveCaddyShape) patch(domain string, privatePort int) ([]byte, error) {
	if shape == nil || shape.headerIndex < 0 {
		return nil, fmt.Errorf("missing validated Caddyfile shape")
	}
	lines := append([]string(nil), shape.lines...)
	lines[shape.headerIndex] = fmt.Sprintf("%shttps://%s:%d {", shape.headerIndent, domain, privatePort)
	bindLine := shape.headerIndent + "  bind 127.0.0.1"
	if shape.bindIndex >= 0 {
		lines[shape.bindIndex] = bindLine
	} else {
		lines = append(lines[:shape.headerIndex+1], append([]string{bindLine}, lines[shape.headerIndex+1:]...)...)
	}
	return joinStrictText(lines, shape.newline, shape.trailing), nil
}

func caddyBraceDelta(line string) int {
	if index := strings.IndexByte(line, '#'); index >= 0 {
		line = line[:index]
	}
	return strings.Count(line, "{") - strings.Count(line, "}")
}

type stunTLSUnitShape struct {
	lines               []string
	newline             string
	trailing            bool
	execIndex           int
	addrStart, addrEnd  int
	portStart, portEnd  int
	existingPrivatePort int
}

var stuntlsServeRE = regexp.MustCompile(`(?:^|[ \t])stuntls[ \t]+serve(?:[ \t]|$)`)

func inspectStunTLSUnit(data []byte, publicPort, privateStart, privateEnd int) (*stunTLSUnitShape, error) {
	lines, newline, trailing, err := splitStrictText(data)
	if err != nil {
		return nil, err
	}
	shape := &stunTLSUnitShape{lines: lines, newline: newline, trailing: trailing, execIndex: -1}
	section, serviceSections := "", 0
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = trimmed
			if section == "[Service]" {
				serviceSections++
			}
			continue
		}
		if section == "[Service]" && strings.HasPrefix(trimmed, "ExecStart=") {
			if shape.execIndex >= 0 {
				return nil, fmt.Errorf("systemd unit has multiple ExecStart directives")
			}
			if strings.HasSuffix(trimmed, `\`) {
				return nil, fmt.Errorf("continued ExecStart lines are not supported")
			}
			shape.execIndex = index
		}
	}
	if serviceSections != 1 || shape.execIndex < 0 {
		return nil, fmt.Errorf("systemd unit must have one Service section and one ExecStart")
	}

	line := lines[shape.execIndex]
	prefix := strings.Index(line, "ExecStart=")
	command := line[prefix+len("ExecStart="):]
	if !stuntlsServeRE.MatchString(command) {
		return nil, fmt.Errorf("ExecStart is not the expected stuntls serve command")
	}
	addrStart, addrEnd, addr, err := systemdFlagValue(command, "addr")
	if err != nil {
		return nil, err
	}
	portStart, portEnd, portText, err := systemdFlagValue(command, "port")
	if err != nil {
		return nil, err
	}
	for _, required := range []string{"ssh", "cert", "key"} {
		if _, _, _, flagErr := systemdFlagValue(command, required); flagErr != nil {
			return nil, fmt.Errorf("expected StunTLS ExecStart: %w", flagErr)
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("StunTLS ExecStart has invalid --port %q", portText)
	}
	wildcard, loopback := isWildcardLiteral(addr), isLoopbackLiteral(addr)
	if !wildcard && !loopback {
		return nil, fmt.Errorf("StunTLS ExecStart has unsafe --addr %q", addr)
	}
	if wildcard && port != publicPort {
		return nil, fmt.Errorf("StunTLS unit public port %d does not match config port %d", port, publicPort)
	}
	if loopback && port != publicPort && (port < privateStart || port > privateEnd) {
		return nil, fmt.Errorf("StunTLS unit has unexpected loopback port %d", port)
	}
	base := prefix + len("ExecStart=")
	shape.addrStart, shape.addrEnd = base+addrStart, base+addrEnd
	shape.portStart, shape.portEnd = base+portStart, base+portEnd
	if loopback && port >= privateStart && port <= privateEnd {
		shape.existingPrivatePort = port
	}
	return shape, nil
}

func (shape *stunTLSUnitShape) patch(privatePort int) ([]byte, error) {
	if shape == nil || shape.execIndex < 0 {
		return nil, fmt.Errorf("missing validated StunTLS unit shape")
	}
	line := shape.lines[shape.execIndex]
	type replacement struct {
		start, end int
		value      string
	}
	replacements := []replacement{{shape.addrStart, shape.addrEnd, "127.0.0.1"}, {shape.portStart, shape.portEnd, strconv.Itoa(privatePort)}}
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].start > replacements[j].start })
	for _, item := range replacements {
		if item.start < 0 || item.end < item.start || item.end > len(line) {
			return nil, fmt.Errorf("invalid validated flag offsets")
		}
		line = line[:item.start] + item.value + line[item.end:]
	}
	lines := append([]string(nil), shape.lines...)
	lines[shape.execIndex] = line
	return joinStrictText(lines, shape.newline, shape.trailing), nil
}

func systemdFlagValue(command, name string) (start, end int, value string, err error) {
	pattern := regexp.MustCompile(`(?:^|[ \t])--` + regexp.QuoteMeta(name) + `(?:=|[ \t]+)([^ \t]+)`)
	matches := pattern.FindAllStringSubmatchIndex(command, -1)
	if len(matches) != 1 || len(matches[0]) < 4 {
		return 0, 0, "", fmt.Errorf("ExecStart must contain exactly one --%s value", name)
	}
	start, end = matches[0][2], matches[0][3]
	return start, end, command[start:end], nil
}

func isWildcardLiteral(value string) bool {
	return value == "0.0.0.0" || value == "::" || value == "[::]"
}

func isLoopbackLiteral(value string) bool {
	if value == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(value, "[]"))
	return ip != nil && ip.IsLoopback()
}

func splitStrictText(data []byte) (lines []string, newline string, trailing bool, err error) {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, "", false, fmt.Errorf("artifact is not valid UTF-8 text")
	}
	text := string(data)
	newline = "\n"
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	if strings.ContainsRune(text, '\r') {
		return nil, "", false, fmt.Errorf("artifact uses unsupported mixed line endings")
	}
	trailing = strings.HasSuffix(text, "\n")
	if trailing {
		text = strings.TrimSuffix(text, "\n")
	}
	return strings.Split(text, "\n"), newline, trailing, nil
}

func joinStrictText(lines []string, newline string, trailing bool) []byte {
	text := strings.Join(lines, newline)
	if trailing {
		text += newline
	}
	return []byte(text)
}
