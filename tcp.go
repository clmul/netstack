package netstack

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/google/netstack/tcpip"
	"github.com/google/netstack/tcpip/network/ipv4"
	"github.com/google/netstack/tcpip/transport/tcp"
	"github.com/google/netstack/waiter"
)

func dialTCP(ctx context.Context, p *Endpoint, address string) (net.Conn, error) {
	var wq waiter.Queue
	ep, e := p.stack.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if e != nil {
		return nil, stackErr{e}
	}

	host, port, err := p.resolver.resolveAddrPort(ctx, address)
	if err != nil {
		return nil, err
	}

	waitREntry, notifyRCh := waiter.NewChannelEntry(nil)
	waitWEntry, notifyWCh := waiter.NewChannelEntry(nil)
	wq.EventRegister(&waitREntry, waiter.EventIn)
	wq.EventRegister(&waitWEntry, waiter.EventOut)

	e = ep.Connect(tcpip.FullAddress{
		NIC:  p.nicID,
		Addr: tcpip.Address(host),
		Port: port,
	})
	if e == tcpip.ErrConnectStarted {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-notifyWCh:
		}
		e = ep.GetSockOpt(tcpip.ErrorOption{})
	}
	if e != nil {
		return nil, stackErr{e}
	}

	return &TCPConn{
		ep:         ep,
		wq:         &wq,
		waitREntry: &waitREntry,
		waitWEntry: &waitWEntry,
		notifyRCh:  notifyRCh,
		notifyWCh:  notifyWCh,
	}, nil
}

func listenTCP(ctx context.Context, p *Endpoint, address string) (net.Listener, error) {
	var wq waiter.Queue
	ep, e := p.stack.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if e != nil {
		return nil, stackErr{e}
	}

	host, port, err := p.resolver.resolveAddrPort(ctx, address)
	if err != nil {
		return nil, err
	}

	e = ep.Bind(tcpip.FullAddress{
		NIC:  p.nicID,
		Addr: tcpip.Address(host),
		Port: port,
	})
	if e != nil {
		return nil, stackErr{e}
	}
	e = ep.Listen(4)
	if e != nil {
		return nil, stackErr{e}
	}
	waitEntry, notifyCh := waiter.NewChannelEntry(nil)
	wq.EventRegister(&waitEntry, waiter.EventIn)
	return &TCPListener{
		ep:        ep,
		wq:        &wq,
		waitEntry: &waitEntry,
		notifyCh:  notifyCh,
	}, nil
}

type TCPListener struct {
	ep        tcpip.Endpoint
	wq        *waiter.Queue
	waitEntry *waiter.Entry
	notifyCh  chan struct{}
}

func (ln *TCPListener) Accept() (net.Conn, error) {
	for {
		ep, wq, e := ln.ep.Accept()
		if e != nil {
			if e == tcpip.ErrWouldBlock {
				<-ln.notifyCh
				continue
			}
			return nil, stackErr{e}
		}
		waitREntry, notifyRCh := waiter.NewChannelEntry(nil)
		waitWEntry, notifyWCh := waiter.NewChannelEntry(nil)
		wq.EventRegister(&waitREntry, waiter.EventIn)
		wq.EventRegister(&waitWEntry, waiter.EventOut)
		return &TCPConn{
			ep:         ep,
			wq:         wq,
			waitREntry: &waitREntry,
			waitWEntry: &waitWEntry,
			notifyRCh:  notifyRCh,
			notifyWCh:  notifyWCh,
		}, nil
	}
}

func (ln *TCPListener) Close() error {
	ln.wq.EventUnregister(ln.waitEntry)
	ln.ep.Close()
	return nil
}

func (ln *TCPListener) Addr() net.Addr {
	addr, e := ln.ep.GetLocalAddress()
	if e != nil {
		panic(e)
	}
	return convertToTCPAddr(addr)
}

type TCPConn struct {
	ep          tcpip.Endpoint
	wq          *waiter.Queue
	waitREntry  *waiter.Entry
	waitWEntry  *waiter.Entry
	notifyRCh   chan struct{}
	notifyWCh   chan struct{}
	pendingRead []byte
}

func (c *TCPConn) Read(b []byte) (int, error) {
	if len(c.pendingRead) != 0 {
		n := copy(b, c.pendingRead)
		c.pendingRead = c.pendingRead[n:]
		return n, nil
	}
	for {
		v, _, e := c.ep.Read(nil)
		if e != nil {
			if e == tcpip.ErrWouldBlock {
				<-c.notifyRCh
				continue
			}
			return 0, stackErr{e}
		}
		n := copy(b, v)
		c.pendingRead = v[n:]
		return n, nil
	}
}

func (c *TCPConn) Write(p []byte) (int, error) {
	size := len(p)
	b := make([]byte, size)
	copy(b, p)
	for len(b) > 0 {
		n, _, e := c.ep.Write(tcpip.SlicePayload(b), tcpip.WriteOptions{})
		if e != nil {
			if e == tcpip.ErrWouldBlock {
				<-c.notifyWCh
				continue
			}
			return int(n), stackErr{e}
		}
		b = b[n:]
	}
	return size, nil
}

func (c *TCPConn) Close() error {
	c.wq.EventUnregister(c.waitREntry)
	c.wq.EventUnregister(c.waitWEntry)
	c.ep.Close()
	return nil
}

func (c *TCPConn) LocalAddr() net.Addr {
	addr, e := c.ep.GetLocalAddress()
	if e != nil {
		panic(e)
	}
	return convertToTCPAddr(addr)
}

func (c *TCPConn) RemoteAddr() net.Addr {
	addr, e := c.ep.GetRemoteAddress()
	if e != nil {
		panic(e)
	}
	return convertToTCPAddr(addr)
}

func (c *TCPConn) SetDeadline(t time.Time) error {
	err := c.SetReadDeadline(t)
	if err != nil {
		return err
	}
	err = c.SetWriteDeadline(t)
	return err
}

func (c *TCPConn) SetReadDeadline(t time.Time) error {
	return fmt.Errorf("not supported")
}

func (c *TCPConn) SetWriteDeadline(t time.Time) error {
	return fmt.Errorf("not supported")
}

func convertToTCPAddr(addr tcpip.FullAddress) *net.TCPAddr {
	return &net.TCPAddr{
		IP:   []byte(addr.Addr),
		Port: int(addr.Port),
	}
}
