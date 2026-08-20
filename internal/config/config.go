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
	"strings"
	"time"

	"github.com/TaJirax/CottenRouter/internal/dnswire"
)

const (
	defaultListen     = "0.0.0.0:53"
	defaultTimeoutMS  = 10000
	defaultPacketSize = 65535
	defaultMaxPending = 8192
	defaultMaxTCPConn = 1024
)

type Config struct {
	ListenUDP            string   `json:"listen_udp"`
	ListenTCP            string   `json:"listen_tcp,omitempty"`
	QueryTimeoutMS       int      `json:"query_timeout_ms"`
	MaxPacketSize        int      `json:"max_packet_size"`
	MaxPendingPerBackend int      `json:"max_pending_per_backend"`
	MaxTCPConnections    int      `json:"max_tcp_connections"`
	UnmatchedAction      string   `json:"unmatched_action"`
	AllowRemoteBackends  bool     `json:"allow_remote_backends"`
	SlipGateConfigs      []string `json:"slipgate_configs,omitempty"`
	Routes               []Route  `json:"routes"`
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
	if c.MaxPendingPerBackend == 0 {
		c.MaxPendingPerBackend = defaultMaxPending
	}
	if c.MaxTCPConnections == 0 {
		c.MaxTCPConnections = defaultMaxTCPConn
	}
	if c.UnmatchedAction == "" {
		c.UnmatchedAction = "drop"
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
	if c.MaxPendingPerBackend < 1 || c.MaxPendingPerBackend > 65535 {
		return fmt.Errorf("max_pending_per_backend must be between 1 and 65535")
	}
	if c.MaxTCPConnections < 1 || c.MaxTCPConnections > 1000000 {
		return fmt.Errorf("max_tcp_connections must be between 1 and 1000000")
	}
	if c.UnmatchedAction != "drop" && c.UnmatchedAction != "refused" {
		return fmt.Errorf("unmatched_action must be drop or refused")
	}
	if len(c.Routes) == 0 {
		return fmt.Errorf("at least one route is required")
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
