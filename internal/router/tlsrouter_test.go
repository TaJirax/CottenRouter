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
	"io"
	"log/slog"
	"math/big"
	"net"
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
