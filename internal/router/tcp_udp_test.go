package router

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/TaJirax/CottenRouter/internal/config"
	"github.com/TaJirax/CottenRouter/internal/dnswire"
)

func pendingQueryQuestion(t *testing.T, packet []byte) dnswire.Question {
	t.Helper()
	question, err := dnswire.ParseQuestion(packet)
	if err != nil {
		t.Fatal(err)
	}
	return question
}

func newTCPUDPTestServer(t *testing.T, backend *net.UDPConn, pending int) *Server {
	t.Helper()
	server, err := New(config.Config{
		QueryTimeoutMS: 1000, MaxPendingPerBackend: pending,
		Routes: []config.Route{{Name: "bridge", Domains: []string{"bridge.example"}, Backend: backend.LocalAddr().String(), TCPBackend: "udp"}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(); server.wg.Wait() })
	return server
}

func openTCPUDPTestBackend(t *testing.T) *net.UDPConn {
	t.Helper()
	backend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	_ = backend.SetDeadline(time.Now().Add(3 * time.Second))
	return backend
}

func tcpUDPTestClient(t *testing.T, server *Server) net.Conn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.ServeTCP(context.Background(), listener) }()
	t.Cleanup(func() { _ = server.Close(); <-done })
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	return client
}

func TestTCPUDPBridgePipelinesTransparentReplies(t *testing.T) {
	backend := openTCPUDPTestBackend(t)
	server := newTCPUDPTestServer(t, backend, 8)
	client := tcpUDPTestClient(t, server)
	queries := make([][]byte, 4)
	for i := range queries {
		queries[i] = makeQuery(string(rune('a'+i))+".bridge.example", 0x1234)
		queries[i] = append(queries[i], byte(0xe0+i))
		if err := writeFramedDNS(client, queries[i]); err != nil {
			t.Fatal(err)
		}
	}
	type request struct {
		packet []byte
		peer   *net.UDPAddr
	}
	requests := make([]request, len(queries))
	for i := range requests {
		buffer := make([]byte, 4096)
		n, peer, err := backend.ReadFromUDP(buffer)
		if err != nil {
			t.Fatal(err)
		}
		requests[i] = request{buffer[:n], peer}
	}
	// The upstream deliberately replies out of order; repeated client IDs
	// must not mix the questions or payloads on a persistent TCP connection.
	for i := len(requests) - 1; i >= 0; i-- {
		response := requests[i].packet
		response[2] |= 0x80
		response = append(response, 0xa5, 0x00, 0xff)
		if _, err := backend.WriteToUDP(response, requests[i].peer); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for range queries {
		response, err := readTCPMessage(client, 4096)
		if err != nil {
			t.Fatal(err)
		}
		matched := false
		for _, query := range queries {
			want := append([]byte(nil), query...)
			want[2] |= 0x80
			want = append(want, 0xa5, 0x00, 0xff)
			if bytes.Equal(response, want) {
				seen[string(query)] = true
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("backend bytes changed: %x", response)
		}
	}
	if len(seen) != len(queries) {
		t.Fatalf("responses crossed requests: %d distinct", len(seen))
	}
}

func TestTCPUDPBridgeRejectsForgedRepliesAndSharesPendingCapacity(t *testing.T) {
	backend := openTCPUDPTestBackend(t)
	server := newTCPUDPTestServer(t, backend, 1)
	query := makeQuery("a.bridge.example", 0x8877)
	question := pendingQueryQuestion(t, query)
	type result struct {
		packet []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		packet, err := server.exchangeTCPViaUDP(context.Background(), server.routes[0], query, question)
		done <- result{packet, err}
	}()
	buffer := make([]byte, 4096)
	n, peer, err := backend.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	forward := append([]byte(nil), buffer[:n]...)
	if _, err := server.exchangeTCPViaUDP(context.Background(), server.routes[0], query, question); err == nil {
		t.Fatal("per-backend pending limit ignored")
	}
	connected, err := server.getBackend(backend.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := connected.reserve(pendingQuery{question: question}, 1); ok {
		t.Fatal("UDP ingress did not share TCP pending limit")
	}
	genuine := append([]byte(nil), forward...)
	genuine[2] |= 0x80
	rogue := openTCPUDPTestBackend(t)
	if _, err := rogue.WriteToUDP(genuine, peer); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"direction", "id", "name", "type", "class"} {
		forged := append([]byte(nil), genuine...)
		switch kind {
		case "direction":
			forged[2] &^= 0x80
		case "id":
			forged[0] ^= 0xff
		case "name":
			forged[13] = 'z'
		case "type":
			forged[len(forged)-3] ^= 1
		case "class":
			forged[len(forged)-1] ^= 1
		}
		if _, err := backend.WriteToUDP(forged, peer); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case got := <-done:
		t.Fatalf("forged response consumed mapping: %+v", got)
	case <-time.After(40 * time.Millisecond):
	}
	if _, err := backend.WriteToUDP(genuine, peer); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	want := append([]byte(nil), query...)
	want[2] |= 0x80
	if !bytes.Equal(got.packet, want) {
		t.Fatalf("genuine response changed: %x", got.packet)
	}
}

func TestTCPUDPBridgeCancellationDiscardsLateReplies(t *testing.T) {
	backend := openTCPUDPTestBackend(t)
	server := newTCPUDPTestServer(t, backend, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	query := makeQuery("a.bridge.example", 0x1122)
	question := pendingQueryQuestion(t, query)
	done := make(chan error, 1)
	go func() { _, err := server.exchangeTCPViaUDP(ctx, server.routes[0], query, question); done <- err }()
	buffer := make([]byte, 4096)
	n, peer, err := backend.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	stale := append([]byte(nil), buffer[:n]...)
	stale[2] |= 0x80
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	type result struct {
		packet []byte
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		packet, err := server.exchangeTCPViaUDP(context.Background(), server.routes[0], query, question)
		resultCh <- result{packet, err}
	}()
	n, nextPeer, err := backend.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	fresh := append([]byte(nil), buffer[:n]...)
	fresh[2] |= 0x80
	if binary.BigEndian.Uint16(fresh[:2]) == binary.BigEndian.Uint16(stale[:2]) {
		t.Fatal("canceled transaction ID reused")
	}
	if _, err := backend.WriteToUDP(stale, peer); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resultCh:
		t.Fatalf("late reply satisfied next query: %+v", got)
	case <-time.After(40 * time.Millisecond):
	}
	if _, err := backend.WriteToUDP(fresh, nextPeer); err != nil {
		t.Fatal(err)
	}
	if got := <-resultCh; got.err != nil {
		t.Fatal(got.err)
	}
}

func TestTCPUDPBridgeAnswersVerificationWithoutBackend(t *testing.T) {
	backend := openTCPUDPTestBackend(t)
	server := newTCPUDPTestServer(t, backend, 1)
	key := []byte("0123456789abcdef0123456789abcdef")
	server.routes[0].verifyKey = key
	nonce := []byte("verification0000")
	binary.BigEndian.PutUint16(nonce[14:16], 300)
	mac := hmac.New(sha256.New, key)
	mac.Write(nonce)
	proof := append(append([]byte(nil), nonce...), mac.Sum(nil)[:16]...)
	query := makeQuery(verificationBase32.EncodeToString(proof)+".bridge.example", 0x7788)
	client := tcpUDPTestClient(t, server)
	if err := writeFramedDNS(client, query); err != nil {
		t.Fatal(err)
	}
	response, err := readTCPMessage(client, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(response) < 200 || len(response) > 300 || binary.BigEndian.Uint16(response[:2]) != 0x7788 {
		t.Fatalf("verification response invalid: %x", response)
	}
	question := pendingQueryQuestion(t, query)
	responseMAC := hmac.New(sha256.New, key)
	responseMAC.Write(nonce)
	responseMAC.Write([]byte{1})
	if !hmac.Equal(response[question.End+13:question.End+45], responseMAC.Sum(nil)) {
		t.Fatalf("bad verification MAC: %s", hex.EncodeToString(response))
	}
	server.mu.Lock()
	count := len(server.backends)
	server.mu.Unlock()
	if count != 0 {
		t.Fatal("verification probe contacted UDP backend")
	}
}

func TestTCPUDPBridgeDisconnectCancelsPendingQuery(t *testing.T) {
	backend := openTCPUDPTestBackend(t)
	server := newTCPUDPTestServer(t, backend, 1)
	server.cfg.QueryTimeoutMS = 5000
	client := tcpUDPTestClient(t, server)
	if err := writeFramedDNS(client, makeQuery("a.bridge.example", 0x3344)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 512)
	if _, _, err := backend.ReadFromUDP(buffer); err != nil {
		t.Fatal(err)
	}
	if err := client.(*net.TCPConn).SetLinger(0); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	connected, err := server.getBackend(backend.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		connected.mu.Lock()
		pending := len(connected.pending)
		connected.mu.Unlock()
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("disconnected client retained its pending UDP query")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTCPUDPBridgeDrainsRepliesAfterClientCloseWrite(t *testing.T) {
	backend := openTCPUDPTestBackend(t)
	server := newTCPUDPTestServer(t, backend, 1)
	client := tcpUDPTestClient(t, server)
	query := makeQuery("a.bridge.example", 0x4455)
	if err := writeFramedDNS(client, query); err != nil {
		t.Fatal(err)
	}
	if err := client.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 512)
	n, peer, err := backend.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	buffer[2] |= 0x80
	if _, err := backend.WriteToUDP(buffer[:n], peer); err != nil {
		t.Fatal(err)
	}
	response, err := readTCPMessage(client, 4096)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), query...)
	want[2] |= 0x80
	if !bytes.Equal(response, want) {
		t.Fatalf("half-closed client's queued response changed: %x", response)
	}
}

func TestTCPUDPBridgeTimeoutAndShutdownReleaseSessions(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "shutdown"}[shutdown], func(t *testing.T) {
			backend := openTCPUDPTestBackend(t)
			server := newTCPUDPTestServer(t, backend, 1)
			if !shutdown {
				server.cfg.QueryTimeoutMS = 100
			}
			query := makeQuery("a.bridge.example", 0x5566)
			question := pendingQueryQuestion(t, query)
			done := make(chan error, 1)
			go func() {
				_, err := server.exchangeTCPViaUDP(context.Background(), server.routes[0], query, question)
				done <- err
			}()
			buffer := make([]byte, 512)
			if _, _, err := backend.ReadFromUDP(buffer); err != nil {
				t.Fatal(err)
			}
			if shutdown {
				_ = server.Close()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("unanswered query succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("pending query did not stop")
			}
			for _, metric := range server.stats.Snapshot().Protocols {
				if metric.Protocol == "dns/tcp" && metric.Route == "bridge" && (metric.Active != 0 || metric.Sessions != 1) {
					t.Fatalf("pending query session accounting leaked: %+v", metric)
				}
			}
		})
	}
}
