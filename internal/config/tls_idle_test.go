package config

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTLSIdleTimeoutDefaultsValidationAndRoundTrip(t *testing.T) {
	newConfig := func() Config {
		return Config{Routes: []Route{{Name: "dns", Domains: []string{"example.com"}, Backend: "127.0.0.1:5301"}}}
	}
	cfg := newConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.TLSIdleTimeout() != 15*time.Minute {
		t.Fatal("missing bounded default")
	}
	cfg.TLSIdleTimeoutMS = 60000
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var restored Config
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if err = restored.Validate(); err != nil || restored.TLSIdleTimeout() != time.Minute {
		t.Fatalf("timeout not preserved: %v", err)
	}
	for _, invalid := range []int{-1, 999, 86400001} {
		cfg := newConfig()
		cfg.TLSIdleTimeoutMS = invalid
		if err := cfg.Validate(); err == nil {
			t.Fatalf("accepted timeout %d", invalid)
		}
	}
}
