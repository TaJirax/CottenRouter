package router

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/TaJirax/CottenRouter/internal/config"
)

var (
	errBandwidthLimit = errors.New("CottenRouter bandwidth limit reached")
	errNoServerName   = errors.New("TLS ClientHello has no SNI")
)

type tlsRouteEntry struct {
	serverName string
	backend    string
	name       string
}

// ServeTLS routes encrypted TCP streams by TLS ClientHello SNI without
// terminating TLS. This preserves backend certificates and protocols for DoT,
// DoH, NaiveProxy, StunTLS, and other TLS services sharing a public port.
func (s *Server) ServeTLS(ctx context.Context, listener net.Listener, cfg config.TLSListener) error {
	s.listenersMu.Lock()
	s.tlsListeners = append(s.tlsListeners, listener)
	s.listenersMu.Unlock()
	s.startMaintenance()
	table := makeTLSRouteTable(cfg)
	s.logger.Info("CottenRouter listening", "network", "tls", "name", cfg.Name, "address", listener.Addr())
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
		client, err := listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				return nil
			default:
			}
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept TLS client on %s: %w", cfg.Name, err)
		}
		clientIP, ok := s.acquireTCPClient(client)
		if !ok || !s.guard.AllowQuery(clientIP) {
			if ok {
				s.releaseTCPClient(client, clientIP)
			} else {
				_ = client.Close()
			}
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.releaseTCPClient(client, clientIP)
			s.handleTLSClient(client, table, cfg.DefaultBackend)
		}()
	}
}

func makeTLSRouteTable(listener config.TLSListener) []tlsRouteEntry {
	entries := make([]tlsRouteEntry, 0)
	for _, route := range listener.Routes {
		for _, serverName := range route.ServerNames {
			entries = append(entries, tlsRouteEntry{serverName: serverName, backend: route.Backend, name: route.Name})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return len(entries[i].serverName) > len(entries[j].serverName) })
	return entries
}

func (s *Server) handleTLSClient(client net.Conn, routes []tlsRouteEntry, defaultBackend string) {
	_ = client.SetReadDeadline(time.Now().Add(s.cfg.QueryTimeout()))
	initial, serverName, err := readTLSClientHello(client, s.cfg.MaxTCPMessageSize)
	if err != nil && !(errors.Is(err, errNoServerName) && defaultBackend != "") {
		return
	}
	backendAddress := defaultBackend
	for _, route := range routes {
		if serverName == route.serverName || strings.HasSuffix(serverName, "."+route.serverName) {
			backendAddress = route.backend
			break
		}
	}
	if backendAddress == "" {
		return
	}
	backend, err := net.DialTimeout("tcp", backendAddress, s.cfg.QueryTimeout())
	if err != nil {
		return
	}
	defer backend.Close()
	_ = backend.SetWriteDeadline(time.Now().Add(s.cfg.QueryTimeout()))
	if !s.guard.AllowIngress(len(initial)) || writeAll(backend, initial) != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	_ = backend.SetDeadline(time.Time{})

	errorsCh := make(chan error, 2)
	go func() {
		_, err := io.CopyBuffer(&guardedWriter{writer: client, allow: s.guard.AllowResponse}, backend, make([]byte, 32*1024))
		errorsCh <- err
	}()
	go func() {
		_, err := io.CopyBuffer(&guardedWriter{writer: backend, allow: s.guard.AllowIngress}, client, make([]byte, 32*1024))
		errorsCh <- err
	}()
	<-errorsCh
	_ = client.Close()
	_ = backend.Close()
	<-errorsCh
}

type guardedWriter struct {
	writer io.Writer
	allow  func(int) bool
}

func (w *guardedWriter) Write(data []byte) (int, error) {
	if !w.allow(len(data)) {
		return 0, errBandwidthLimit
	}
	return w.writer.Write(data)
}

func readTLSClientHello(reader io.Reader, maxSize int) ([]byte, string, error) {
	var raw, handshake []byte
	for {
		var header [5]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return nil, "", err
		}
		if header[0] != 22 {
			return nil, "", fmt.Errorf("first TLS record is not a handshake")
		}
		recordSize := int(binary.BigEndian.Uint16(header[3:5]))
		if recordSize < 1 || len(raw)+5+recordSize > maxSize {
			return nil, "", fmt.Errorf("TLS ClientHello exceeds limit")
		}
		record := make([]byte, recordSize)
		if _, err := io.ReadFull(reader, record); err != nil {
			return nil, "", err
		}
		raw = append(raw, header[:]...)
		raw = append(raw, record...)
		handshake = append(handshake, record...)
		if len(handshake) < 4 {
			continue
		}
		if handshake[0] != 1 {
			return nil, "", fmt.Errorf("TLS handshake is not ClientHello")
		}
		handshakeSize := int(handshake[1])<<16 | int(handshake[2])<<8 | int(handshake[3])
		if handshakeSize+4 > maxSize {
			return nil, "", fmt.Errorf("TLS ClientHello exceeds limit")
		}
		if len(handshake) >= handshakeSize+4 {
			serverName, err := parseServerName(handshake[4 : handshakeSize+4])
			return raw, serverName, err
		}
	}
}

func parseServerName(clientHello []byte) (string, error) {
	offset := 2 + 32
	if offset >= len(clientHello) {
		return "", io.ErrUnexpectedEOF
	}
	sessionSize := int(clientHello[offset])
	offset += 1 + sessionSize
	if offset+2 > len(clientHello) {
		return "", io.ErrUnexpectedEOF
	}
	cipherSize := int(binary.BigEndian.Uint16(clientHello[offset : offset+2]))
	offset += 2 + cipherSize
	if offset >= len(clientHello) {
		return "", io.ErrUnexpectedEOF
	}
	compressionSize := int(clientHello[offset])
	offset += 1 + compressionSize
	if offset+2 > len(clientHello) {
		return "", errNoServerName
	}
	extensionsSize := int(binary.BigEndian.Uint16(clientHello[offset : offset+2]))
	offset += 2
	end := offset + extensionsSize
	if end > len(clientHello) {
		return "", io.ErrUnexpectedEOF
	}
	for offset+4 <= end {
		extensionType := binary.BigEndian.Uint16(clientHello[offset : offset+2])
		extensionSize := int(binary.BigEndian.Uint16(clientHello[offset+2 : offset+4]))
		offset += 4
		if offset+extensionSize > end {
			return "", io.ErrUnexpectedEOF
		}
		if extensionType == 0 {
			return parseServerNameExtension(clientHello[offset : offset+extensionSize])
		}
		offset += extensionSize
	}
	return "", fmt.Errorf("TLS ClientHello has no SNI")
}

func parseServerNameExtension(extension []byte) (string, error) {
	if len(extension) < 2 {
		return "", io.ErrUnexpectedEOF
	}
	end := 2 + int(binary.BigEndian.Uint16(extension[:2]))
	if end > len(extension) {
		return "", io.ErrUnexpectedEOF
	}
	for offset := 2; offset+3 <= end; {
		nameType := extension[offset]
		nameSize := int(binary.BigEndian.Uint16(extension[offset+1 : offset+3]))
		offset += 3
		if offset+nameSize > end {
			return "", io.ErrUnexpectedEOF
		}
		if nameType == 0 && nameSize > 0 {
			return strings.ToLower(string(extension[offset : offset+nameSize])), nil
		}
		offset += nameSize
	}
	return "", errNoServerName
}
