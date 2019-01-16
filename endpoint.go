// Copyright 2016 The Netstack Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package netstack

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"

	"github.com/google/netstack/tcpip"
	"github.com/google/netstack/tcpip/buffer"
	"github.com/google/netstack/tcpip/network/ipv4"
	"github.com/google/netstack/tcpip/stack"
	"github.com/google/netstack/tcpip/transport/tcp"
	"github.com/google/netstack/tcpip/transport/udp"
)

var id int32

func getID() int32 {
	return atomic.AddInt32(&id, 1)
}

// PacketInfo holds all the information about an outbound packet.
type PacketInfo struct {
	Packet []byte
	Proto  tcpip.NetworkProtocolNumber
}

// Endpoint is link layer endpoint that stores outbound packets in a channel
// and allows injection of inbound packets.
type Endpoint struct {
	id         tcpip.LinkEndpointID
	dispatcher stack.NetworkDispatcher
	linkAddr   tcpip.LinkAddress
	stack      *stack.Stack
	resolver   *resolver
	mtu        uint32
	nicID      tcpip.NICID

	// C is where outbound packets are queued.
	C chan []byte
}

type stackErr struct {
	e *tcpip.Error
}

func (e stackErr) Error() string {
	return e.e.String()
}

func (e *Endpoint) Resolve(ctx context.Context, name string) (net.IP, error) {
	return e.resolver.lookupIPv4(ctx, name)
}

// New creates a new channel endpoint.
func New(cidr string, mtu uint32) (*Endpoint, error) {
	s := stack.New([]string{ipv4.ProtocolName}, []string{tcp.ProtocolName, udp.ProtocolName}, stack.Options{})
	ep := &Endpoint{
		C:        make(chan []byte, 32),
		mtu:      mtu,
		stack:    s,
		linkAddr: "",
		nicID:    tcpip.NICID(getID()),
	}
	ep.id = stack.RegisterLinkEndpoint(ep)
	var err error

	e := s.CreateNIC(ep.nicID, ep.id)
	if e != nil {
		return nil, stackErr{e}
	}

	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("can't parse cidr %v, %v", cidr, err)
	}
	ip = ip.To4()
	if ip == nil {
		return nil, fmt.Errorf("%v is not an IPv4 CIDR", cidr)
	}

	e = s.AddAddress(ep.nicID, ipv4.ProtocolNumber, tcpip.Address(ip))
	if e != nil {
		return nil, stackErr{e}
	}
	s.SetRouteTable([]tcpip.Route{
		{
			Destination: tcpip.Address([]byte{0, 0, 0, 0}),
			Mask:        tcpip.AddressMask([]byte{0, 0, 0, 0}),
			Gateway:     tcpip.Address([]byte{0, 0, 0, 0}),
			NIC:         ep.nicID,
		},
	})

	ep.resolver, err = newResolver("udp", "8.8.8.8:53", ep.Dial)
	if err != nil {
		return nil, err
	}
	return ep, nil
}

// Drain removes all outbound packets from the channel and counts them.
func (e *Endpoint) Drain() int {
	c := 0
	for {
		select {
		case <-e.C:
			c++
		default:
			return c
		}
	}
}

var _ stack.LinkEndpoint = (*Endpoint)(nil)

// Inject injects an inbound packet.
func (e *Endpoint) Inject(packet []byte) {
	vv := buffer.NewVectorisedView(len(packet), []buffer.View{packet})
	e.dispatcher.DeliverNetworkPacket(e, "", "", ipv4.ProtocolNumber, vv)
}

// Attach saves the stack network-layer dispatcher for use later when packets
// are injected.
func (e *Endpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.dispatcher = dispatcher
}

func (e *Endpoint) IsAttached() bool {
	return e.dispatcher != nil
}

// MTU implements stack.LinkEndpoint.MTU. It returns the value initialized
// during construction.
func (e *Endpoint) MTU() uint32 {
	return e.mtu
}

func (e *Endpoint) Capabilities() stack.LinkEndpointCapabilities {
	return 0
}

// MaxHeaderLength returns the maximum size of the link layer header. Given it
// doesn't have a header, it just returns 0.
func (*Endpoint) MaxHeaderLength() uint16 {
	return 0
}

// LinkAddress returns the link address of this endpoint.
func (e *Endpoint) LinkAddress() tcpip.LinkAddress {
	return e.linkAddr
}

// WritePacket stores outbound packets into the channel.
func (e *Endpoint) WritePacket(r *stack.Route, hdr buffer.Prependable, payload buffer.VectorisedView, protocol tcpip.NetworkProtocolNumber) *tcpip.Error {
	if protocol != ipv4.ProtocolNumber {
		return nil
	}
	p := make([]byte, 0, 2048)
	p = append(p, hdr.View()...)
	for _, view := range payload.Views() {
		p = append(p, view...)
	}

	select {
	case e.C <- p:
	default:
	}

	return nil
}

func (e *Endpoint) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4":
		return dialTCP(ctx, e, address)
	case "udp", "udp4":
		return dialUDP(ctx, e, address)
	default:
		return nil, fmt.Errorf("network %v is not supported", network)
	}
}

func (e *Endpoint) Listen(ctx context.Context, network, address string) (net.Listener, error) {
	switch network {
	case "tcp", "tcp4":
		return listenTCP(ctx, e, address)
	default:
		return nil, fmt.Errorf("network %v is not supported", network)
	}
}

func (e *Endpoint) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	switch network {
	case "udp", "udp4":
		return listenUDP(ctx, e, address)
	default:
		return nil, fmt.Errorf("network %v is not supported", network)
	}
}
