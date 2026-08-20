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
		MaxPacketSize: 65535, MaxPendingPerBackend: 32, MaxTCPConnections: 16,
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
