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
	"github.com/TaJirax/CottenRouter/internal/dnswire"
)

func makeQuery(name string, id uint16) []byte {
	packet := make([]byte, 12)
	binary.BigEndian.PutUint16(packet[:2], id)
	binary.BigEndian.PutUint16(packet[2:4], 0x0100)
	binary.BigEndian.PutUint16(packet[4:6], 1)
	start := 0
	for i := 0; i <= len(name); i++ {
		if i == len(name) || name[i] == '.' {
			packet = append(packet, byte(i-start))
			packet = append(packet, name[start:i]...)
			start = i + 1
		}
	}
	return append(packet, 0, 0, 16, 0, 1)
}

// makeReply builds the reply a real resolver would send: the same question
// echoed back with QR set, plus a trailing marker byte the test can identify.
func makeReply(name string, id uint16, marker byte) []byte {
	packet := makeQuery(name, id)
	binary.BigEndian.PutUint16(packet[2:4], 0x8180)
	return append(packet, marker)
}

func fakeBackend(t *testing.T, marker byte) (*net.UDPConn, func()) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 4096)
		for {
			n, peer, err := conn.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			response := append([]byte(nil), buffer[:n]...)
			response[2] |= 0x80
			response = append(response, marker)
			_, _ = conn.WriteToUDP(response, peer)
		}
	}()
	return conn, func() { _ = conn.Close(); <-done }
}

func fakeTCPBackend(t *testing.T, marker byte) (net.Listener, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var connections sync.WaitGroup
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer conn.Close()
				packet, err := readTCPMessage(conn, 65535)
				if err != nil {
					return
				}
				packet[2] |= 0x80
				packet = append(packet, marker)
				_ = writeFramedDNS(conn, packet)
			}()
		}
	}()
	return listener, func() { _ = listener.Close(); <-done; connections.Wait() }
}

func TestServerRoutesTwoBackendsAndRestoresTransactionID(t *testing.T) {
	first, closeFirst := fakeBackend(t, 0xa1)
	defer closeFirst()
	second, closeSecond := fakeBackend(t, 0xb2)
	defer closeSecond()

	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenUDP: "127.0.0.1:0", QueryTimeoutMS: 1000, MaxPacketSize: 4096,
		MaxPendingPerBackend: 32, UnmatchedAction: "refused",
		Routes: []config.Route{
			{Name: "first", Domains: []string{"one.example"}, Backend: first.LocalAddr().String()},
			{Name: "second", Domains: []string{"two.example"}, Backend: second.LocalAddr().String()},
		},
	}
	server, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()

	client, err := net.DialUDP("udp", nil, listener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	const sharedID = 0x1234
	if _, err := client.Write(makeQuery("data.one.example", sharedID)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write(makeQuery("data.two.example", sharedID)); err != nil {
		t.Fatal(err)
	}

	markers := map[byte]bool{}
	for i := 0; i < 2; i++ {
		buffer := make([]byte, 4096)
		n, err := client.Read(buffer)
		if err != nil {
			t.Fatal(err)
		}
		if binary.BigEndian.Uint16(buffer[:2]) != sharedID {
			t.Fatalf("client transaction ID was not restored")
		}
		markers[buffer[n-1]] = true
	}
	if !markers[0xa1] || !markers[0xb2] {
		t.Fatalf("responses came from wrong backends: %v", markers)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestServerRefusesUnknownDomain(t *testing.T) {
	backend, closeBackend := fakeBackend(t, 1)
	defer closeBackend()
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{ListenUDP: "127.0.0.1:0", QueryTimeoutMS: 1000, MaxPacketSize: 4096, MaxPendingPerBackend: 8, UnmatchedAction: "refused", Routes: []config.Route{{Name: "known", Domains: []string{"known.example"}, Backend: backend.LocalAddr().String()}}}
	server, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	client, err := net.DialUDP("udp", nil, listener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(time.Second))
	if _, err := client.Write(makeQuery("unknown.example", 9)); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 512)
	n, err := client.Read(response)
	if err != nil {
		t.Fatal(err)
	}
	if n < 12 || binary.BigEndian.Uint16(response[2:4])&0xf != 5 {
		t.Fatal("expected REFUSED")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServerRoutesThreeSimilarUDPBackendsConcurrently(t *testing.T) {
	type backendFixture struct {
		domain string
		marker byte
		conn   *net.UDPConn
		close  func()
	}
	fixtures := []backendFixture{{domain: "cotten.example", marker: 0xc1}, {domain: "master.example", marker: 0xc2}, {domain: "storm.example", marker: 0xc3}}
	for i := range fixtures {
		fixtures[i].conn, fixtures[i].close = fakeBackend(t, fixtures[i].marker)
		defer fixtures[i].close()
	}
	routes := make([]config.Route, 0, len(fixtures))
	for i, fixture := range fixtures {
		routes = append(routes, config.Route{Name: fmt.Sprintf("backend-%d", i), Domains: []string{fixture.domain}, Backend: fixture.conn.LocalAddr().String()})
	}
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(config.Config{ListenUDP: "127.0.0.1:0", QueryTimeoutMS: 2000, MaxPacketSize: 4096, MaxPendingPerBackend: 256, Routes: routes}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	client, err := net.DialUDP("udp", nil, listener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))

	const perBackend = 40
	const sharedID = 0x5151
	for i := 0; i < perBackend; i++ {
		for _, fixture := range fixtures {
			if _, err := client.Write(makeQuery(fmt.Sprintf("packet-%d.%s", i, fixture.domain), sharedID)); err != nil {
				t.Fatal(err)
			}
		}
	}
	counts := map[byte]int{}
	for i := 0; i < perBackend*len(fixtures); i++ {
		buffer := make([]byte, 4096)
		n, err := client.Read(buffer)
		if err != nil {
			t.Fatalf("response %d: %v", i, err)
		}
		if binary.BigEndian.Uint16(buffer[:2]) != sharedID {
			t.Fatalf("response %d has transaction ID %x", i, binary.BigEndian.Uint16(buffer[:2]))
		}
		counts[buffer[n-1]]++
	}
	for _, fixture := range fixtures {
		if counts[fixture.marker] != perBackend {
			t.Fatalf("marker %x count=%d, want %d (all=%v)", fixture.marker, counts[fixture.marker], perBackend, counts)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServerRoutesPipelinedTCPQueriesByDomain(t *testing.T) {
	first, closeFirst := fakeTCPBackend(t, 0xd1)
	defer closeFirst()
	second, closeSecond := fakeTCPBackend(t, 0xd2)
	defer closeSecond()
	frontend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ListenUDP: "127.0.0.1:0", ListenTCP: frontend.Addr().String(), QueryTimeoutMS: 2000,
		MaxPacketSize: 16384, MaxPendingPerBackend: 32, MaxTCPConnections: 16,
		Routes: []config.Route{
			{Name: "first", Domains: []string{"tcp-one.example"}, Backend: first.Addr().String(), TCPBackend: first.Addr().String()},
			{Name: "second", Domains: []string{"tcp-two.example"}, Backend: second.Addr().String(), TCPBackend: second.Addr().String()},
		},
	}
	server, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.ServeTCP(ctx, frontend) }()
	client, err := net.Dial("tcp", frontend.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	if err := writeFramedDNS(client, makeQuery("x.tcp-one.example", 0x1010)); err != nil {
		t.Fatal(err)
	}
	if err := writeFramedDNS(client, makeQuery("x.tcp-two.example", 0x2020)); err != nil {
		t.Fatal(err)
	}
	seen := map[byte]uint16{}
	for i := 0; i < 2; i++ {
		response, err := readTCPMessage(client, 65535)
		if err != nil {
			t.Fatal(err)
		}
		seen[response[len(response)-1]] = binary.BigEndian.Uint16(response[:2])
	}
	if seen[0xd1] != 0x1010 || seen[0xd2] != 0x2020 {
		t.Fatalf("TCP responses crossed routes: %v", seen)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServerEnforcesUDPSourceBurst(t *testing.T) {
	backend, closeBackend := fakeBackend(t, 0xf1)
	defer closeBackend()
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		QueryTimeoutMS: 500, Routes: []config.Route{{Name: "limited", Domains: []string{"limited.example"}, Backend: backend.LocalAddr().String()}},
	}
	cfg.Limits.PerIPQueriesPerSecond = 1
	cfg.Limits.PerIPQueryBurst = 2
	server, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	client, err := net.DialUDP("udp", nil, listener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for i := 0; i < 20; i++ {
		_, _ = client.Write(makeQuery("x.limited.example", uint16(i)))
	}
	_ = client.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	responses := 0
	buffer := make([]byte, 4096)
	for {
		if _, err := client.Read(buffer); err != nil {
			break
		}
		responses++
	}
	if responses != 2 {
		t.Fatalf("received %d responses, want exactly burst of 2", responses)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServerEnforcesUDPIngressByteBurst(t *testing.T) {
	backend, closeBackend := fakeBackend(t, 0xf2)
	defer closeBackend()
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	query := makeQuery("x.ingress.example", 1)
	cfg := config.Config{
		QueryTimeoutMS: 500,
		Routes:         []config.Route{{Name: "ingress", Domains: []string{"ingress.example"}, Backend: backend.LocalAddr().String()}},
	}
	cfg.Limits.IngressBytesPerSecond = 1
	cfg.Limits.IngressBurstBytes = len(query) * 2
	server, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	client, err := net.DialUDP("udp", nil, listener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for i := 0; i < 3; i++ {
		packet := append([]byte(nil), query...)
		binary.BigEndian.PutUint16(packet[:2], uint16(i))
		if _, err := client.Write(packet); err != nil {
			t.Fatal(err)
		}
	}
	_ = client.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	responses := 0
	buffer := make([]byte, 4096)
	for {
		if _, err := client.Read(buffer); err != nil {
			break
		}
		responses++
	}
	if responses != 2 {
		t.Fatalf("received %d responses, want exactly byte burst of 2 packets", responses)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServerEnforcesTCPIngressByteBurst(t *testing.T) {
	backend, closeBackend := fakeTCPBackend(t, 0xf3)
	defer closeBackend()
	frontend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	query := makeQuery("x.tcp-ingress.example", 1)
	cfg := config.Config{
		ListenTCP: frontend.Addr().String(), QueryTimeoutMS: 500,
		Routes: []config.Route{{Name: "tcp-ingress", Domains: []string{"tcp-ingress.example"}, Backend: "127.0.0.1:5399", TCPBackend: backend.Addr().String()}},
	}
	cfg.Limits.IngressBytesPerSecond = 1
	cfg.Limits.IngressBurstBytes = len(query) + 2
	server, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.ServeTCP(ctx, frontend) }()
	client, err := net.Dial("tcp", frontend.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(time.Second))
	if err := writeFramedDNS(client, query); err != nil {
		t.Fatal(err)
	}
	if _, err := readTCPMessage(client, 65535); err != nil {
		t.Fatalf("first query within ingress budget failed: %v", err)
	}
	if err := writeFramedDNS(client, query); err != nil {
		t.Fatal(err)
	}
	if _, err := readTCPMessage(client, 65535); err == nil {
		t.Fatal("second query exceeded ingress byte budget but received a response")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServerDropsOversizedClientDatagramWithoutPoisoningSocket(t *testing.T) {
	backend, closeBackend := fakeBackend(t, 0xf4)
	defer closeBackend()
	frontend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(config.Config{MaxPacketSize: 512, QueryTimeoutMS: 500, Routes: []config.Route{{Name: "sized", Domains: []string{"sized.example"}, Backend: backend.LocalAddr().String()}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, frontend) }()
	client, err := net.DialUDP("udp", nil, frontend.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	oversized := makeQuery("sized.example", 1)
	oversized = append(oversized, make([]byte, 513-len(oversized))...)
	if _, err := client.Write(oversized); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, err := client.Read(make([]byte, 1024)); err == nil {
		t.Fatal("oversized datagram was forwarded")
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Write(makeQuery("sized.example", 2)); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 1024)
	if n, err := client.Read(response); err != nil || n == 0 || response[n-1] != 0xf4 {
		t.Fatalf("valid datagram after oversized packet failed: n=%d err=%v", n, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServerDropsOversizedBackendDatagram(t *testing.T) {
	backend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		buffer := make([]byte, 1024)
		for count := 0; count < 2; count++ {
			n, peer, readErr := backend.ReadFromUDP(buffer)
			if readErr != nil {
				return
			}
			response := append([]byte(nil), buffer[:n]...)
			response[2] |= 0x80
			if count == 0 {
				response = append(response, make([]byte, 513-len(response))...)
			} else {
				response = append(response, 0xf5)
			}
			_, _ = backend.WriteToUDP(response, peer)
		}
	}()
	frontend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(config.Config{MaxPacketSize: 512, QueryTimeoutMS: 500, Routes: []config.Route{{Name: "backend-sized", Domains: []string{"backend-sized.example"}, Backend: backend.LocalAddr().String()}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, frontend) }()
	client, err := net.DialUDP("udp", nil, frontend.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	_, _ = client.Write(makeQuery("backend-sized.example", 1))
	if _, err := client.Read(make([]byte, 1024)); err == nil {
		t.Fatal("oversized backend response reached client")
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = client.Write(makeQuery("backend-sized.example", 2))
	response := make([]byte, 1024)
	if n, err := client.Read(response); err != nil || n == 0 || response[n-1] != 0xf5 {
		t.Fatalf("valid backend response after oversized packet failed: n=%d err=%v", n, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServerRecreatesFailedUDPBackendConnection(t *testing.T) {
	backend, closeBackend := fakeBackend(t, 0xf6)
	defer closeBackend()
	frontend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(config.Config{QueryTimeoutMS: 500, Routes: []config.Route{{Name: "restart", Domains: []string{"restart.example"}, Backend: backend.LocalAddr().String()}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, frontend) }()
	client, err := net.DialUDP("udp", nil, frontend.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	query := func(id uint16) {
		t.Helper()
		if _, err := client.Write(makeQuery("restart.example", id)); err != nil {
			t.Fatal(err)
		}
		response := make([]byte, 1024)
		if n, err := client.Read(response); err != nil || n == 0 || response[n-1] != 0xf6 {
			t.Fatalf("query %d failed: n=%d err=%v", id, n, err)
		}
	}
	query(1)
	server.mu.Lock()
	failed := server.backends[backend.LocalAddr().String()]
	server.mu.Unlock()
	if failed == nil {
		t.Fatal("router did not create backend connection")
	}
	_ = failed.conn.Close()
	deadline := time.Now().Add(time.Second)
	for {
		server.mu.Lock()
		current := server.backends[backend.LocalAddr().String()]
		server.mu.Unlock()
		if current == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed backend connection remained cached")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	query(2)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUDPBackendWriteErrorEvictsCachedSocket(t *testing.T) {
	remote, err := net.ResolveUDPAddr("udp", "127.0.0.1:5398")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	server, err := New(config.Config{Routes: []config.Route{{Name: "write-failure", Domains: []string{"write-failure.example"}, Backend: remote.String()}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	failed := &backendConn{address: remote.String(), conn: conn, pending: make(map[uint16]pendingQuery), done: make(chan struct{})}
	server.backends[remote.String()] = failed
	server.handleQuery(makeQuery("write-failure.example", 7), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5353})
	server.mu.Lock()
	cached := server.backends[remote.String()]
	server.mu.Unlock()
	if cached != nil {
		t.Fatal("UDP write error left failed backend socket cached")
	}
}

func TestUDPBackendIDWrapUsesNewSocketAndRejectsLateDuplicate(t *testing.T) {
	upstream, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	frontend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	server, err := New(config.Config{QueryTimeoutMS: 1000, MaxPendingPerBackend: 8, Routes: []config.Route{{Name: "generation", Domains: []string{"generation.example"}, Backend: upstream.LocalAddr().String()}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server.listenersMu.Lock()
	server.listener = frontend
	server.listenersMu.Unlock()
	defer func() {
		_ = server.Close()
		server.wg.Wait()
	}()

	old, err := server.getBackend(upstream.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	clientAddr := client.LocalAddr().(*net.UDPAddr)
	generationQuestion := dnswire.Question{Name: "generation.example", Type: 16, Class: 1}
	// Complete 65,536 logical queries while keeping the pending set small. The
	// next query must rotate sockets instead of reusing ID zero on this socket.
	for queryNumber := 0; queryNumber < 1<<16; queryNumber++ {
		id, ok, exhausted := old.reserve(pendingQuery{clientAddr: clientAddr, clientID: uint16(queryNumber), expiresAt: time.Now().Add(time.Second), route: "generation", question: generationQuestion}, 8)
		if !ok || exhausted || id != uint16(queryNumber) {
			t.Fatalf("query %d reservation: id=%d ok=%t exhausted=%t", queryNumber, id, ok, exhausted)
		}
		if _, ok := old.take(id, generationQuestion); !ok {
			t.Fatalf("query %d could not be completed", queryNumber)
		}
	}
	if _, ok, exhausted := old.reserve(pendingQuery{}, 8); ok || !exhausted {
		t.Fatalf("query 65537 did not exhaust the first ID namespace: ok=%t exhausted=%t", ok, exhausted)
	}

	replacement, err := server.rotateBackend(upstream.LocalAddr().String(), old)
	if err != nil {
		t.Fatal(err)
	}
	if replacement == old || replacement.conn.LocalAddr().String() == old.conn.LocalAddr().String() {
		t.Fatal("ID wrap did not move to a distinct connected UDP socket")
	}
	newID, ok, exhausted := replacement.reserve(pendingQuery{clientAddr: clientAddr, clientID: 0x7777, expiresAt: time.Now().Add(time.Second), route: "generation", question: generationQuestion}, 8)
	if !ok || exhausted || newID != 0 {
		t.Fatalf("replacement reservation: id=%d ok=%t exhausted=%t", newID, ok, exhausted)
	}
	server.stats.SessionOpen("dns/udp", "generation")

	late := makeReply("generation.example", 0, 0xee)
	if _, err := upstream.WriteToUDP(late, old.conn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 512)
	_ = client.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, _, err := client.ReadFromUDP(buffer); err == nil {
		t.Fatalf("late duplicate from retired generation reached client (%d bytes)", n)
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("waiting for late duplicate: %v", err)
	}
	replacement.mu.Lock()
	_, stillPending := replacement.pending[newID]
	replacement.mu.Unlock()
	if !stillPending {
		t.Fatal("late duplicate removed the new generation's pending query")
	}

	correct := makeReply("generation.example", 0, 0xaa)
	if _, err := upstream.WriteToUDP(correct, replacement.conn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := client.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(correct) || binary.BigEndian.Uint16(buffer[:2]) != 0x7777 || buffer[n-1] != 0xaa {
		t.Fatalf("replacement response crossed generations: n=%d id=%x marker=%x", n, binary.BigEndian.Uint16(buffer[:2]), buffer[n-1])
	}
}

func TestUDPSessionOpensBeforePendingQueryBecomesVisible(t *testing.T) {
	upstream, closeUpstream := fakeBackend(t, 0xab)
	defer closeUpstream()
	frontend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(config.Config{QueryTimeoutMS: 1000, MaxPendingPerBackend: 8, Routes: []config.Route{{Name: "telemetry", Domains: []string{"telemetry.example"}, Backend: upstream.LocalAddr().String()}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server.listenersMu.Lock()
	server.listener = frontend
	server.listenersMu.Unlock()
	defer func() {
		_ = server.Close()
		server.wg.Wait()
	}()

	backend, err := server.getBackend(upstream.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	locked := true
	defer func() {
		if locked {
			backend.mu.Unlock()
		}
	}()
	done := make(chan struct{})
	go func() {
		server.handleQuery(makeQuery("telemetry.example", 0x1212), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5353})
		close(done)
	}()

	activeForRoute := func() (int64, uint64) {
		for _, metric := range server.stats.Snapshot().Protocols {
			if metric.Protocol == "dns/udp" && metric.Route == "telemetry" {
				return metric.Active, metric.Sessions
			}
		}
		return 0, 0
	}
	deadline := time.Now().Add(time.Second)
	for {
		active, sessions := activeForRoute()
		if active == 1 && sessions == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session was not opened before reserve published the pending query: active=%d sessions=%d", active, sessions)
		}
		time.Sleep(time.Millisecond)
	}
	backend.mu.Unlock()
	locked = false
	<-done

	deadline = time.Now().Add(time.Second)
	for {
		active, sessions := activeForRoute()
		if active == 0 && sessions == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("completed response left phantom telemetry: active=%d sessions=%d", active, sessions)
		}
		time.Sleep(time.Millisecond)
	}
}

// A reply carrying the right transaction ID but a different question is a
// cache-poisoning attempt. It must neither reach the client nor consume the
// mapping the genuine reply still needs.
func TestUDPReplyWithWrongQuestionDoesNotConsumeMapping(t *testing.T) {
	upstream, closeUpstream := fakeBackend(t, 0xab)
	defer closeUpstream()
	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	frontend, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(config.Config{QueryTimeoutMS: 2000, MaxPendingPerBackend: 8, Routes: []config.Route{{Name: "poison", Domains: []string{"poison.example"}, Backend: upstream.LocalAddr().String()}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server.listenersMu.Lock()
	server.listener = frontend
	server.listenersMu.Unlock()
	defer func() {
		_ = server.Close()
		server.wg.Wait()
	}()

	backend, err := server.getBackend(upstream.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	question := dnswire.Question{Name: "poison.example", Type: 16, Class: 1}
	id, ok, _ := backend.reserve(pendingQuery{clientAddr: client.LocalAddr().(*net.UDPAddr), clientID: 0x1234, expiresAt: time.Now().Add(2 * time.Second), route: "poison", question: question}, 8)
	if !ok {
		t.Fatal("could not reserve the pending query")
	}
	server.stats.SessionOpen("dns/udp", "poison")

	forged := makeReply("attacker.example", id, 0xee)
	if _, err := upstream.WriteToUDP(forged, backend.conn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 512)
	_ = client.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if n, _, err := client.ReadFromUDP(buffer); err == nil {
		t.Fatalf("forged reply reached the client (%d bytes)", n)
	}
	backend.mu.Lock()
	_, stillPending := backend.pending[id]
	backend.mu.Unlock()
	if !stillPending {
		t.Fatal("forged reply consumed the pending mapping")
	}

	genuine := makeReply("poison.example", id, 0xaa)
	if _, err := upstream.WriteToUDP(genuine, backend.conn.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := client.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint16(buffer[:2]) != 0x1234 || buffer[n-1] != 0xaa {
		t.Fatalf("genuine reply was not delivered: id=%x marker=%x", binary.BigEndian.Uint16(buffer[:2]), buffer[n-1])
	}
}
