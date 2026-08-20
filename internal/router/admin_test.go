package router

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/TaJirax/CottenRouter/internal/config"
	"github.com/TaJirax/CottenRouter/internal/telemetry"
)

func TestAdminStatusIsLoopbackReadable(t *testing.T) {
	server, err := New(config.Config{Routes: []config.Route{{Name: "test", Domains: []string{"test.example"}, Backend: "127.0.0.1:5301"}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server.stats.Query("dns/udp", "test", 42)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.ServeAdmin(ctx, listener) }()
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + listener.Addr().String() + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var snapshot telemetry.Snapshot
	if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, metric := range snapshot.Protocols {
		if metric.Protocol == "dns/udp" && metric.Route == "test" && metric.BytesIn == 42 {
			found = true
		}
	}
	if !found || snapshot.MemoryBytes == 0 || snapshot.Goroutines == 0 {
		t.Fatalf("unexpected status: %+v", snapshot)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
