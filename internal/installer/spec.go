package installer

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/TaJirax/CottenRouter/internal/dnswire"
)

type ConfigKind string

const (
	ConfigTOML     ConfigKind = "toml"
	ConfigEnv      ConfigKind = "env"
	ConfigSlipGate ConfigKind = "slipgate"
)

type Spec struct {
	ID, Name, Service, WorkDir, ConfigPath, TemplatePath string
	Kind                                                 ConfigKind
	DefaultPort                                          int
	SupportsTCP, SupportsDoT, SupportsDoH                bool
}

func Specs() []Spec {
	return []Spec{
		{ID: "cottendns", Name: "CottenDNS", Service: "cottendns", WorkDir: "/opt/cottenrouter/backends/cottendns", ConfigPath: "/opt/cottenrouter/backends/cottendns/server_config.toml", TemplatePath: "server_config.toml.simple", Kind: ConfigTOML, DefaultPort: 5301, SupportsTCP: true, SupportsDoT: true, SupportsDoH: true},
		{ID: "masterdnsvpn", Name: "MasterDnsVPN", Service: "masterdnsvpn", WorkDir: "/opt/cottenrouter/backends/masterdnsvpn", ConfigPath: "/opt/cottenrouter/backends/masterdnsvpn/server_config.toml", TemplatePath: "server_config.toml.simple", Kind: ConfigTOML, DefaultPort: 5302},
		{ID: "stormdns", Name: "StormDNS", Service: "stormdns", WorkDir: "/opt/cottenrouter/backends/stormdns", ConfigPath: "/opt/cottenrouter/backends/stormdns/server_config.toml", TemplatePath: "server_config.toml.simple", Kind: ConfigTOML, DefaultPort: 5303},
		{ID: "thefeed", Name: "thefeed", Service: "thefeed-server", WorkDir: "/opt/thefeed", ConfigPath: "/opt/thefeed/data/thefeed.env", Kind: ConfigEnv, DefaultPort: 5304},
		{ID: "slipgate", Name: "SlipGate", Service: "slipgate-dnsrouter", WorkDir: "/etc/slipgate", ConfigPath: "/etc/slipgate/config.json", Kind: ConfigSlipGate, DefaultPort: 5310},
	}
}

func FindSpec(id string) (Spec, bool) {
	for _, spec := range Specs() {
		if spec.ID == id {
			return spec, true
		}
	}
	return Spec{}, false
}

type Request struct {
	ProjectID, Domain, ExtraDomains, ChatDomains string
	PrivatePort                                  int
	EnableTCP, EnableDoT, EnableDoH              bool
	DoTDomain, DoHDomain                         string
	RouterConfig                                 string
}

func (r *Request) Validate() (Spec, error) {
	spec, ok := FindSpec(r.ProjectID)
	if !ok {
		return Spec{}, fmt.Errorf("unknown project %q", r.ProjectID)
	}
	if r.PrivatePort == 0 {
		r.PrivatePort = spec.DefaultPort
	}
	if r.PrivatePort < 1024 || r.PrivatePort > 65535 {
		return Spec{}, fmt.Errorf("private port must be between 1024 and 65535")
	}
	if spec.Kind != ConfigSlipGate {
		domain, err := dnswire.NormalizeDomain(r.Domain)
		if err != nil {
			return Spec{}, fmt.Errorf("domain: %w", err)
		}
		r.Domain = domain
		var normalizeErr error
		if r.ExtraDomains, normalizeErr = normalizeDomainCSV(r.ExtraDomains); normalizeErr != nil {
			return Spec{}, fmt.Errorf("extra domains: %w", normalizeErr)
		}
		if r.ChatDomains, normalizeErr = normalizeDomainCSV(r.ChatDomains); normalizeErr != nil {
			return Spec{}, fmt.Errorf("chat domains: %w", normalizeErr)
		}
	}
	if r.RouterConfig == "" {
		r.RouterConfig = "/etc/cottenrouter/config.json"
	}
	if r.EnableTCP && !spec.SupportsTCP {
		return Spec{}, fmt.Errorf("%s does not expose DNS-over-TCP", spec.Name)
	}
	if r.EnableDoT && !spec.SupportsDoT {
		return Spec{}, fmt.Errorf("%s does not expose DoT", spec.Name)
	}
	if r.EnableDoH && !spec.SupportsDoH {
		return Spec{}, fmt.Errorf("%s does not expose DoH", spec.Name)
	}
	if r.EnableDoT {
		name, err := dnswire.NormalizeDomain(r.DoTDomain)
		if err != nil {
			return Spec{}, fmt.Errorf("DoT hostname: %w", err)
		}
		r.DoTDomain = name
	}
	if r.EnableDoH {
		name, err := dnswire.NormalizeDomain(r.DoHDomain)
		if err != nil {
			return Spec{}, fmt.Errorf("DoH hostname: %w", err)
		}
		r.DoHDomain = name
	}
	return spec, nil
}

func normalizeDomainCSV(value string) (string, error) {
	var result []string
	seen := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		domain, err := dnswire.NormalizeDomain(item)
		if err != nil {
			return "", err
		}
		if !seen[domain] {
			seen[domain] = true
			result = append(result, domain)
		}
	}
	return strings.Join(result, ","), nil
}

type Listener struct {
	Port                       int
	Protocol, Process, Address string
}
type PortPlan struct {
	DNSPort, DoTPublicPort, DoTPrivatePort, DoHPublicPort, DoHPrivatePort int
	DoHMode                                                               string
	Conflicts                                                             []Listener
	Notes                                                                 []string
}

func PlanPorts(occupied []Listener, request Request) PortPlan {
	plan := PortPlan{DNSPort: request.PrivatePort, DoTPublicPort: 853, DoTPrivatePort: 8853, DoHPublicPort: 443, DoHPrivatePort: 8443, DoHMode: "router-front"}
	doHPrivateStart := plan.DoHPrivatePort
	ports := map[int][]Listener{}
	for _, item := range occupied {
		ports[item.Port] = append(ports[item.Port], item)
	}
	if len(ports[request.PrivatePort]) > 0 {
		plan.Conflicts = append(plan.Conflicts, ports[request.PrivatePort]...)
		plan.DNSPort = firstFree(ports, 5301, 5399)
		plan.Notes = append(plan.Notes, fmt.Sprintf("Private port %d is occupied; assigned %d instead.", request.PrivatePort, plan.DNSPort))
	}
	ports[plan.DNSPort] = append(ports[plan.DNSPort], Listener{Port: plan.DNSPort, Process: "reserved for selected backend"})
	if request.EnableDoH && len(ports[443]) > 0 {
		plan.Conflicts = append(plan.Conflicts, ports[443]...)
		plan.DoHPublicPort = firstFree(ports, 8443, 8999)
		plan.Notes = append(plan.Notes, fmt.Sprintf("Port 443 stays with the existing panel; DoH will use public TLS port %d.", plan.DoHPublicPort))
	}
	if request.EnableDoH && plan.DoHPublicPort > 0 {
		ports[plan.DoHPublicPort] = append(ports[plan.DoHPublicPort], Listener{Port: plan.DoHPublicPort, Process: "reserved public DoH"})
	}
	if request.EnableDoT && len(ports[853]) > 0 {
		plan.Conflicts = append(plan.Conflicts, ports[853]...)
		plan.DoTPublicPort = firstFree(ports, 8853, 8999)
		plan.Notes = append(plan.Notes, fmt.Sprintf("Port 853 is protected; DoT will use public port %d unless its owner is reconfigured.", plan.DoTPublicPort))
	}
	if request.EnableDoT && plan.DoTPublicPort > 0 {
		ports[plan.DoTPublicPort] = append(ports[plan.DoTPublicPort], Listener{Port: plan.DoTPublicPort, Process: "reserved public DoT"})
	}
	plan.DoTPrivatePort = firstFree(ports, 8853, 8999)
	if plan.DoTPrivatePort > 0 {
		ports[plan.DoTPrivatePort] = append(ports[plan.DoTPrivatePort], Listener{Port: plan.DoTPrivatePort, Process: "reserved private DoT"})
	}
	plan.DoHPrivatePort = firstFree(ports, doHPrivateStart, 8999)
	if plan.DoHPrivatePort > 0 {
		ports[plan.DoHPrivatePort] = append(ports[plan.DoHPrivatePort], Listener{Port: plan.DoHPrivatePort, Process: "reserved private DoH"})
	}
	return plan
}

func firstFree(occupied map[int][]Listener, start, end int) int {
	for port := start; port <= end; port++ {
		if len(occupied[port]) == 0 {
			return port
		}
	}
	return 0
}

func ParseSS(output string) []Listener {
	var result []Listener
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		address := fields[4]
		_, portText, err := net.SplitHostPort(address)
		if err != nil {
			continue
		}
		var port int
		if _, err := fmt.Sscanf(portText, "%d", &port); err != nil {
			continue
		}
		process := "unknown"
		if len(fields) > 6 {
			process = strings.Join(fields[6:], " ")
		}
		result = append(result, Listener{Port: port, Protocol: fields[0], Process: process, Address: address})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Port < result[j].Port })
	return result
}
