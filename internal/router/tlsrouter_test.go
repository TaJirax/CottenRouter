package router

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/TaJirax/CottenRouter/internal/config"
)

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func fakeTLSBackend(t *testing.T, certificate tls.Certificate, marker byte) (net.Listener, func()) {
	t.Helper()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{certificate}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var request [1]byte
				if _, err := io.ReadFull(conn, request[:]); err == nil {
					_, _ = conn.Write([]byte{marker})
				}
			}()
		}
	}()
	return listener, func() { _ = listener.Close(); <-done }
}

func TestTLSRouterPreservesTLSAndRoutesBySNI(t *testing.T) {
	certificate := testCertificate(t)
	first, closeFirst := fakeTLSBackend(t, certificate, 0xe1)
	defer closeFirst()
	second, closeSecond := fakeTLSBackend(t, certificate, 0xe2)
	defer closeSecond()
	frontend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsListener := config.TLSListener{
		Name: "https", Listen: frontend.Addr().String(),
		Routes: []config.TLSRoute{
			{Name: "doh", ServerNames: []string{"doh.example"}, Backend: first.Addr().String()},
			{Name: "stuntls", ServerNames: []string{"stun.example"}, Backend: second.Addr().String()},
		},
	}
	server, err := New(config.Config{TLSListeners: []config.TLSListener{tlsListener}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(ctx, frontend, server.cfg.TLSListeners[0]) }()

	for serverName, marker := range map[string]byte{"doh.example": 0xe1, "stun.example": 0xe2} {
		client, err := tls.Dial("tcp", frontend.Addr().String(), &tls.Config{ServerName: serverName, InsecureSkipVerify: true}) // test certificate
		if err != nil {
			t.Fatalf("dial %s: %v", serverName, err)
		}
		_ = client.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := client.Write([]byte{1}); err != nil {
			t.Fatal(err)
		}
		var response [1]byte
		if _, err := io.ReadFull(client, response[:]); err != nil {
			t.Fatal(err)
		}
		_ = client.Close()
		if response[0] != marker {
			t.Fatalf("SNI %s reached marker %x, want %x", serverName, response[0], marker)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTLSRouterUsesDefaultBackendWhenClientHelloHasNoSNI(t *testing.T) {
	certificate := testCertificate(t)
	backend, closeBackend := fakeTLSBackend(t, certificate, 0xe3)
	defer closeBackend()
	frontend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := config.TLSListener{Name: "slipgate-stuntls", Listen: frontend.Addr().String(), DefaultBackend: backend.Addr().String(), DefaultRouteName: "slipgate:stuntls:test"}
	server, err := New(config.Config{TLSListeners: []config.TLSListener{listener}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(ctx, frontend, server.cfg.TLSListeners[0]) }()

	client, err := tls.Dial("tcp", frontend.Addr().String(), &tls.Config{InsecureSkipVerify: true}) // test certificate; empty ServerName deliberately omits SNI
	if err != nil {
		t.Fatalf("no-SNI handshake did not reach default backend: %v", err)
	}
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	var response [1]byte
	if _, err := io.ReadFull(client, response[:]); err != nil || response[0] != 0xe3 {
		t.Fatalf("default backend response=%x err=%v", response[0], err)
	}
	_ = client.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSharedHTTPSListenerClassifiesDoHNaiveAndStunDefaultIndependently(t *testing.T) {
	server, err := New(config.Config{
		ListenUDP: "127.0.0.1:0", AdminListen: "127.0.0.1:9088",
		Routes: []config.Route{{Name: "dns", Domains: []string{"dns.example"}, Backend: "127.0.0.1:5301", TCPBackend: "disabled"}},
		TLSListeners: []config.TLSListener{{
			Name: "https", Listen: "127.0.0.1:0",
			DefaultBackend: "127.0.0.1:9445", DefaultRouteName: "slipgate:stuntls:ssh",
			Routes: []config.TLSRoute{
				{Name: "cottendns-doh", ServerNames: []string{"doh.example"}, Backend: "127.0.0.1:8443"},
				{Name: "slipgate:naive:web", ServerNames: []string{"naive.example"}, Backend: "127.0.0.1:9444"},
			},
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"doh\x00cottendns-doh":             false,
		"naiveproxy\x00slipgate:naive:web": false,
		"stuntls\x00slipgate:stuntls:ssh":  false,
	}
	for _, metric := range server.stats.Snapshot().Protocols {
		key := metric.Protocol + "\x00" + metric.Route
		if _, exists := want[key]; exists {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("missing independently classified TLS metric %q", key)
		}
	}
}

func TestTLSRouterDefaultBackendPreservesNonTLSStunPayload(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backendDone := make(chan struct{})
	go func() {
		defer close(backendDone)
		conn, acceptErr := backend.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		request := make([]byte, len("CONNECT payload\r\n"))
		if _, readErr := io.ReadFull(conn, request); readErr == nil && string(request) == "CONNECT payload\r\n" {
			_, _ = conn.Write([]byte{0xe4})
		}
	}()
	defer func() { _ = backend.Close(); <-backendDone }()

	frontend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := config.TLSListener{Name: "slipgate-stuntls", Listen: frontend.Addr().String(), DefaultBackend: backend.Addr().String(), DefaultRouteName: "slipgate:stuntls:raw"}
	server, err := New(config.Config{TLSListeners: []config.TLSListener{listener}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(ctx, frontend, server.cfg.TLSListeners[0]) }()

	client, err := net.Dial("tcp", frontend.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write([]byte("CONNECT payload\r\n")); err != nil {
		t.Fatal(err)
	}
	var marker [1]byte
	if _, err := io.ReadFull(client, marker[:]); err != nil || marker[0] != 0xe4 {
		t.Fatalf("raw StunTLS default response=%x err=%v", marker[0], err)
	}
	_ = client.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedProtocolsRemainIsolatedConcurrently(t *testing.T) {
	certificate := testCertificate(t)
	type target struct {
		name, host string
		marker     byte
		backend    net.Listener
		close      func()
	}
	targets := []target{
		{name: "cottendns-doh", host: "doh.compat.test", marker: 0xd1},
		{name: "slipgate-naive", host: "naive.compat.test", marker: 0xd2},
		{name: "slipgate-stuntls", host: "stun.compat.test", marker: 0xd3},
		{name: "cottendns-dot", host: "dot.compat.test", marker: 0xd4},
	}
	for i := range targets {
		targets[i].backend, targets[i].close = fakeTLSBackend(t, certificate, targets[i].marker)
		defer targets[i].close()
	}
	httpsFrontend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dotFrontend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listeners := []config.TLSListener{
		{Name: "https", Listen: httpsFrontend.Addr().String(), Routes: []config.TLSRoute{
			{Name: targets[0].name, ServerNames: []string{targets[0].host}, Backend: targets[0].backend.Addr().String()},
			{Name: targets[1].name, ServerNames: []string{targets[1].host}, Backend: targets[1].backend.Addr().String()},
			{Name: targets[2].name, ServerNames: []string{targets[2].host}, Backend: targets[2].backend.Addr().String()},
		}},
		{Name: "dot", Listen: dotFrontend.Addr().String(), Routes: []config.TLSRoute{
			{Name: targets[3].name, ServerNames: []string{targets[3].host}, Backend: targets[3].backend.Addr().String()},
		}},
	}
	cfg := config.Config{TLSListeners: listeners, MaxTCPConnections: 256}
	cfg.Limits.MaxTCPConnectionsPerIP = 256
	cfg.Limits.MaxTLSConnectionsPerIP = 256
	server, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	go func() { done <- server.ServeTLS(ctx, httpsFrontend, listeners[0]) }()
	go func() { done <- server.ServeTLS(ctx, dotFrontend, listeners[1]) }()

	const sessionsPerProtocol = 30
	var clients sync.WaitGroup
	errors := make(chan error, len(targets)*sessionsPerProtocol)
	for index, item := range targets {
		address := httpsFrontend.Addr().String()
		if index == 3 {
			address = dotFrontend.Addr().String()
		}
		for n := 0; n < sessionsPerProtocol; n++ {
			item, address := item, address
			clients.Add(1)
			go func() {
				defer clients.Done()
				client, err := tls.Dial("tcp", address, &tls.Config{ServerName: item.host, InsecureSkipVerify: true}) // test certificate
				if err != nil {
					errors <- err
					return
				}
				defer client.Close()
				_ = client.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := client.Write([]byte{1}); err != nil {
					errors <- err
					return
				}
				var response [1]byte
				if _, err := io.ReadFull(client, response[:]); err != nil || response[0] != item.marker {
					errors <- fmt.Errorf("%s crossed route: marker=%x err=%v", item.name, response[0], err)
				}
			}()
		}
	}
	clients.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	for range listeners {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestLongLivedRelayCannotConsumeDoHConnectionCapacity(t *testing.T) {
	certificate := testCertificate(t)
	dohBackend, closeDoH := fakeTLSBackend(t, certificate, 0xc1)
	defer closeDoH()
	naiveBackend, closeNaive := fakeTLSBackend(t, certificate, 0xc2)
	defer closeNaive()
	frontend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := config.TLSListener{Name: "https", Listen: frontend.Addr().String(), Routes: []config.TLSRoute{
		{Name: "cottendns-doh", ServerNames: []string{"doh.capacity.test"}, Backend: dohBackend.Addr().String()},
		{Name: "slipgate-naive", ServerNames: []string{"naive.capacity.test"}, Backend: naiveBackend.Addr().String()},
	}}
	cfg := config.Config{TLSListeners: []config.TLSListener{listener}, MaxTLSConnectionsPerProtocol: 1}
	cfg.Limits.MaxTLSConnectionsPerIP = 4
	server, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(ctx, frontend, listener) }()

	naive, err := tls.Dial("tcp", frontend.Addr().String(), &tls.Config{ServerName: "naive.capacity.test", InsecureSkipVerify: true}) // test certificate
	if err != nil {
		t.Fatal(err)
	}
	defer naive.Close()
	dialer := &net.Dialer{Timeout: time.Second}
	secondNaive, secondErr := tls.DialWithDialer(dialer, "tcp", frontend.Addr().String(), &tls.Config{ServerName: "naive.capacity.test", InsecureSkipVerify: true}) // test certificate
	if secondErr == nil {
		_ = secondNaive.Close()
		t.Fatal("second NaiveProxy stream exceeded its protocol capacity")
	}
	// Keep the single NaiveProxy-class stream open. DoH has an independent
	// capacity channel and must still complete its own TLS handshake.
	doh, err := tls.Dial("tcp", frontend.Addr().String(), &tls.Config{ServerName: "doh.capacity.test", InsecureSkipVerify: true}) // test certificate
	if err != nil {
		t.Fatalf("NaiveProxy stream starved DoH capacity: %v", err)
	}
	_ = doh.SetDeadline(time.Now().Add(time.Second))
	if _, err := doh.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	var marker [1]byte
	if _, err := io.ReadFull(doh, marker[:]); err != nil || marker[0] != 0xc1 {
		t.Fatalf("DoH route failed while relay was active: marker=%x err=%v", marker[0], err)
	}
	_ = doh.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
