package router

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/TaJirax/CottenRouter/internal/dnswire"
)

// exchangeTCPViaUDP uses the same connected UDP socket generations, question
// matching and pending limit as ordinary UDP ingress. IDs are never reused in a
// generation, so a late reply to a canceled TCP query cannot satisfy a new one.
func (s *Server) exchangeTCPViaUDP(ctx context.Context, route routeEntry, packet []byte, question dnswire.Question) ([]byte, error) {
	if len(packet) > s.cfg.MaxPacketSize {
		return nil, fmt.Errorf("TCP query exceeds UDP backend packet limit")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, net.ErrClosed
	default:
	}
	backend, err := s.getBackend(route.backend)
	if err != nil {
		return nil, err
	}
	query := pendingQuery{
		clientID:  binary.BigEndian.Uint16(packet[:2]),
		expiresAt: time.Now().Add(s.cfg.QueryTimeout()),
		route:     route.name, question: question, response: make(chan []byte, 1),
	}
	s.stats.SessionOpen(query.protocol(), query.route)
	var serverID uint16
	for {
		var reserved, exhausted bool
		serverID, reserved, exhausted = backend.reserve(query, s.cfg.MaxPendingPerBackend)
		if reserved {
			break
		}
		if !exhausted {
			s.stats.SessionClose(query.protocol(), query.route)
			return nil, fmt.Errorf("UDP backend pending queue full")
		}
		backend, err = s.rotateBackend(route.backend, backend)
		if err != nil {
			s.stats.SessionClose(query.protocol(), query.route)
			return nil, err
		}
	}
	defer func() {
		backend.mu.Lock()
		_, pending := backend.pending[serverID]
		delete(backend.pending, serverID)
		backend.mu.Unlock()
		if pending {
			s.stats.SessionClose(query.protocol(), query.route)
		}
	}()
	forward := append([]byte(nil), packet...)
	binary.BigEndian.PutUint16(forward[:2], serverID)
	if _, err := backend.conn.Write(forward); err != nil {
		s.dropBackend(backend)
		return nil, err
	}
	timer := time.NewTimer(time.Until(query.expiresAt))
	defer timer.Stop()
	select {
	case response := <-query.response:
		return response, nil
	case <-timer.C:
		return nil, context.DeadlineExceeded
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, net.ErrClosed
	case <-backend.done:
		return nil, fmt.Errorf("UDP backend closed")
	}
}
