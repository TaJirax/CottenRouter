package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/TaJirax/CottenRouter/internal/dnswire"
)

const (
	defaultListen         = "0.0.0.0:53"
	defaultTimeoutMS      = 10000
	defaultPacketSize     = 4096
	defaultTCPMessageSize = 65535
	defaultMaxPending     = 8192
	defaultMaxTCPConn     = 256
)

type Config struct {
	ListenUDP            string        `json:"listen_udp"`
	ListenTCP            string        `json:"listen_tcp,omitempty"`
	QueryTimeoutMS       int           `json:"query_timeout_ms"`
	MaxPacketSize        int           `json:"max_packet_size"`
	MaxTCPMessageSize    int           `json:"max_tcp_message_size"`
	MaxPendingPerBackend int           `json:"max_pending_per_backend"`
	MaxTCPConnections    int           `json:"max_tcp_connections"`
	UnmatchedAction      string        `json:"unmatched_action"`
	AllowRemoteBackends  bool          `json:"allow_remote_backends"`
	SlipGateConfigs      []string      `json:"slipgate_configs,omitempty"`
	Limits               Limits        `json:"limits"`
	TLSListeners         []TLSListener `json:"tls_listeners,omitempty"`
	Routes               []Route       `json:"routes"`
}

type TLSListener struct {
	Name           string     `json:"name"`
	Listen         string     `json:"listen"`
	DefaultBackend string     `json:"default_backend,omitempty"`
	Routes         []TLSRoute `json:"routes"`
}

type TLSRoute struct {
	Name        string   `json:"name"`
	ServerNames []string `json:"server_names"`
	Backend     string   `json:"backend"`
}

type Limits struct {
	UDPWorkers                  int      `json:"udp_workers"`
	UDPQueue                    int      `json:"udp_queue"`
	GlobalQueriesPerSecond      int      `json:"global_queries_per_second"`
	GlobalQueryBurst            int      `json:"global_query_burst"`
	PerIPQueriesPerSecond       int      `json:"per_ip_queries_per_second"`
	PerIPQueryBurst             int      `json:"per_ip_query_burst"`
	MaxTrackedIPs               int      `json:"max_tracked_ips"`
	MaxTCPConnectionsPerIP      int      `json:"max_tcp_connections_per_ip"`
	MaxTCPQueriesPerConnection  int      `json:"max_tcp_queries_per_connection"`
	MaxTCPInflightPerConnection int      `json:"max_tcp_inflight_per_connection"`
	ResponseBytesPerSecond      int      `json:"response_bytes_per_second"`
	ResponseBurstBytes          int      `json:"response_burst_bytes"`
	IngressBytesPerSecond       int      `json:"ingress_bytes_per_second"`
	IngressBurstBytes           int      `json:"ingress_burst_bytes"`
	TrustedResolverCIDRs        []string `json:"trusted_resolver_cidrs,omitempty"`
}

type Route struct {
	Name       string        `json:"name"`
	Domains    []string      `json:"domains"`
	Backend    string        `json:"backend"`
	TCPBackend string        `json:"tcp_backend,omitempty"`
	Verify     *VerifyConfig `json:"verify,omitempty"`
	VerifyKey  []byte        `json:"-"`
}

type VerifyConfig struct {
	Key      string `json:"key,omitempty"`
	CertFile string `json:"cert_file,omitempty"`
	MTU      int    `json:"mtu,omitempty"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	baseDir := filepath.Dir(path)
	for _, slipGatePath := range cfg.SlipGateConfigs {
		if !filepath.IsAbs(slipGatePath) {
			slipGatePath = filepath.Join(baseDir, slipGatePath)
		}
		routes, err := LoadSlipGateRoutes(slipGatePath)
		if err != nil {
			return Config{}, err
		}
		cfg.Routes = append(cfg.Routes, routes...)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.ListenUDP == "" {
		c.ListenUDP = defaultListen
	}
	if c.QueryTimeoutMS == 0 {
		c.QueryTimeoutMS = defaultTimeoutMS
	}
	if c.MaxPacketSize == 0 {
		c.MaxPacketSize = defaultPacketSize
	}
	if c.MaxTCPMessageSize == 0 {
		c.MaxTCPMessageSize = defaultTCPMessageSize
	}
	if c.MaxPendingPerBackend == 0 {
		c.MaxPendingPerBackend = defaultMaxPending
	}
	if c.MaxTCPConnections == 0 {
		c.MaxTCPConnections = defaultMaxTCPConn
	}
	if c.UnmatchedAction == "" {
		c.UnmatchedAction = "drop"
	}
	c.Limits.applyDefaults()
}

func (l *Limits) applyDefaults() {
	if l.UDPWorkers == 0 {
		l.UDPWorkers = runtime.NumCPU() * 4
		if l.UDPWorkers < 8 {
			l.UDPWorkers = 8
		}
		if l.UDPWorkers > 128 {
			l.UDPWorkers = 128
		}
	}
	if l.UDPQueue == 0 {
		l.UDPQueue = 4096
	}
	if l.GlobalQueriesPerSecond == 0 {
		l.GlobalQueriesPerSecond = 20000
	}
	if l.GlobalQueryBurst == 0 {
		l.GlobalQueryBurst = 40000
	}
	if l.PerIPQueriesPerSecond == 0 {
		l.PerIPQueriesPerSecond = 2000
	}
	if l.PerIPQueryBurst == 0 {
		l.PerIPQueryBurst = 4000
	}
	if l.MaxTrackedIPs == 0 {
		l.MaxTrackedIPs = 16384
	}
	if l.MaxTCPConnectionsPerIP == 0 {
		l.MaxTCPConnectionsPerIP = 32
	}
	if l.MaxTCPQueriesPerConnection == 0 {
		l.MaxTCPQueriesPerConnection = 1024
	}
	if l.MaxTCPInflightPerConnection == 0 {
		l.MaxTCPInflightPerConnection = 8
	}
	if l.ResponseBytesPerSecond == 0 {
		l.ResponseBytesPerSecond = 10 * 1024 * 1024
	}
	if l.ResponseBurstBytes == 0 {
		l.ResponseBurstBytes = 4 * 1024 * 1024
	}
	if l.IngressBytesPerSecond == 0 {
		l.IngressBytesPerSecond = 10 * 1024 * 1024
	}
	if l.IngressBurstBytes == 0 {
		l.IngressBurstBytes = 4 * 1024 * 1024
	}
}

func (c *Config) Validate() error {
	c.applyDefaults()
	if _, err := net.ResolveUDPAddr("udp", c.ListenUDP); err != nil {
		return fmt.Errorf("invalid listen_udp %q: %w", c.ListenUDP, err)
	}
	if c.ListenTCP != "" {
		if _, err := net.ResolveTCPAddr("tcp", c.ListenTCP); err != nil {
			return fmt.Errorf("invalid listen_tcp %q: %w", c.ListenTCP, err)
		}
	}
	if c.QueryTimeoutMS < 100 || c.QueryTimeoutMS > int((10*time.Minute)/time.Millisecond) {
		return fmt.Errorf("query_timeout_ms must be between 100 and 600000")
	}
	if c.MaxPacketSize < 512 || c.MaxPacketSize > 65535 {
		return fmt.Errorf("max_packet_size must be between 512 and 65535")
	}
	if c.MaxTCPMessageSize < 512 || c.MaxTCPMessageSize > 65535 {
		return fmt.Errorf("max_tcp_message_size must be between 512 and 65535")
	}
	if c.MaxPendingPerBackend < 1 || c.MaxPendingPerBackend > 65535 {
		return fmt.Errorf("max_pending_per_backend must be between 1 and 65535")
	}
	if c.MaxTCPConnections < 1 || c.MaxTCPConnections > 1000000 {
		return fmt.Errorf("max_tcp_connections must be between 1 and 1000000")
	}
	if c.UnmatchedAction != "drop" && c.UnmatchedAction != "refused" {
		return fmt.Errorf("unmatched_action must be drop or refused")
	}
	if err := c.Limits.validate(); err != nil {
		return err
	}
	if len(c.Routes) == 0 {
		if len(c.TLSListeners) == 0 {
			return fmt.Errorf("at least one DNS or TLS route is required")
		}
	}
	if err := c.validateTLSListeners(); err != nil {
		return err
	}

	seenDomains := make(map[string]string)
	for i := range c.Routes {
		route := &c.Routes[i]
		route.Name = strings.TrimSpace(route.Name)
		if route.Name == "" {
			return fmt.Errorf("routes[%d].name is required", i)
		}
		if len(route.Domains) == 0 {
			return fmt.Errorf("route %q needs at least one domain", route.Name)
		}
		backend, err := net.ResolveUDPAddr("udp", route.Backend)
		if err != nil {
			return fmt.Errorf("route %q has invalid backend: %w", route.Name, err)
		}
		if !c.AllowRemoteBackends && (backend.IP == nil || !backend.IP.IsLoopback()) {
			return fmt.Errorf("route %q backend must be a loopback address (or set allow_remote_backends)", route.Name)
		}
		route.Backend = backend.String()
		if route.TCPBackend == "" {
			route.TCPBackend = route.Backend
		} else if route.TCPBackend != "disabled" {
			tcpBackend, err := net.ResolveTCPAddr("tcp", route.TCPBackend)
			if err != nil {
				return fmt.Errorf("route %q has invalid tcp_backend: %w", route.Name, err)
			}
			if !c.AllowRemoteBackends && (tcpBackend.IP == nil || !tcpBackend.IP.IsLoopback()) {
				return fmt.Errorf("route %q TCP backend must be a loopback address (or set allow_remote_backends)", route.Name)
			}
			route.TCPBackend = tcpBackend.String()
		}
		if route.Verify != nil {
			key, err := loadVerifyKey(*route.Verify)
			if err != nil {
				return fmt.Errorf("route %q verify config: %w", route.Name, err)
			}
			if route.Verify.MTU < 0 || route.Verify.MTU > 4096 {
				return fmt.Errorf("route %q verify MTU must be between 0 and 4096", route.Name)
			}
			route.VerifyKey = key
		}
		for j, domain := range route.Domains {
			normalized, err := dnswire.NormalizeDomain(domain)
			if err != nil {
				return fmt.Errorf("route %q: %w", route.Name, err)
			}
			if owner, exists := seenDomains[normalized]; exists {
				return fmt.Errorf("domain %q is assigned to both %q and %q", normalized, owner, route.Name)
			}
			seenDomains[normalized] = route.Name
			route.Domains[j] = normalized
		}
	}
	return nil
}

func (c *Config) validateTLSListeners() error {
	seenListeners := make(map[string]bool)
	if c.ListenTCP != "" {
		address, _ := net.ResolveTCPAddr("tcp", c.ListenTCP)
		seenListeners[address.String()] = true
	}
	for i := range c.TLSListeners {
		listener := &c.TLSListeners[i]
		listener.Name = strings.TrimSpace(listener.Name)
		if listener.Name == "" || listener.Listen == "" {
			return fmt.Errorf("TLS listener name and listen address are required")
		}
		address, err := net.ResolveTCPAddr("tcp", listener.Listen)
		if err != nil {
			return fmt.Errorf("TLS listener %q: %w", listener.Name, err)
		}
		listener.Listen = address.String()
		if seenListeners[listener.Listen] {
			return fmt.Errorf("duplicate TCP/TLS listen address %q", listener.Listen)
		}
		seenListeners[listener.Listen] = true
		if listener.DefaultBackend != "" {
			backend, err := c.validateTCPBackend(listener.Name, listener.DefaultBackend)
			if err != nil {
				return err
			}
			listener.DefaultBackend = backend
		}
		if len(listener.Routes) == 0 && listener.DefaultBackend == "" {
			return fmt.Errorf("TLS listener %q needs a route or default_backend", listener.Name)
		}
		seenNames := make(map[string]string)
		for j := range listener.Routes {
			route := &listener.Routes[j]
			route.Name = strings.TrimSpace(route.Name)
			if route.Name == "" || len(route.ServerNames) == 0 {
				return fmt.Errorf("TLS listener %q route needs a name and server_names", listener.Name)
			}
			backend, err := c.validateTCPBackend(route.Name, route.Backend)
			if err != nil {
				return err
			}
			route.Backend = backend
			for k, name := range route.ServerNames {
				name = strings.TrimPrefix(strings.TrimSpace(name), "*.")
				normalized, err := dnswire.NormalizeDomain(name)
				if err != nil {
					return fmt.Errorf("TLS route %q: %w", route.Name, err)
				}
				if owner := seenNames[normalized]; owner != "" {
					return fmt.Errorf("TLS server name %q is assigned to both %q and %q", normalized, owner, route.Name)
				}
				seenNames[normalized] = route.Name
				route.ServerNames[k] = normalized
			}
		}
	}
	return nil
}

func (c Config) validateTCPBackend(name, value string) (string, error) {
	backend, err := net.ResolveTCPAddr("tcp", value)
	if err != nil {
		return "", fmt.Errorf("%q has invalid TCP backend: %w", name, err)
	}
	if !c.AllowRemoteBackends && (backend.IP == nil || !backend.IP.IsLoopback()) {
		return "", fmt.Errorf("%q TCP backend must be loopback (or set allow_remote_backends)", name)
	}
	return backend.String(), nil
}

func (l Limits) validate() error {
	positive := map[string]int{
		"udp_workers":                     l.UDPWorkers,
		"udp_queue":                       l.UDPQueue,
		"global_queries_per_second":       l.GlobalQueriesPerSecond,
		"global_query_burst":              l.GlobalQueryBurst,
		"per_ip_queries_per_second":       l.PerIPQueriesPerSecond,
		"per_ip_query_burst":              l.PerIPQueryBurst,
		"max_tracked_ips":                 l.MaxTrackedIPs,
		"max_tcp_connections_per_ip":      l.MaxTCPConnectionsPerIP,
		"max_tcp_queries_per_connection":  l.MaxTCPQueriesPerConnection,
		"max_tcp_inflight_per_connection": l.MaxTCPInflightPerConnection,
		"response_bytes_per_second":       l.ResponseBytesPerSecond,
		"response_burst_bytes":            l.ResponseBurstBytes,
		"ingress_bytes_per_second":        l.IngressBytesPerSecond,
		"ingress_burst_bytes":             l.IngressBurstBytes,
	}
	for name, value := range positive {
		if value < 1 {
			return fmt.Errorf("limits.%s must be positive", name)
		}
	}
	if l.UDPWorkers > 4096 || l.UDPQueue > 1000000 || l.MaxTrackedIPs > 1000000 {
		return fmt.Errorf("worker, queue, or tracked-IP limit is unreasonably large")
	}
	for _, cidr := range l.TrustedResolverCIDRs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("invalid trusted resolver CIDR %q", cidr)
		}
	}
	return nil
}

func loadVerifyKey(verify VerifyConfig) ([]byte, error) {
	if (verify.Key == "") == (verify.CertFile == "") {
		return nil, fmt.Errorf("exactly one of key or cert_file is required")
	}
	if verify.CertFile != "" {
		data, err := os.ReadFile(verify.CertFile)
		if err != nil {
			return nil, err
		}
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("no PEM certificate in %s", verify.CertFile)
		}
		hash := sha256.Sum256(block.Bytes)
		return hash[:], nil
	}
	value := strings.TrimSpace(verify.Key)
	if data, err := os.ReadFile(value); err == nil {
		value = strings.TrimSpace(string(data))
	}
	key, err := hex.DecodeString(value)
	if err != nil || len(key) == 0 {
		return nil, fmt.Errorf("key must be hex or a path containing hex")
	}
	return key, nil
}

func (c Config) QueryTimeout() time.Duration {
	return time.Duration(c.QueryTimeoutMS) * time.Millisecond
}
