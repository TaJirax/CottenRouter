package router

import (
	"net"
	"sync"
	"time"
)

// Either direction's payload keeps the entire relay alive. Independent read
// timeouts would interrupt a healthy download whose upload side is quiet.
type relayActivity struct {
	mu      sync.Mutex
	client  net.Conn
	backend net.Conn
	idle    time.Duration
}

type idleRelayConn struct {
	net.Conn
	activity *relayActivity
}

func newIdleRelay(client, backend net.Conn, idle time.Duration) (net.Conn, net.Conn) {
	a := &relayActivity{client: client, backend: backend, idle: idle}
	a.touch()
	return &idleRelayConn{Conn: client, activity: a}, &idleRelayConn{Conn: backend, activity: a}
}

func (a *relayActivity) touch() {
	a.mu.Lock()
	defer a.mu.Unlock()
	deadline := time.Now().Add(a.idle)
	_ = a.client.SetDeadline(deadline)
	_ = a.backend.SetDeadline(deadline)
}

func (c *idleRelayConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.activity.touch()
	}
	return n, err
}

func (c *idleRelayConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.activity.touch()
	}
	return n, err
}
