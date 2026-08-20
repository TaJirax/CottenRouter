package config

import (
	"encoding/json"
	"fmt"
	"os"
)

var slipGateDNSTransports = map[string]bool{
	"dnstt": true, "slipstream": true, "vaydns": true, "external": true,
}

type slipGateConfig struct {
	Tunnels []slipGateTunnel `json:"tunnels"`
}

type slipGateTunnel struct {
	Tag       string `json:"tag"`
	Transport string `json:"transport"`
	Domain    string `json:"domain"`
	Port      int    `json:"port"`
	Enabled   bool   `json:"enabled"`
	DNSTT     *struct {
		MTU       int    `json:"mtu"`
		PublicKey string `json:"public_key"`
	} `json:"dnstt"`
	VayDNS *struct {
		MTU       int    `json:"mtu"`
		PublicKey string `json:"public_key"`
	} `json:"vaydns"`
	Slipstream *struct {
		Cert string `json:"cert"`
	} `json:"slipstream"`
}

// LoadSlipGateRoutes imports the DNS-routing portion of SlipGate's native
// config.json. Non-DNS transports (NaiveProxy, StunTLS, SSH, and SOCKS) remain
// managed directly by SlipGate and do not belong on port 53.
func LoadSlipGateRoutes(path string) ([]Route, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read SlipGate config %q: %w", path, err)
	}
	var cfg slipGateConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode SlipGate config %q: %w", path, err)
	}
	routes := make([]Route, 0, len(cfg.Tunnels))
	for _, tunnel := range cfg.Tunnels {
		if !tunnel.Enabled || !slipGateDNSTransports[tunnel.Transport] {
			continue
		}
		if tunnel.Tag == "" || tunnel.Domain == "" || tunnel.Port < 1 || tunnel.Port > 65535 {
			return nil, fmt.Errorf("SlipGate DNS tunnel %q has invalid tag, domain, or port", tunnel.Tag)
		}
		route := Route{
			Name:       "slipgate:" + tunnel.Tag,
			Domains:    []string{tunnel.Domain},
			Backend:    fmt.Sprintf("127.0.0.1:%d", tunnel.Port),
			TCPBackend: "disabled",
		}
		switch tunnel.Transport {
		case "dnstt":
			if tunnel.DNSTT != nil && tunnel.DNSTT.PublicKey != "" {
				route.Verify = &VerifyConfig{Key: tunnel.DNSTT.PublicKey, MTU: tunnel.DNSTT.MTU}
			}
		case "vaydns":
			if tunnel.VayDNS != nil && tunnel.VayDNS.PublicKey != "" {
				route.Verify = &VerifyConfig{Key: tunnel.VayDNS.PublicKey, MTU: tunnel.VayDNS.MTU}
			}
		case "slipstream":
			if tunnel.Slipstream != nil && tunnel.Slipstream.Cert != "" {
				route.Verify = &VerifyConfig{CertFile: tunnel.Slipstream.Cert}
			}
		}
		routes = append(routes, route)
	}
	return routes, nil
}

// NewFromSlipGate creates a standalone CottenRouter config from SlipGate's
// native configuration. The result can be marshaled as cottenrouter.json.
func NewFromSlipGate(path string) (Config, error) {
	routes, err := LoadSlipGateRoutes(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{ListenUDP: defaultListen, Routes: routes}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
