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

	mu                   sync.Mutex
	backends             map[string]*backendConn
	allBackends          map[*backendConn]struct{}
	backendRetry         map[string]time.Time
	wg                   sync.WaitGroup
	closeOnce            sync.Once
	maintenanceOnce      sync.Once
	packetPool           sync.Pool
	tcpCapacity          chan struct{}
	tlsHandshakeCapacity chan struct{}
	tlsCapacities        map[string]chan struct{}
	listenersMu          sync.Mutex
	connectionsMu        sync.Mutex
	connections          map[net.Conn]struct{}
}

type backendConn struct {
	address   string
	conn      *net.UDPConn
	mu        sync.Mutex
	pending   map[uint16]pendingQuery
	issued    uint32
	retireAt  time.Time
	done      chan struct{}
	closeOnce sync.Once
}

type pendingQuery struct {
	clientAddr *net.UDPAddr
	clientID   uint16
	expiresAt  time.Time
	route      string
	// question fingerprints the query so a reply carrying the right transaction
	// ID but the wrong question cannot consume the mapping.
	question dnswire.Question
}

// matches reports whether a reply's echoed question is the one we asked. End is
// deliberately excluded: only the name, type, and class identify the question.
func (q pendingQuery) matches(reply dnswire.Question) bool {
	return q.question.Name == reply.Name && q.question.Type == reply.Type && q.question.Class == reply.Class
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
		done: make(chan struct{}), guard: guard.New(cfg.Limits), stats: telemetry.New(), backends: make(map[string]*backendConn), allBackends: make(map[*backendConn]struct{}), backendRetry: make(map[string]time.Time),
		tcpCapacity: make(chan struct{}, cfg.MaxTCPConnections), tlsHandshakeCapacity: make(chan struct{}, cfg.MaxTLSConnectionsPerProtocol), tlsCapacities: make(map[string]chan struct{}), connections: make(map[net.Conn]struct{}),
	}
	// One extra byte makes an oversized UDP datagram observable instead of
	// silently forwarding a MaxPacketSize-byte truncation.
	server.packetPool.New = func() any { return make([]byte, cfg.MaxPacketSize+1) }
	for _, class := range []string{"doh", "dot", "naiveproxy", "stuntls", "tls/other"} {
		server.ensureTLSCapacity(class)
	}
	for _, route := range cfg.Routes {
		server.stats.Ensure("dns/udp", route.Name)
		if route.TCPBackend != "" && route.TCPBackend != "disabled" {
			server.stats.Ensure("dns/tcp", route.Name)
		}
	}
	for _, listener := range cfg.TLSListeners {
		for _, route := range listener.Routes {
			protocol := classifyTLSProtocol(listener.Name, route.Name)
			server.stats.Ensure(protocol, route.Name)
			server.ensureTLSCapacity(protocol)
		}
		if listener.DefaultBackend != "" {
			protocol := classifyTLSProtocol(listener.Name, listener.DefaultRouteName)
			server.stats.Ensure(protocol, listener.DefaultRouteName)
			server.ensureTLSCapacity(protocol)
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
		if n > s.cfg.MaxPacketSize {
			// Windows may return WSAEMSGSIZE together with the truncated byte
			// count; Unix normally returns a successful MaxPacketSize+1 read.
			// In either case the datagram is consumed and must not be forwarded.
			s.stats.Drop(false)
			s.packetPool.Put(packet[:cap(packet)])
			continue
		}
		if err != nil {
			s.packetPool.Put(packet)
			finish()
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("read client packet: %w", err)
		}
		packet = packet[:n]
		clientIP := ipFromUDP(clientAddr)
		if !s.guard.AllowQueryIngress(clientIP, n) {
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
		backends := make([]*backendConn, 0, len(s.allBackends))
		for backend := range s.allBackends {
			backends = append(backends, backend)
		}
		clear(s.backends)
		clear(s.allBackends)
		s.mu.Unlock()
		for _, backend := range backends {
			s.closeBackend(backend, false)
		}
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
		if !s.guard.AllowQueryIngress(clientIP, len(packet)+2) {
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
	question, err := dnswire.ParseQuestion(packet)
	if err != nil {
		return
	}
	route, ok := s.routes.match(question.Name)
	if !ok {
		if s.cfg.UnmatchedAction == "refused" {
			s.writeTCPMessage(client, writeMu, dnswire.RefusedResponse(packet), s.cfg.QueryTimeout())
		}
		return
	}
	s.stats.Query("dns/tcp", route.name, len(packet)+2)
	if route.tcpBackend == "disabled" {
		response := dnswire.ErrorResponse(packet, 2)
		if s.writeTCPMessage(client, writeMu, response, s.cfg.QueryTimeout()) {
			s.stats.Out("dns/tcp", route.name, len(response)+2)
		}
		return
	}
	backend, err := net.DialTimeout("tcp", route.tcpBackend, s.cfg.QueryTimeout())
	if err != nil {
		s.logger.Debug("TCP backend unavailable", "route", route.name, "backend", route.tcpBackend, "error", err)
		response := dnswire.ErrorResponse(packet, 2)
		if s.writeTCPMessage(client, writeMu, response, s.cfg.QueryTimeout()) {
			s.stats.Out("dns/tcp", route.name, len(response)+2)
		}
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
	if s.writeTCPMessage(client, writeMu, response, s.cfg.QueryTimeout()) {
		s.stats.Out("dns/tcp", route.name, len(response)+2)
	} else {
		s.stats.Error("dns/tcp", route.name)
	}
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

func (s *Server) writeTCPMessage(conn net.Conn, mu *sync.Mutex, packet []byte, timeout time.Duration) bool {
	if len(packet) == 0 || len(packet) > 65535 {
		return false
	}
	if !s.guard.AllowResponse(len(packet) + 2) {
		s.stats.Drop(true)
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	return writeFramedDNS(conn, packet) == nil
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
	question, err := dnswire.ParseQuestion(packet)
	if err != nil {
		s.stats.Drop(false)
		s.logger.Debug("dropping malformed DNS query", "client", clientAddr, "error", err)
		return
	}
	route, ok := s.routes.match(question.Name)
	if !ok {
		s.stats.Drop(false)
		if s.cfg.UnmatchedAction == "refused" {
			if response := dnswire.RefusedResponse(packet); response != nil {
				s.writeUDPResponse(response, clientAddr, "")
			}
		}
		return
	}
	s.stats.Query("dns/udp", route.name, len(packet))
	if response, verified := s.verificationResponse(packet, route); verified {
		s.writeUDPResponse(response, clientAddr, route.name)
		return
	}
	backend, err := s.getBackend(route.backend)
	if err != nil {
		s.stats.Error("dns/udp", route.name)
		s.logger.Debug("backend unavailable", "route", route.name, "backend", route.backend, "error", err)
		return
	}
	if len(packet) < 2 {
		return
	}

	clientID := binary.BigEndian.Uint16(packet[:2])
	query := pendingQuery{
		clientAddr: clientAddr,
		clientID:   clientID,
		expiresAt:  time.Now().Add(s.cfg.QueryTimeout()),
		route:      route.name,
		question:   question,
	}
	// Publish the active session before reserve makes the query visible to the
	// backend reader. A loopback backend can answer immediately after Write;
	// opening afterward races SessionClose and can leave a phantom +1.
	s.stats.SessionOpen("dns/udp", route.name)
	var serverID uint16
	for {
		var reserved, exhausted bool
		serverID, reserved, exhausted = backend.reserve(query, s.cfg.MaxPendingPerBackend)
		if reserved {
			break
		}
		if !exhausted {
			s.stats.SessionClose("dns/udp", route.name)
			s.stats.Drop(false)
			s.logger.Debug("backend pending queue full", "route", route.name, "backend", route.backend)
			return
		}
		backend, err = s.rotateBackend(route.backend, backend)
		if err != nil {
			s.stats.SessionClose("dns/udp", route.name)
			s.stats.Error("dns/udp", route.name)
			s.logger.Debug("backend generation rotation failed", "route", route.name, "backend", route.backend, "error", err)
			return
		}
	}
	binary.BigEndian.PutUint16(packet[:2], serverID)
	if _, err := backend.conn.Write(packet); err != nil {
		// A connected UDP write error means the cached socket is no longer a
		// reliable path. Evict it immediately; closeBackend drains this and all
		// other pending queries exactly once, and the next request reconnects.
		s.dropBackend(backend)
		s.logger.Debug("forward query failed", "route", route.name, "backend", route.backend, "error", err)
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
	if retryAt := s.backendRetry[address]; time.Now().Before(retryAt) {
		return nil, fmt.Errorf("backend reconnect is cooling down until %s", retryAt.Format(time.RFC3339Nano))
	}
	return s.newBackendLocked(address)
}

func (s *Server) newBackendLocked(address string) (*backendConn, error) {
	remote, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		return nil, err
	}
	backend := &backendConn{address: address, conn: conn, pending: make(map[uint16]pendingQuery), done: make(chan struct{})}
	s.backends[address] = backend
	s.allBackends[backend] = struct{}{}
	delete(s.backendRetry, address)
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

// rotateBackend installs a new connected UDP socket after the current socket
// has issued all 65,536 transaction IDs. The retired socket remains open for
// two query timeouts, so delayed/duplicate replies stay in their original
// source-port namespace and can never match a query in the new generation.
func (s *Server) rotateBackend(address string, exhausted *backendConn) (*backendConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return nil, net.ErrClosed
	default:
	}
	if current := s.backends[address]; current != nil && current != exhausted {
		return current, nil
	}
	if s.backends[address] == nil {
		if retryAt := s.backendRetry[address]; time.Now().Before(retryAt) {
			return nil, fmt.Errorf("backend reconnect is cooling down until %s", retryAt.Format(time.RFC3339Nano))
		}
	}
	replacement, err := s.newBackendLocked(address)
	if err != nil {
		return nil, err
	}
	exhausted.mu.Lock()
	exhausted.retireAt = time.Now().Add(2 * s.cfg.QueryTimeout())
	exhausted.mu.Unlock()
	return replacement, nil
}

// reserve returns exhausted=true only when this socket generation has used
// every ID. IDs are deliberately never reused on one connected UDP socket;
// source-port-separated generations provide additional 16-bit namespaces.
func (b *backendConn) reserve(query pendingQuery, limit int) (id uint16, ok, exhausted bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.done:
		return 0, false, false
	default:
	}
	if b.issued >= 1<<16 {
		return 0, false, true
	}
	if len(b.pending) >= limit {
		return 0, false, false
	}
	id = uint16(b.issued)
	b.issued++
	b.pending[id] = query
	return id, true, false
}

// take consumes the mapping for id only when reply echoes the question that was
// actually sent under that ID. A mismatch leaves the mapping in place so the
// genuine reply can still arrive.
func (b *backendConn) take(id uint16, reply dnswire.Question) (pendingQuery, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	query, ok := b.pending[id]
	if !ok || !query.matches(reply) {
		return pendingQuery{}, false
	}
	delete(b.pending, id)
	return query, true
}

func (s *Server) readBackend(backend *backendConn) {
	buffer := make([]byte, s.cfg.MaxPacketSize+1)
	for {
		n, err := backend.conn.Read(buffer)
		if n > s.cfg.MaxPacketSize {
			s.stats.Drop(false)
			continue
		}
		if err != nil {
			select {
			case <-s.done:
				s.closeBackend(backend, false)
			default:
				s.logger.Debug("backend response reader restarting", "backend", backend.address, "error", err)
				s.dropBackend(backend)
			}
			return
		}
		if n < 2 {
			continue
		}
		serverID := binary.BigEndian.Uint16(buffer[:2])
		reply, err := dnswire.ParseReplyQuestion(buffer[:n])
		if err != nil {
			s.stats.Drop(false)
			continue
		}
		query, ok := backend.take(serverID, reply)
		if !ok {
			continue
		}
		s.stats.SessionClose("dns/udp", query.route)
		binary.BigEndian.PutUint16(buffer[:2], query.clientID)
		if !s.guard.AllowResponse(n) {
			s.stats.Drop(true)
			continue
		}
		written, err := s.listener.WriteToUDP(buffer[:n], query.clientAddr)
		if err != nil || written != n {
			s.stats.Error("dns/udp", query.route)
		}
		if err != nil && !errors.Is(err, net.ErrClosed) {
			s.logger.Debug("client response write failed", "client", query.clientAddr, "error", err)
			continue
		}
		if written == n {
			s.stats.Out("dns/udp", query.route, n)
		}
	}
}

func (s *Server) writeUDPResponse(packet []byte, client *net.UDPAddr, route string) bool {
	if len(packet) == 0 {
		return false
	}
	if !s.guard.AllowResponse(len(packet)) {
		s.stats.Drop(true)
		return false
	}
	written, err := s.listener.WriteToUDP(packet, client)
	if err != nil || written != len(packet) {
		if route != "" {
			s.stats.Error("dns/udp", route)
		}
		return false
	}
	if route != "" {
		s.stats.Out("dns/udp", route, written)
	}
	return true
}

func (s *Server) dropBackend(backend *backendConn) {
	s.mu.Lock()
	if s.backends[backend.address] == backend {
		delete(s.backends, backend.address)
		s.backendRetry[backend.address] = time.Now().Add(250 * time.Millisecond)
	}
	s.mu.Unlock()
	s.closeBackend(backend, true)
}

func (s *Server) closeBackend(backend *backendConn, failed bool) {
	var pending []pendingQuery
	closed := false
	backend.closeOnce.Do(func() {
		closed = true
		close(backend.done)
		_ = backend.conn.Close()
		backend.mu.Lock()
		pending = make([]pendingQuery, 0, len(backend.pending))
		for id, query := range backend.pending {
			pending = append(pending, query)
			delete(backend.pending, id)
		}
		backend.mu.Unlock()
	})
	if closed {
		s.mu.Lock()
		delete(s.allBackends, backend)
		s.mu.Unlock()
	}
	for _, query := range pending {
		s.stats.SessionClose("dns/udp", query.route)
		if failed {
			s.stats.Error("dns/udp", query.route)
		}
	}
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

func tlsCapacityClass(protocol string) string {
	switch protocol {
	case "doh", "dot", "naiveproxy", "stuntls":
		return protocol
	default:
		return "tls/other"
	}
}

func (s *Server) ensureTLSCapacity(protocol string) chan struct{} {
	class := tlsCapacityClass(protocol)
	if capacity := s.tlsCapacities[class]; capacity != nil {
		return capacity
	}
	capacity := make(chan struct{}, s.cfg.MaxTLSConnectionsPerProtocol)
	s.tlsCapacities[class] = capacity
	return capacity
}

func (s *Server) acquireTLSHandshake(conn net.Conn) (netip.Addr, bool) {
	clientIP := ipFromAddr(conn.RemoteAddr())
	if !s.guard.AcquireTLS(clientIP, "handshake") {
		return clientIP, false
	}
	select {
	case s.tlsHandshakeCapacity <- struct{}{}:
	case <-s.done:
		s.guard.ReleaseTLS(clientIP, "handshake")
		return clientIP, false
	default:
		s.guard.ReleaseTLS(clientIP, "handshake")
		return clientIP, false
	}
	if !s.trackConnection(conn) {
		s.releaseTLSHandshakeLease(clientIP)
		return clientIP, false
	}
	return clientIP, true
}

// trackConnection registers conn so Close can shut it down, refusing once
// shutdown has begun. Close closes s.done before sweeping s.connections under
// connectionsMu, so a refused registration is always a connection the sweep
// would have missed; the caller closes it instead.
func (s *Server) trackConnection(conn net.Conn) bool {
	s.connectionsMu.Lock()
	defer s.connectionsMu.Unlock()
	select {
	case <-s.done:
		return false
	default:
		s.connections[conn] = struct{}{}
		return true
	}
}

func (s *Server) releaseTLSHandshakeLease(clientIP netip.Addr) {
	<-s.tlsHandshakeCapacity
	s.guard.ReleaseTLS(clientIP, "handshake")
}

func (s *Server) releaseTLSHandshake(conn net.Conn, clientIP netip.Addr) {
	s.releaseTLSHandshakeLease(clientIP)
	s.untrackConnection(conn)
}

func (s *Server) acquireTLSProtocol(clientIP netip.Addr, protocol string) bool {
	class := tlsCapacityClass(protocol)
	if !s.guard.AcquireTLS(clientIP, class) {
		return false
	}
	capacity := s.tlsCapacities[class]
	if capacity == nil {
		s.guard.ReleaseTLS(clientIP, class)
		return false
	}
	select {
	case capacity <- struct{}{}:
		return true
	default:
		s.guard.ReleaseTLS(clientIP, class)
		return false
	}
}

func (s *Server) releaseTLSProtocol(conn net.Conn, clientIP netip.Addr, protocol string) {
	class := tlsCapacityClass(protocol)
	<-s.tlsCapacities[class]
	s.guard.ReleaseTLS(clientIP, class)
	s.untrackConnection(conn)
}

func (s *Server) untrackConnection(conn net.Conn) {
	_ = conn.Close()
	s.connectionsMu.Lock()
	delete(s.connections, conn)
	s.connectionsMu.Unlock()
}

func (s *Server) acquireTCPClient(conn net.Conn) (netip.Addr, bool) {
	clientIP := ipFromAddr(conn.RemoteAddr())
	if !s.guard.AcquireTCP(clientIP) {
		return clientIP, false
	}
	select {
	case s.tcpCapacity <- struct{}{}:
		if !s.trackConnection(conn) {
			<-s.tcpCapacity
			s.guard.ReleaseTCP(clientIP)
			_ = conn.Close()
			return clientIP, false
		}
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
			retiredAndDrained := false
			backend.mu.Lock()
			for id, query := range backend.pending {
				if !now.Before(query.expiresAt) {
					delete(backend.pending, id)
					s.stats.SessionClose("dns/udp", query.route)
				}
			}
			retiredAndDrained = !backend.retireAt.IsZero() && !now.Before(backend.retireAt) && len(backend.pending) == 0
			backend.mu.Unlock()
			if retiredAndDrained {
				s.closeBackend(backend, false)
				return
			}
		case <-s.done:
			return
		case <-backend.done:
			return
		}
	}
}
