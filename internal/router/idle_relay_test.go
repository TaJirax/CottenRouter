package router

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/TaJirax/CottenRouter/internal/config"
)

func TestIdleRelayExpiresQuietAndBlockedConnections(t *testing.T) {
	for _, blockedWrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "quiet-read", true: "blocked-write"}[blockedWrite], func(t *testing.T) {
			client, peer := net.Pipe()
			defer client.Close()
			defer peer.Close()
			backend, remote := net.Pipe()
			defer backend.Close()
			defer remote.Close()
			wrapped, _ := newIdleRelay(client, backend, 50*time.Millisecond)
			finished := make(chan error, 1)
			go func() {
				var err error
				if blockedWrite {
					_, err = wrapped.Write([]byte{1})
				} else {
					_, err = wrapped.Read(make([]byte, 1))
				}
				finished <- err
			}()
			select {
			case err := <-finished:
				var networkError net.Error
				if !errors.As(err, &networkError) || !networkError.Timeout() {
					t.Fatalf("idle operation: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("idle relay retained a blocked operation")
			}
		})
	}
}

func TestIdleRelayOneWayTrafficKeepsBothDirectionsAlive(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	backend, remote := net.Pipe()
	defer backend.Close()
	defer remote.Close()
	upload, download := newIdleRelay(client, backend, 200*time.Millisecond)
	done := make(chan error, 2)
	go func() { _, err := io.Copy(download, upload); done <- err }()
	go func() { _, err := io.Copy(upload, download); done <- err }()
	for i := 0; i < 10; i++ {
		written := make(chan error, 1)
		go func() { _, err := remote.Write([]byte{42}); written <- err }()
		peer.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := io.ReadFull(peer, make([]byte, 1)); err != nil {
			t.Fatal("active download interrupted", err)
		}
		if err := <-written; err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatal("quiet upload side timed out a live download", err)
	default:
	}
	// Once both directions stop, a blocked copy exits and closes its partner.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay did not expire after traffic stopped")
	}
	client.Close()
	backend.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second relay worker remained blocked")
	}
}

func TestTLSIdleExpiryReleasesConnectionSlots(t *testing.T) {
	certificate := testCertificate(t)
	backend, closeBackend := fakeTLSBackend(t, certificate, 0x55)
	defer closeBackend()
	frontend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := config.TLSListener{Name: "https", Listen: frontend.Addr().String(), Routes: []config.TLSRoute{{Name: "doh", ServerNames: []string{"doh.example"}, Backend: backend.Addr().String()}}}
	cfg := config.Config{TLSIdleTimeoutMS: 1000, MaxTLSConnectionsPerProtocol: 1, TLSListeners: []config.TLSListener{listener}}
	cfg.Limits.MaxTLSConnectionsPerIP = 1
	server, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		frontend.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- server.ServeTLS(ctx, frontend, server.cfg.TLSListeners[0]) }()
	defer func() { cancel(); server.Close(); <-stopped; server.wg.Wait() }()
	client, err := tls.Dial("tcp", frontend.Addr().String(), &tls.Config{ServerName: "doh.example", InsecureSkipVerify: true}) // local test certificate
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err = client.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle client was not closed")
	}
	limit := time.Now().Add(time.Second)
	for time.Now().Before(limit) {
		server.connectionsMu.Lock()
		count := len(server.connections)
		server.connectionsMu.Unlock()
		if count == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	server.connectionsMu.Lock()
	count := len(server.connections)
	server.connectionsMu.Unlock()
	if count != 0 {
		t.Fatal("idle connection remained tracked")
	}
	for _, metric := range server.Stats().Protocols {
		if metric.Active != 0 {
			t.Fatalf("idle lease remained active: %+v", metric)
		}
	}
	replacement, err := tls.Dial("tcp", frontend.Addr().String(), &tls.Config{ServerName: "doh.example", InsecureSkipVerify: true}) // local test certificate
	if err != nil {
		t.Fatal("idle expiry did not release per-IP/protocol capacity", err)
	}
	replacement.Close()
}
