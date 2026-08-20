package router

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/TaJirax/CottenRouter/internal/config"
	"github.com/TaJirax/CottenRouter/internal/dnswire"
)

type Server struct {
	cfg         config.Config
	routes      routeTable
	logger      *slog.Logger
	listener    *net.UDPConn
	tcpListener net.Listener
	done        chan struct{}

	mu        sync.Mutex
	backends  map[string]*backendConn
	wg        sync.WaitGroup
	closeOnce sync.Once
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
	return &Server{
		cfg: cfg, routes: table, logger: logger,
		done: make(chan struct{}), backends: make(map[string]*backendConn),
	}, nil
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
	var tcpErrors chan error
	if s.cfg.ListenTCP != "" {
		tcpListener, err := net.Listen("tcp", s.cfg.ListenTCP)
		if err != nil {
			_ = listener.Close()
			return fmt.Errorf("listen on TCP %s: %w", s.cfg.ListenTCP, err)
		}
		s.tcpListener = tcpListener
		tcpErrors = make(chan error, 1)
		go func() {
			err := s.ServeTCP(ctx, tcpListener)
			if err != nil {
				s.Close()
			}
			tcpErrors <- err
		}()
	}
	udpErr := s.Serve(ctx, listener)
	s.Close()
	if tcpErrors != nil {
		if tcpErr := <-tcpErrors; udpErr == nil && tcpErr != nil {
			return tcpErr
		}
	}
	return udpErr
}

// Serve runs on an already-open UDP socket. It is exported to support socket
// activation and deterministic local tests.
func (s *Server) Serve(ctx context.Context, listener *net.UDPConn) error {
	s.listener = listener
	s.logger.Info("CottenRouter listening", "network", "udp", "address", listener.LocalAddr())

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
		packet := make([]byte, s.cfg.MaxPacketSize)
		n, clientAddr, err := listener.ReadFromUDP(packet)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				s.wg.Wait()
				return nil
			}
			return fmt.Errorf("read client packet: %w", err)
		}
		packet = packet[:n]
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleQuery(packet, clientAddr)
		}()
	}
}

func (s *Server) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.LocalAddr()
}

func (s *Server) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		close(s.done)
		if s.listener != nil {
			closeErr = s.listener.Close()
		}
		if s.tcpListener != nil {
			if err := s.tcpListener.Close(); closeErr == nil {
				closeErr = err
			}
		}
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
	s.tcpListener = listener
	s.logger.Info("CottenRouter listening", "network", "tcp", "address", listener.Addr())
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stop:
		}
	}()
	defer close(stop)
	capacity := make(chan struct{}, s.cfg.MaxTCPConnections)
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
		select {
		case capacity <- struct{}{}:
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer func() { <-capacity }()
				s.handleTCPConnection(conn)
			}()
		default:
			s.logger.Warn("TCP connection limit reached", "client", conn.RemoteAddr())
			_ = conn.Close()
		}
	}
}

func (s *Server) handleTCPConnection(client net.Conn) {
	defer client.Close()
	var writeMu sync.Mutex
	var queries sync.WaitGroup
	defer queries.Wait()
	for {
		_ = client.SetReadDeadline(time.Now().Add(s.cfg.QueryTimeout()))
		packet, err := readTCPMessage(client, s.cfg.MaxPacketSize)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.logger.Debug("TCP client read stopped", "client", client.RemoteAddr(), "error", err)
			}
			return
		}
		queries.Add(1)
		go func() {
			defer queries.Done()
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
			writeTCPMessage(client, writeMu, dnswire.RefusedResponse(packet), s.cfg.QueryTimeout())
		}
		return
	}
	if route.tcpBackend == "disabled" {
		writeTCPMessage(client, writeMu, dnswire.ErrorResponse(packet, 2), s.cfg.QueryTimeout())
		return
	}
	backend, err := net.DialTimeout("tcp", route.tcpBackend, s.cfg.QueryTimeout())
	if err != nil {
		s.logger.Debug("TCP backend unavailable", "route", route.name, "backend", route.tcpBackend, "error", err)
		writeTCPMessage(client, writeMu, dnswire.ErrorResponse(packet, 2), s.cfg.QueryTimeout())
		return
	}
	defer backend.Close()
	_ = backend.SetDeadline(time.Now().Add(s.cfg.QueryTimeout()))
	if err := writeFramedDNS(backend, packet); err != nil {
		return
	}
	response, err := readTCPMessage(backend, s.cfg.MaxPacketSize)
	if err != nil {
		return
	}
	writeTCPMessage(client, writeMu, response, s.cfg.QueryTimeout())
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

func writeTCPMessage(conn net.Conn, mu *sync.Mutex, packet []byte, timeout time.Duration) {
	if len(packet) == 0 || len(packet) > 65535 {
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
		s.logger.Debug("dropping malformed DNS query", "client", clientAddr, "error", err)
		return
	}
	route, ok := s.routes.match(qname)
	if !ok {
		if s.cfg.UnmatchedAction == "refused" {
			if response := dnswire.RefusedResponse(packet); response != nil {
				_, _ = s.listener.WriteToUDP(response, clientAddr)
			}
		}
		return
	}
	if response, verified := s.verificationResponse(packet, route); verified {
		_, _ = s.listener.WriteToUDP(response, clientAddr)
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
	}, s.cfg.MaxPendingPerBackend)
	if !ok {
		s.logger.Warn("backend pending queue full", "route", route.name, "backend", route.backend)
		return
	}
	binary.BigEndian.PutUint16(packet[:2], serverID)
	if _, err := backend.conn.Write(packet); err != nil {
		backend.remove(serverID)
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
		binary.BigEndian.PutUint16(buffer[:2], query.clientID)
		if _, err := s.listener.WriteToUDP(buffer[:n], query.clientAddr); err != nil && !errors.Is(err, net.ErrClosed) {
			s.logger.Debug("client response write failed", "client", query.clientAddr, "error", err)
		}
	}
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
				}
			}
			backend.mu.Unlock()
		case <-s.done:
			return
		}
	}
}
