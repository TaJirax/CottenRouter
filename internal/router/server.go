package router

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/TaJirax/CottenRouter/internal/config"
	"github.com/TaJirax/CottenRouter/internal/dnswire"
	"github.com/TaJirax/CottenRouter/internal/guard"
	"github.com/TaJirax/CottenRouter/internal/telemetry"
)

type Server struct {
	cfg           config.Config
	routes        routeTable
	logger        *slog.Logger
	listener      *net.UDPConn
	tcpListener   net.Listener
	tlsListeners  []net.Listener
	adminListener net.Listener
	adminServer   *http.Server
	done          chan struct{}
	guard         *guard.Limiter
	stats         *telemetry.Registry

	mu              sync.Mutex
	backends        map[string]*backendConn
	wg              sync.WaitGroup
	closeOnce       sync.Once
	maintenanceOnce sync.Once
	packetPool      sync.Pool
	tcpCapacity     chan struct{}
	listenersMu     sync.Mutex
	connectionsMu   sync.Mutex
	connections     map[net.Conn]struct{}
}

type backendConn struct {
	address string
	conn    *net.UDPConn
	mu      sync.Mutex
	pending map[uint16]pendingQuery
	nextID  uint16
}

type pendingQuery struct {
	clientAddr *net.UDPAddr
	clientID   uint16
	expiresAt  time.Time
	route      string
}

func New(cfg config.Config, logger *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	table, err := newRouteTable(cfg.Routes)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	server := &Server{
		cfg: cfg, routes: table, logger: logger,
		done: make(chan struct{}), guard: guard.New(cfg.Limits), stats: telemetry.New(), backends: make(map[string]*backendConn),
		tcpCapacity: make(chan struct{}, cfg.MaxTCPConnections), connections: make(map[net.Conn]struct{}),
	}
	server.packetPool.New = func() any { return make([]byte, cfg.MaxPacketSize) }
	for _, route := range cfg.Routes {
		server.stats.Ensure("dns/udp", route.Name)
		if route.TCPBackend != "" && route.TCPBackend != "disabled" {
			server.stats.Ensure("dns/tcp", route.Name)
		}
	}
	for _, listener := range cfg.TLSListeners {
		for _, route := range listener.Routes {
			server.stats.Ensure(classifyTLSProtocol(listener.Name, route.Name), route.Name)
		}
	}
	return server, nil
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp", s.cfg.ListenUDP)
	if err != nil {
		return err
	}
	listener, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.ListenUDP, err)
	}
	s.listenersMu.Lock()
	s.listener = listener
	s.listenersMu.Unlock()
	streamErrors := make(chan error, 2+len(s.cfg.TLSListeners))
	streamCount := 0
	openedTLS := make([]net.Listener, 0, len(s.cfg.TLSListeners))
	adminListener, err := net.Listen("tcp", s.cfg.AdminListen)
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("listen on admin %s: %w", s.cfg.AdminListen, err)
	}
	s.listenersMu.Lock()
	s.adminListener = adminListener
	s.listenersMu.Unlock()
	streamCount++
	go func() {
		err := s.ServeAdmin(ctx, adminListener)
		if err != nil {
			s.Close()
		}
		streamErrors <- err
	}()
	if s.cfg.ListenTCP != "" {
		tcpListener, err := net.Listen("tcp", s.cfg.ListenTCP)
		if err != nil {
			_ = listener.Close()
			s.Close()
			return fmt.Errorf("listen on TCP %s: %w", s.cfg.ListenTCP, err)
		}
		s.listenersMu.Lock()
		s.tcpListener = tcpListener
		s.listenersMu.Unlock()
		streamCount++
		go func() {
			err := s.ServeTCP(ctx, tcpListener)
			if err != nil {
				s.Close()
			}
			streamErrors <- err
		}()
	}
	for _, tlsConfig := range s.cfg.TLSListeners {
		tlsListener, err := net.Listen("tcp", tlsConfig.Listen)
		if err != nil {
			_ = listener.Close()
			for _, opened := range openedTLS {
				_ = opened.Close()
			}
			s.Close()
			return fmt.Errorf("listen on TLS %s (%s): %w", tlsConfig.Name, tlsConfig.Listen, err)
		}
		openedTLS = append(openedTLS, tlsListener)
		streamCount++
		go func() {
			err := s.ServeTLS(ctx, tlsListener, tlsConfig)
			if err != nil {
				s.Close()
			}
			streamErrors <- err
		}()
	}
	udpErr := s.Serve(ctx, listener)
	s.Close()
	for i := 0; i < streamCount; i++ {
		if streamErr := <-streamErrors; udpErr == nil && streamErr != nil {
			udpErr = streamErr
		}
	}
	return udpErr
}

// Serve runs on an already-open UDP socket. It is exported to support socket
// activation and deterministic local tests.
func (s *Server) Serve(ctx context.Context, listener *net.UDPConn) error {
	s.listenersMu.Lock()
	s.listener = listener
	s.listenersMu.Unlock()
	s.logger.Info("CottenRouter listening", "network", "udp", "address", listener.LocalAddr())
	s.startMaintenance()

	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-stop:
		}
	}()
	defer close(stop)

	type datagram struct {
		packet []byte
		client *net.UDPAddr
	}
	queue := make(chan datagram, s.cfg.Limits.UDPQueue)
	for i := 0; i < s.cfg.Limits.UDPWorkers; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for item := range queue {
				s.handleQuery(item.packet, item.client)
				s.packetPool.Put(item.packet[:cap(item.packet)])
			}
		}()
	}
	finish := func() {
		close(queue)
		s.Close()
		s.wg.Wait()
	}

	for {
		packet := s.packetPool.Get().([]byte)
		n, clientAddr, err := listener.ReadFromUDP(packet)
		if err != nil {
			s.packetPool.Put(packet)
			finish()
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("read client packet: %w", err)
		}
		packet = packet[:n]
		if !s.guard.AllowQuery(ipFromUDP(clientAddr)) {
			s.stats.Drop(true)
			s.packetPool.Put(packet[:cap(packet)])
			continue
		}
		select {
		case queue <- datagram{packet: packet, client: clientAddr}:
		default:
			// Drop on overload. The queue and worker count are fixed, so an
			// attacker cannot turn packets into unbounded goroutines or memory.
			s.stats.Drop(false)
			s.packetPool.Put(packet[:cap(packet)])
		}
	}
}

func (s *Server) Addr() net.Addr {
	s.listenersMu.Lock()
	defer s.listenersMu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.LocalAddr()
}

func (s *Server) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		close(s.done)
		s.listenersMu.Lock()
		if s.listener != nil {
			closeErr = s.listener.Close()
		}
		if s.tcpListener != nil {
			if err := s.tcpListener.Close(); closeErr == nil {
				closeErr = err
			}
		}
		for _, listener := range s.tlsListeners {
			if err := listener.Close(); closeErr == nil {
				closeErr = err
			}
		}
		if s.adminServer != nil {
			if err := s.adminServer.Close(); closeErr == nil {
				closeErr = err
			}
		} else if s.adminListener != nil {
			if err := s.adminListener.Close(); closeErr == nil {
				closeErr = err
			}
		}
		s.listenersMu.Unlock()
		s.connectionsMu.Lock()
		for conn := range s.connections {
			_ = conn.Close()
		}
		s.connectionsMu.Unlock()
		s.mu.Lock()
		for _, backend := range s.backends {
			_ = backend.conn.Close()
		}
		s.mu.Unlock()
	})
	return closeErr
}

// ServeTCP accepts RFC 1035 two-byte-length-prefixed DNS messages. Each query
// is routed independently, so one persistent client connection can use more
// than one configured suffix without being pinned to the first backend.
func (s *Server) ServeTCP(ctx context.Context, listener net.Listener) error {
	s.listenersMu.Lock()
	s.tcpListener = listener
	s.listenersMu.Unlock()
	s.logger.Info("CottenRouter listening", "network", "tcp", "address", listener.Addr())
	s.startMaintenance()
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-stop:
		}
	}()
	defer close(stop)
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
			}
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept TCP DNS client: %w", err)
		}
		clientIP, ok := s.acquireTCPClient(conn)
		if !ok {
			_ = conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.releaseTCPClient(conn, clientIP)
			s.stats.SessionOpen("dns/tcp", "")
			defer s.stats.SessionClose("dns/tcp", "")
			s.handleTCPConnection(conn)
		}()
	}
}

func (s *Server) handleTCPConnection(client net.Conn) {
	defer client.Close()
	var writeMu sync.Mutex
	var queries sync.WaitGroup
	inflight := make(chan struct{}, s.cfg.Limits.MaxTCPInflightPerConnection)
	defer queries.Wait()
	clientIP := ipFromAddr(client.RemoteAddr())
	for queryCount := 0; queryCount < s.cfg.Limits.MaxTCPQueriesPerConnection; queryCount++ {
		_ = client.SetReadDeadline(time.Now().Add(s.cfg.QueryTimeout()))
		packet, err := readTCPMessage(client, s.cfg.MaxTCPMessageSize)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.logger.Debug("TCP client read stopped", "client", client.RemoteAddr(), "error", err)
			}
			return
		}
		if !s.guard.AllowQuery(clientIP) {
			s.stats.Drop(true)
			return
		}
		inflight <- struct{}{}
		queries.Add(1)
		go func() {
			defer queries.Done()
			defer func() { <-inflight }()
			s.handleTCPQuery(client, &writeMu, packet)
		}()
	}
}

func (s *Server) handleTCPQuery(client net.Conn, writeMu *sync.Mutex, packet []byte) {
	qname, err := dnswire.QuestionName(packet)
	if err != nil {
		return
	}
	route, ok := s.routes.match(qname)
	if !ok {
		if s.cfg.UnmatchedAction == "refused" {
			s.writeTCPMessage(client, writeMu, dnswire.RefusedResponse(packet), s.cfg.QueryTimeout())
		}
		return
	}
	s.stats.Query("dns/tcp", route.name, len(packet)+2)
	if route.tcpBackend == "disabled" {
		s.writeTCPMessage(client, writeMu, dnswire.ErrorResponse(packet, 2), s.cfg.QueryTimeout())
		return
	}
	backend, err := net.DialTimeout("tcp", route.tcpBackend, s.cfg.QueryTimeout())
	if err != nil {
		s.logger.Debug("TCP backend unavailable", "route", route.name, "backend", route.tcpBackend, "error", err)
		s.writeTCPMessage(client, writeMu, dnswire.ErrorResponse(packet, 2), s.cfg.QueryTimeout())
		return
	}
	defer backend.Close()
	_ = backend.SetDeadline(time.Now().Add(s.cfg.QueryTimeout()))
	if err := writeFramedDNS(backend, packet); err != nil {
		return
	}
	response, err := readTCPMessage(backend, s.cfg.MaxTCPMessageSize)
	if err != nil {
		s.stats.Error("dns/tcp", route.name)
		return
	}
	s.stats.Out("dns/tcp", route.name, len(response)+2)
	s.writeTCPMessage(client, writeMu, response, s.cfg.QueryTimeout())
}

func readTCPMessage(reader io.Reader, maxSize int) ([]byte, error) {
	var prefix [2]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return nil, err
	}
	size := int(binary.BigEndian.Uint16(prefix[:]))
	if size < 12 || size > maxSize {
		return nil, fmt.Errorf("invalid TCP DNS message size %d", size)
	}
	packet := make([]byte, size)
	_, err := io.ReadFull(reader, packet)
	return packet, err
}

func (s *Server) writeTCPMessage(conn net.Conn, mu *sync.Mutex, packet []byte, timeout time.Duration) {
	if len(packet) == 0 || len(packet) > 65535 {
		return
	}
	if !s.guard.AllowResponse(len(packet) + 2) {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	_ = writeFramedDNS(conn, packet)
}

func writeFramedDNS(writer io.Writer, packet []byte) error {
	var prefix [2]byte
	binary.BigEndian.PutUint16(prefix[:], uint16(len(packet)))
	if err := writeAll(writer, prefix[:]); err != nil {
		return err
	}
	return writeAll(writer, packet)
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		data = data[n:]
	}
	return nil
}

func (s *Server) handleQuery(packet []byte, clientAddr *net.UDPAddr) {
	qname, err := dnswire.QuestionName(packet)
	if err != nil {
		s.stats.Drop(false)
		s.logger.Debug("dropping malformed DNS query", "client", clientAddr, "error", err)
		return
	}
	route, ok := s.routes.match(qname)
	if !ok {
		s.stats.Drop(false)
		if s.cfg.UnmatchedAction == "refused" {
			if response := dnswire.RefusedResponse(packet); response != nil {
				s.writeUDPResponse(response, clientAddr)
			}
		}
		return
	}
	s.stats.Query("dns/udp", route.name, len(packet))
	if response, verified := s.verificationResponse(packet, route); verified {
		s.writeUDPResponse(response, clientAddr)
		return
	}
	backend, err := s.getBackend(route.backend)
	if err != nil {
		s.logger.Warn("backend unavailable", "route", route.name, "backend", route.backend, "error", err)
		return
	}
	if len(packet) < 2 {
		return
	}

	clientID := binary.BigEndian.Uint16(packet[:2])
	serverID, ok := backend.reserve(pendingQuery{
		clientAddr: clientAddr,
		clientID:   clientID,
		expiresAt:  time.Now().Add(s.cfg.QueryTimeout()),
		route:      route.name,
	}, s.cfg.MaxPendingPerBackend)
	if !ok {
		s.stats.Drop(false)
		s.logger.Warn("backend pending queue full", "route", route.name, "backend", route.backend)
		return
	}
	s.stats.SessionOpen("dns/udp", route.name)
	binary.BigEndian.PutUint16(packet[:2], serverID)
	if _, err := backend.conn.Write(packet); err != nil {
		backend.remove(serverID)
		s.stats.SessionClose("dns/udp", route.name)
		s.logger.Warn("forward query failed", "route", route.name, "backend", route.backend, "error", err)
	}
}

func (s *Server) getBackend(address string) (*backendConn, error) {
	select {
	case <-s.done:
		return nil, net.ErrClosed
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return nil, net.ErrClosed
	default:
	}
	if backend := s.backends[address]; backend != nil {
		return backend, nil
	}
	remote, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		return nil, err
	}
	backend := &backendConn{address: address, conn: conn, pending: make(map[uint16]pendingQuery)}
	s.backends[address] = backend
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		s.readBackend(backend)
	}()
	go func() {
		defer s.wg.Done()
		s.expirePending(backend)
	}()
	return backend, nil
}

func (b *backendConn) reserve(query pendingQuery, limit int) (uint16, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) >= limit {
		return 0, false
	}
	for attempts := 0; attempts < 65536; attempts++ {
		b.nextID++
		if _, exists := b.pending[b.nextID]; !exists {
			b.pending[b.nextID] = query
			return b.nextID, true
		}
	}
	return 0, false
}

func (b *backendConn) take(id uint16) (pendingQuery, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	query, ok := b.pending[id]
	if ok {
		delete(b.pending, id)
	}
	return query, ok
}

func (b *backendConn) remove(id uint16) {
	b.mu.Lock()
	delete(b.pending, id)
	b.mu.Unlock()
}

func (s *Server) readBackend(backend *backendConn) {
	buffer := make([]byte, s.cfg.MaxPacketSize)
	for {
		n, err := backend.conn.Read(buffer)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.logger.Warn("backend response reader stopped", "backend", backend.address, "error", err)
			}
			return
		}
		if n < 2 {
			continue
		}
		serverID := binary.BigEndian.Uint16(buffer[:2])
		query, ok := backend.take(serverID)
		if !ok {
			continue
		}
		s.stats.SessionClose("dns/udp", query.route)
		binary.BigEndian.PutUint16(buffer[:2], query.clientID)
		if !s.guard.AllowResponse(n) {
			s.stats.Drop(true)
			continue
		}
		s.stats.Out("dns/udp", query.route, n)
		if _, err := s.listener.WriteToUDP(buffer[:n], query.clientAddr); err != nil && !errors.Is(err, net.ErrClosed) {
			s.logger.Debug("client response write failed", "client", query.clientAddr, "error", err)
		}
	}
}

func (s *Server) writeUDPResponse(packet []byte, client *net.UDPAddr) {
	if len(packet) == 0 || !s.guard.AllowResponse(len(packet)) {
		return
	}
	_, _ = s.listener.WriteToUDP(packet, client)
}

func (s *Server) startMaintenance() {
	s.maintenanceOnce.Do(func() {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					s.guard.Prune(10 * time.Minute)
				case <-s.done:
					return
				}
			}
		}()
	})
}

func ipFromUDP(address *net.UDPAddr) netip.Addr {
	if address == nil {
		return netip.Addr{}
	}
	value, _ := netip.AddrFromSlice(address.IP)
	return value.Unmap()
}

func ipFromAddr(address net.Addr) netip.Addr {
	switch value := address.(type) {
	case *net.TCPAddr:
		ip, _ := netip.AddrFromSlice(value.IP)
		return ip.Unmap()
	case *net.UDPAddr:
		return ipFromUDP(value)
	default:
		host, _, err := net.SplitHostPort(address.String())
		if err != nil {
			return netip.Addr{}
		}
		ip, _ := netip.ParseAddr(host)
		return ip.Unmap()
	}
}

func (s *Server) acquireTCPClient(conn net.Conn) (netip.Addr, bool) {
	clientIP := ipFromAddr(conn.RemoteAddr())
	if !s.guard.AcquireTCP(clientIP) {
		return clientIP, false
	}
	select {
	case s.tcpCapacity <- struct{}{}:
		s.connectionsMu.Lock()
		s.connections[conn] = struct{}{}
		s.connectionsMu.Unlock()
		return clientIP, true
	default:
		s.guard.ReleaseTCP(clientIP)
		return clientIP, false
	}
}

func (s *Server) releaseTCPClient(conn net.Conn, clientIP netip.Addr) {
	_ = conn.Close()
	s.connectionsMu.Lock()
	delete(s.connections, conn)
	s.connectionsMu.Unlock()
	<-s.tcpCapacity
	s.guard.ReleaseTCP(clientIP)
}

func (s *Server) expirePending(backend *backendConn) {
	interval := s.cfg.QueryTimeout() / 2
	if interval > time.Second {
		interval = time.Second
	}
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			backend.mu.Lock()
			for id, query := range backend.pending {
				if !now.Before(query.expiresAt) {
					delete(backend.pending, id)
					s.stats.SessionClose("dns/udp", query.route)
				}
			}
			backend.mu.Unlock()
		case <-s.done:
			return
		}
	}
}
