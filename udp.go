package netstack

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/google/netstack/tcpip"
	"github.com/google/netstack/tcpip/network/ipv4"
	"github.com/google/netstack/tcpip/transport/tcp"
	"github.com/google/netstack/tcpip/transport/udp"
	"github.com/google/netstack/waiter"
)

func dialUDP(ctx context.Context, p *Endpoint, address string) (net.Conn, error) {
	var wq waiter.Queue
	ep, e := p.stack.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if e != nil {
		return nil, stackErr{e}
	}

	host, port, err := p.resolver.resolveAddrPort(ctx, address)
	if err != nil {
		return nil, err
	}

	waitREntry, notifyRCh := waiter.NewChannelEntry(nil)
	wq.EventRegister(&waitREntry, waiter.EventIn)

	e = ep.Connect(tcpip.FullAddress{
		NIC:  p.nicID,
		Addr: tcpip.Address(host),
		Port: port,
	})
	if e != nil {
		return nil, stackErr{e}
	}

	return &UDPConn{
		ep:         ep,
		wq:         &wq,
		waitREntry: &waitREntry,
		notifyRCh:  notifyRCh,
	}, nil
}

func listenUDP(ctx context.Context, p *Endpoint, address string) (net.PacketConn, error) {
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
	waitREntry, notifyRCh := waiter.NewChannelEntry(nil)
	wq.EventRegister(&waitREntry, waiter.EventIn)
	return &UDPConn{
		ep:         ep,
		wq:         &wq,
		waitREntry: &waitREntry,
		notifyRCh:  notifyRCh,
	}, nil
}

var _ net.PacketConn = &UDPConn{}

type UDPConn struct {
	ep         tcpip.Endpoint
	wq         *waiter.Queue
	waitREntry *waiter.Entry
	notifyRCh  chan struct{}
}

func (c *UDPConn) ReadFrom(b []byte) (n int, addr net.Addr, err error) {
	for {
		var sender tcpip.FullAddress
		v, _, e := c.ep.Read(&sender)
		if e != nil {
			if e == tcpip.ErrWouldBlock {
				<-c.notifyRCh
				continue
			}
			return 0, nil, stackErr{e}
		}
		n = copy(b, v)
		addr = convertToUDPAddr(sender)
		return n, addr, nil
	}
}

func (c *UDPConn) WriteTo(b []byte, addr net.Addr) (n int, err error) {
	var dst *tcpip.FullAddress
	if addr != nil {
		dst = &tcpip.FullAddress{
			Addr: tcpip.Address(addr.(*net.UDPAddr).IP),
			Port: uint16(addr.(*net.UDPAddr).Port),
		}
	}
	written, _, e := c.ep.Write(tcpip.SlicePayload(b), tcpip.WriteOptions{To: dst})
	if e != nil {
		return int(written), stackErr{e}
	}
	return int(written), nil
}

func (c *UDPConn) Close() error {
	c.wq.EventUnregister(c.waitREntry)
	c.ep.Close()
	return nil
}

func (c *UDPConn) LocalAddr() net.Addr {
	addr, e := c.ep.GetLocalAddress()
	if e != nil {
		panic(e)
	}
	return convertToTCPAddr(addr)
}

func (c *UDPConn) SetDeadline(t time.Time) error {
	err := c.SetReadDeadline(t)
	if err != nil {
		return err
	}
	err = c.SetWriteDeadline(t)
	return err
}

func (c *UDPConn) SetReadDeadline(t time.Time) error {
	return fmt.Errorf("not supported")
}

func (c *UDPConn) SetWriteDeadline(t time.Time) error {
	return nil
}

func convertToUDPAddr(addr tcpip.FullAddress) *net.UDPAddr {
	return &net.UDPAddr{
		IP:   []byte(addr.Addr),
		Port: int(addr.Port),
	}
}

// interface net.Conn

func (c *UDPConn) RemoteAddr() net.Addr {
	addr, e := c.ep.GetRemoteAddress()
	if e != nil {
		panic(e)
	}
	return convertToUDPAddr(addr)
}

func (c *UDPConn) Read(b []byte) (n int, err error) {
	n, _, err = c.ReadFrom(b)
	return
}

func (c *UDPConn) Write(b []byte) (n int, err error) {
	n, err = c.WriteTo(b, nil)
	return
}
