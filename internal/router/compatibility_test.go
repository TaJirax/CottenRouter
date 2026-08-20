package router

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/TaJirax/CottenRouter/internal/config"
)

// TestAllDNSBackendsShareOneFrontendUnderLoad is the compatibility gate for
// CottenDNS, MasterDnsVPN, StormDNS, thefeed, and SlipGate DNS transports. It
// deliberately reuses transaction IDs across projects and drives every route
// concurrently, catching cross-route replies, loss, serialization, and ID
// restoration regressions.
func TestAllDNSBackendsShareOneFrontendUnderLoad(t *testing.T) {
	type fixture struct {
		name, domain string
		marker       byte
		backend      *net.UDPConn
		close        func()
	}
	fixtures := []fixture{
		{name: "cottendns", domain: "cotten.compat.test", marker: 0xa1},
		{name: "masterdnsvpn", domain: "master.compat.test", marker: 0xa2},
		{name: "stormdns", domain: "storm.compat.test", marker: 0xa3},
		{name: "thefeed", domain: "feed.compat.test", marker: 0xa4},
		{name: "slipgate", domain: "slip.compat.test", marker: 0xa5},
	}
	routes := make([]config.Route, 0, len(fixtures))
	for i := range fixtures {
		fixtures[i].backend, fixtures[i].close = fakeBackend(t, fixtures[i].marker)
		defer fixtures[i].close()
		routes = append(routes, config.Route{Name: fixtures[i].name, Domains: []string{fixtures[i].domain}, Backend: fixtures[i].backend.LocalAddr().String()})
	}
	frontend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{QueryTimeoutMS: 2000, MaxPacketSize: 16 * 1024, MaxPendingPerBackend: 1024, Routes: routes}
	cfg.Limits.UDPWorkers, cfg.Limits.UDPQueue = 32, 2048
	cfg.Limits.GlobalQueriesPerSecond, cfg.Limits.GlobalQueryBurst = 100000, 100000
	cfg.Limits.PerIPQueriesPerSecond, cfg.Limits.PerIPQueryBurst = 100000, 100000
	server, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, frontend) }()

	const perBackend = 150
	started := time.Now()
	errors := make(chan error, len(fixtures))
	var clients sync.WaitGroup
	for _, item := range fixtures {
		item := item
		clients.Add(1)
		go func() {
			defer clients.Done()
			client, err := net.DialUDP("udp", nil, frontend.LocalAddr().(*net.UDPAddr))
			if err != nil {
				errors <- err
				return
			}
			defer client.Close()
			_ = client.SetDeadline(time.Now().Add(10 * time.Second))
			for n := 0; n < perBackend; n++ {
				const sharedID = 0x5353
				if _, err := client.Write(makeQuery(fmt.Sprintf("packet-%d.%s", n, item.domain), sharedID)); err != nil {
					errors <- err
					return
				}
				response := make([]byte, 512)
				size, err := client.Read(response)
				if err != nil {
					errors <- fmt.Errorf("%s response %d: %w", item.name, n, err)
					return
				}
				if binary.BigEndian.Uint16(response[:2]) != sharedID || response[size-1] != item.marker {
					errors <- fmt.Errorf("%s response crossed routes or lost its transaction ID", item.name)
					return
				}
			}
		}()
	}
	clients.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(started)
	queries := perBackend * len(fixtures)
	rate := float64(queries) / elapsed.Seconds()
	t.Logf("lossless five-backend throughput: %.0f queries/s (%d replies in %s)", rate, queries, elapsed.Round(time.Millisecond))
	if rate < 100 {
		t.Fatalf("router throughput regression: %.1f queries/s for %d lossless replies", rate, queries)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUDPMaximumPacketRemainsByteTransparent(t *testing.T) {
	backend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		packet := make([]byte, 16*1024)
		size, peer, readErr := backend.ReadFromUDP(packet)
		if readErr == nil {
			packet[2] |= 0x80
			_, _ = backend.WriteToUDP(packet[:size], peer)
		}
	}()
	frontend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(config.Config{MaxPacketSize: 16 * 1024, Routes: []config.Route{{Name: "large", Domains: []string{"large.compat.test"}, Backend: backend.LocalAddr().String()}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, frontend) }()
	query := makeQuery("large.compat.test", 0x1616)
	query = append(query, make([]byte, 16*1024-len(query))...)
	query[len(query)-1] = 0x7e
	client, err := net.DialUDP("udp", nil, frontend.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := client.Write(query); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 16*1024)
	size, err := client.Read(response)
	if err != nil {
		t.Fatal(err)
	}
	if size != len(query) || response[size-1] != 0x7e || binary.BigEndian.Uint16(response[:2]) != 0x1616 {
		t.Fatalf("16 KiB packet changed: size=%d marker=%x id=%x", size, response[size-1], binary.BigEndian.Uint16(response[:2]))
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
