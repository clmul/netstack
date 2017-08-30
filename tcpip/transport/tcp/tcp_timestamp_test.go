package tcp_test

import (
	"bytes"
	"math/rand"
	"testing"
	"time"

	"github.com/google/netstack/tcpip"
	"github.com/google/netstack/tcpip/buffer"
	"github.com/google/netstack/tcpip/checker"
	"github.com/google/netstack/tcpip/header"
	"github.com/google/netstack/tcpip/network/ipv4"
	"github.com/google/netstack/tcpip/seqnum"
	"github.com/google/netstack/tcpip/transport/tcp"
	"github.com/google/netstack/waiter"
)

// defaultWindowScale value specified here depends on the tcp.DefaultBufferSize
// constant defined in the endpoint.go because the tcp.DefaultBufferSize is used
// in tcp.newHandshake to determine the window scale to use when sending a
// SYN/SYN-ACK.
var defaultWindowScale = tcp.FindWndScale(tcp.DefaultBufferSize)

// createConnectedWithTimestampOption creates and connects c.ep with the
// timestamp option enabled.
func createConnectedWithTimestampOption(c *testContext) *rawEndpoint {
	return createConnectedWithOptions(c, header.TCPSynOptions{TS: true, TSVal: 1})
}

// createConnectedWithOptions creates and connects c.ep with the
// specified TCP options enabled and returns a rawEndpoint which
// represents the other end of the connection.
//
// It also verifies where required(eg.Timestamp) that the ACK to the
// SYN-ACK does not carry an option that was not requested.
func createConnectedWithOptions(c *testContext, wantOptions header.TCPSynOptions) *rawEndpoint {
	var err *tcpip.Error
	c.ep, err = c.s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &c.wq)
	if err != nil {
		c.t.Fatalf("c.s.NewEndpoint(tcp, ipv4...) = %v", err)
	}

	// Start connection attempt.
	waitEntry, notifyCh := waiter.NewChannelEntry(nil)
	c.wq.EventRegister(&waitEntry, waiter.EventOut)
	defer c.wq.EventUnregister(&waitEntry)

	testFullAddr := tcpip.FullAddress{Addr: testAddr, Port: testPort}
	err = c.ep.Connect(testFullAddr)
	if err != tcpip.ErrConnectStarted {
		c.t.Fatalf("c.ep.Connect(%v) = %v", testFullAddr, err)
	}
	// Receive SYN packet.
	b := c.getPacket()
	// Validate that the syn has the timestamp option and a valid
	// TS value.
	checker.IPv4(c.t, b,
		checker.TCP(
			checker.DstPort(testPort),
			checker.TCPFlags(header.TCPFlagSyn),
			checker.TCPSynOptions(header.TCPSynOptions{
				MSS: uint16(c.linkEP.MTU() - header.IPv4MinimumSize - header.TCPMinimumSize),
				TS:  true,
				WS:  defaultWindowScale,
			}),
		),
	)
	tcpSeg := header.TCP(header.IPv4(b).Payload())
	synOptions := header.ParseSynOptions(tcpSeg.Options(), false)

	// Build options w/ tsVal to be sent in the SYN-ACK.
	var synAckOptions []byte
	if wantOptions.TS {
		tsOpt := header.EncodeTSOption(wantOptions.TSVal, synOptions.TSVal)
		synAckOptions = append(synAckOptions, tsOpt[:]...)
	}

	// Build SYN-ACK.
	c.irs = seqnum.Value(tcpSeg.SequenceNumber())
	iss := seqnum.Value(789)
	c.sendPacket(nil, &headers{
		srcPort: tcpSeg.DestinationPort(),
		dstPort: tcpSeg.SourcePort(),
		flags:   header.TCPFlagSyn | header.TCPFlagAck,
		seqNum:  iss,
		ackNum:  c.irs.Add(1),
		rcvWnd:  30000,
		tcpOpts: synAckOptions[:],
	})

	// Read ACK.
	ackPacket := c.getPacket()

	// Verify TCP header fields.
	tcpCheckers := []checker.TransportChecker{
		checker.DstPort(testPort),
		checker.TCPFlags(header.TCPFlagAck),
		checker.SeqNum(uint32(c.irs) + 1),
		checker.AckNum(uint32(iss) + 1),
	}

	// Verify that tsEcr of ACK packet is wantOptions.TSVal if the
	// timestamp option was enabled, if not then we verify that
	// there is no timestamp in the ACK packet.
	if wantOptions.TS {
		tcpCheckers = append(tcpCheckers, checker.TCPTimestampChecker(true, 0, wantOptions.TSVal))
	} else {
		tcpCheckers = append(tcpCheckers, checker.TCPTimestampChecker(false, 0, 0))
	}

	checker.IPv4(c.t, ackPacket, checker.TCP(tcpCheckers...))

	ackSeg := header.TCP(header.IPv4(ackPacket).Payload())
	ackOptions := ackSeg.ParsedOptions()

	// Wait for connection to be established.
	select {
	case <-notifyCh:
		err = c.ep.GetSockOpt(tcpip.ErrorOption{})
		if err != nil {
			c.t.Fatalf("Unexpected error when connecting: %v", err)
		}
	case <-time.After(1 * time.Second):
		c.t.Fatalf("Timed out waiting for connection")
	}

	// Store the source port in use by the endpoint.
	c.port = tcpSeg.SourcePort()
	// Mark in context that timestamp option is enabled for this endpoint.
	c.timeStampEnabled = true

	return &rawEndpoint{
		c:          c,
		srcPort:    tcpSeg.DestinationPort(),
		dstPort:    tcpSeg.SourcePort(),
		flags:      header.TCPFlagAck | header.TCPFlagPsh,
		nextSeqNum: iss + 1,
		ackNum:     c.irs.Add(1),
		wndSize:    30000,
		recentTS:   ackOptions.TSVal,
		tsVal:      wantOptions.TSVal,
	}
}

// TestTimeStampEnabledConnect tests that netstack sends the timestamp option on
// an active connect and sets the TS Echo Reply fields correctly when the
// SYN-ACK also indicates support for the TS option and provides a TSVal.
func TestTimeStampEnabledConnect(t *testing.T) {
	c := newTestContext(t, defaultMTU)
	defer c.cleanup()

	rep := createConnectedWithTimestampOption(c)

	// Register for read and validate that we have data to read.
	we, ch := waiter.NewChannelEntry(nil)
	c.wq.EventRegister(&we, waiter.EventIn)
	defer c.wq.EventUnregister(&we)

	// The following tests ensure that TS option once enabled behaves
	// correctly as described in
	// https://tools.ietf.org/html/rfc7323#section-4.3.
	//
	// We are not testing delayed ACKs here, but we do test out of order
	// packet delivery and filling the sequence number hole created due to
	// the out of order packet.
	//
	// The test also verifies that the sequence numbers and timestamps are
	// as expected.
	data := []byte{1, 2, 3}

	// First we increment tsVal by a small amount.
	tsVal := rep.tsVal + 100
	rep.sendPacketWithTS(data, tsVal)
	rep.verifyACKWithTS(tsVal)

	// Next we send an out of order packet.
	rep.nextSeqNum += 3
	tsVal += 200
	rep.sendPacketWithTS(data, tsVal)

	// The ACK should contain the original sequenceNumber and an older TS.
	rep.nextSeqNum -= 6
	rep.verifyACKWithTS(tsVal - 200)

	// Next we fill the hole and the returned ACK should contain the
	// cumulative sequence number acking all data sent till now and have the
	// latest timestamp sent below in its TSEcr field.
	tsVal -= 100
	rep.sendPacketWithTS(data, tsVal)
	rep.nextSeqNum += 3
	rep.verifyACKWithTS(tsVal)

	// Increment tsVal by a large value that doesn't result in a wrap around.
	tsVal += 0x7fffffff
	rep.sendPacketWithTS(data, tsVal)
	rep.verifyACKWithTS(tsVal)

	// Increment tsVal again by a large value which should cause the
	// timestamp value to wrap around. The returned ACK should contain the
	// wrapped around timestamp in its tsEcr field and not the tsVal from
	// the previous packet sent above.
	tsVal += 0x7fffffff
	rep.sendPacketWithTS(data, tsVal)
	rep.verifyACKWithTS(tsVal)

	select {
	case <-ch:
	case <-time.After(1 * time.Second):
		t.Fatalf("Timed out waiting for data to arrive")
	}

	// There should be 5 views to read and each of them should
	// contain the same data.
	for i := 0; i < 5; i++ {
		got, err := c.ep.Read(nil)
		if err != nil {
			t.Fatalf("Unexpected error from Read: %v", err)
		}
		if want := data; bytes.Compare(got, want) != 0 {
			t.Fatalf("Data is different: got: %v, want: %v", got, want)
		}
	}
}

// rawEndpoint is just a small wrapper around a TCP endpoint's state to
// make sending data and ACK packets easily while being able to manipulate
// the sequence numbers and timestamp values as needed.
type rawEndpoint struct {
	c          *testContext
	srcPort    uint16
	dstPort    uint16
	flags      int
	nextSeqNum seqnum.Value
	ackNum     seqnum.Value
	wndSize    seqnum.Size
	recentTS   uint32 // Stores the latest timestamp to echo back.
	tsVal      uint32 // tsVal stores the last timestamp sent by this endpoint.
}

// sendPacketWithTS embeds the provided tsVal in the Timestamp option
// for the packet to be sent out.
func (r *rawEndpoint) sendPacketWithTS(payload []byte, tsVal uint32) {
	r.tsVal = tsVal
	// Increment TSVal by 1 from the value sent in the SYN and echo the
	// TSVal in the SYN-ACK in the TSEcr field.
	tsOpt := header.EncodeTSOption(r.tsVal, r.recentTS)
	r.sendPacket(payload, tsOpt[:])
}

// sendPacket is a small wrapper function to build and send packets.
func (r *rawEndpoint) sendPacket(payload []byte, opts []byte) {
	packetHeaders := &headers{
		srcPort: r.srcPort,
		dstPort: r.dstPort,
		flags:   r.flags,
		seqNum:  r.nextSeqNum,
		ackNum:  r.ackNum,
		rcvWnd:  r.wndSize,
		tcpOpts: opts,
	}
	r.c.sendPacket(payload, packetHeaders)
	r.nextSeqNum = r.nextSeqNum.Add(seqnum.Size(len(payload)))
}

// verifyACKWithTS verifies that the tsEcr field in the ack matches the provided
// tsVal.
func (r *rawEndpoint) verifyACKWithTS(tsVal uint32) {
	// Read ACK and verify that tsEcr of ACK packet is [1,2,3,4]
	ackPacket := r.c.getPacket()
	checker.IPv4(r.c.t, ackPacket,
		checker.TCP(
			checker.DstPort(r.srcPort),
			checker.TCPFlags(header.TCPFlagAck),
			checker.SeqNum(uint32(r.ackNum)),
			checker.AckNum(uint32(r.nextSeqNum)),
			checker.TCPTimestampChecker(true, 0, tsVal),
		),
	)
	// Store the parsed TSVal from the ack as recentTS.
	tcpSeg := header.TCP(header.IPv4(ackPacket).Payload())
	opts := tcpSeg.ParsedOptions()
	r.recentTS = opts.TSVal
}

// TestTimeStampDisabledConnect tests that netstack sends timestamp option on an
// active connect but if the SYN-ACK doesn't specify the TS option then
// timestamp option is not enabled and future packets do not contain a
// timestamp.
func TestTimeStampDisabledConnect(t *testing.T) {
	c := newTestContext(t, defaultMTU)
	defer c.cleanup()

	createConnectedWithOptions(c, header.TCPSynOptions{})
}

func timeStampEnabledAccept(t *testing.T, cookieEnabled bool, wndScale int, wndSize uint16) {
	savedSynCountThreshold := tcp.SynRcvdCountThreshold
	defer func() {
		tcp.SynRcvdCountThreshold = savedSynCountThreshold
	}()

	if cookieEnabled {
		tcp.SynRcvdCountThreshold = 0
	}
	c := newTestContext(t, defaultMTU)
	defer c.cleanup()

	c.t.Logf("Test w/ CookieEnabled = %v", cookieEnabled)
	// Create EP and start listening.
	wq := &waiter.Queue{}
	ep, err := c.s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, wq)
	if err != nil {
		c.t.Fatalf("NewEndpoint failed: %v", err)
	}
	defer ep.Close()

	if err := ep.Bind(tcpip.FullAddress{Port: stackPort}, nil); err != nil {
		c.t.Fatalf("Bind failed: %v", err)
	}

	if err := ep.Listen(10); err != nil {
		c.t.Fatalf("Listen failed: %v", err)
	}

	tsVal := rand.Uint32()
	passiveConnectWithOptions(c, 100, wndScale, header.TCPSynOptions{MSS: defaultIPv4MSS, TS: true, TSVal: tsVal})

	// Try to accept the connection.
	we, ch := waiter.NewChannelEntry(nil)
	wq.EventRegister(&we, waiter.EventIn)
	defer wq.EventUnregister(&we)

	c.ep, _, err = ep.Accept()
	if err == tcpip.ErrWouldBlock {
		// Wait for connection to be established.
		select {
		case <-ch:
			c.ep, _, err = ep.Accept()
			if err != nil {
				c.t.Fatalf("Accept failed: %v", err)
			}

		case <-time.After(1 * time.Second):
			c.t.Fatalf("Timed out waiting for accept")
		}
	}

	data := []byte{1, 2, 3}
	view := buffer.NewView(len(data))
	copy(view, data)

	if _, err := c.ep.Write(view, nil); err != nil {
		t.Fatalf("Unexpected error from Write: %v", err)
	}

	// Check that data is received and that the timestamp option TSEcr field
	// matches the expected value.
	b := c.getPacket()
	checker.IPv4(c.t, b,
		// Add 12 bytes for the timestamp option + 2 NOPs to align at 4
		// byte boundary.
		checker.PayloadLen(len(data)+header.TCPMinimumSize+12),
		checker.TCP(
			checker.DstPort(testPort),
			checker.SeqNum(uint32(c.irs)+1),
			checker.AckNum(790),
			checker.Window(wndSize),
			checker.TCPFlagsMatch(header.TCPFlagAck, ^uint8(header.TCPFlagPsh)),
			checker.TCPTimestampChecker(true, 0, tsVal+1),
		),
	)
}

// TestTimeStampEnabledAccept tests that if the SYN on a passive connect
// specifies the Timestamp option then the Timestamp option is sent on a SYN-ACK
// and echoes the tsVal field of the original SYN in the tcEcr field of the
// SYN-ACK. We cover the cases where SYN cookies are enabled/disabled and verify
// that Timestamp option is enabled in both cases if requested in the original
// SYN.
func TestTimeStampEnabledAccept(t *testing.T) {
	testCases := []struct {
		cookieEnabled bool
		wndScale      int
		wndSize       uint16
	}{
		{true, -1, 0xffff}, // When cookie is used window scaling is disabled.
		{false, 2, 0xd000},
	}
	for _, tc := range testCases {
		timeStampEnabledAccept(t, tc.cookieEnabled, tc.wndScale, tc.wndSize)
	}
}

func timeStampDisabledAccept(t *testing.T, cookieEnabled bool, wndScale int, wndSize uint16) {
	savedSynCountThreshold := tcp.SynRcvdCountThreshold
	defer func() {
		tcp.SynRcvdCountThreshold = savedSynCountThreshold
	}()
	if cookieEnabled {
		tcp.SynRcvdCountThreshold = 0
	}

	c := newTestContext(t, defaultMTU)
	defer c.cleanup()

	c.t.Logf("Test w/ CookieEnabled = %v", cookieEnabled)
	// Create EP and start listening.
	wq := &waiter.Queue{}
	ep, err := c.s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, wq)
	if err != nil {
		c.t.Fatalf("NewEndpoint failed: %v", err)
	}
	defer ep.Close()

	if err := ep.Bind(tcpip.FullAddress{Port: stackPort}, nil); err != nil {
		c.t.Fatalf("Bind failed: %v", err)
	}

	if err := ep.Listen(10); err != nil {
		c.t.Fatalf("Listen failed: %v", err)
	}

	// Do 3-way handshake/w no timestamp option.
	passiveConnectWithOptions(c, 100, wndScale, header.TCPSynOptions{MSS: defaultIPv4MSS})

	// Try to accept the connection.
	we, ch := waiter.NewChannelEntry(nil)
	wq.EventRegister(&we, waiter.EventIn)
	defer wq.EventUnregister(&we)

	c.ep, _, err = ep.Accept()
	if err == tcpip.ErrWouldBlock {
		// Wait for connection to be established.
		select {
		case <-ch:
			c.ep, _, err = ep.Accept()
			if err != nil {
				c.t.Fatalf("Accept failed: %v", err)
			}

		case <-time.After(1 * time.Second):
			c.t.Fatalf("Timed out waiting for accept")
		}
	}

	data := []byte{1, 2, 3}
	view := buffer.NewView(len(data))
	copy(view, data)

	if _, err := c.ep.Write(view, nil); err != nil {
		t.Fatalf("Unexpected error from Write: %v", err)
	}

	// Check that data is received and that the timestamp option is disabled
	// when SYN cookies are enabled/disabled.
	b := c.getPacket()
	checker.IPv4(c.t, b,
		checker.PayloadLen(len(data)+header.TCPMinimumSize),
		checker.TCP(
			checker.DstPort(testPort),
			checker.SeqNum(uint32(c.irs)+1),
			checker.AckNum(790),
			checker.Window(wndSize),
			checker.TCPFlagsMatch(header.TCPFlagAck, ^uint8(header.TCPFlagPsh)),
			checker.TCPTimestampChecker(false, 0, 0),
		),
	)
}

// TestTimeStampDisabledAccept tests that Timestamp option is not used when the
// peer doesn't advertise it and connection is established with Accept().
func TestTimeStampDisabledAccept(t *testing.T) {
	testCases := []struct {
		cookieEnabled bool
		wndScale      int
		wndSize       uint16
	}{
		{true, -1, 0xffff}, // When cookie is used window scaling is disabled.
		{false, 2, 0xd000},
	}
	for _, tc := range testCases {
		timeStampDisabledAccept(t, tc.cookieEnabled, tc.wndScale, tc.wndSize)
	}
}

func TestSendGreaterThanMTUWithOptions(t *testing.T) {
	const maxPayload = 100
	c := newTestContext(t, uint32(header.TCPMinimumSize+header.IPv4MinimumSize+maxPayload))
	defer c.cleanup()

	createConnectedWithTimestampOption(c)
	testBrokenUpWrite(c, maxPayload)
}

func TestSegmentDropWhenTimestampMissing(t *testing.T) {
	const maxPayload = 100
	c := newTestContext(t, uint32(header.TCPMinimumSize+header.IPv4MinimumSize+maxPayload))
	defer c.cleanup()

	rep := createConnectedWithTimestampOption(c)

	// Register for read.
	we, ch := waiter.NewChannelEntry(nil)
	c.wq.EventRegister(&we, waiter.EventIn)
	defer c.wq.EventUnregister(&we)

	droppedPackets := c.s.Stats().DroppedPackets
	data := []byte{1, 2, 3}
	// Save the sequence number as we will reset it later down
	// in the test.
	savedSeqNum := rep.nextSeqNum
	rep.sendPacket(data, nil)

	select {
	case <-ch:
		t.Fatalf("Got data to read when we expect packet to be dropped")
	case <-time.After(1 * time.Second):
		// We expect that no data will be available to read.
	}

	// Assert that DroppedPackets was incremented by 1.
	if got, want := c.s.Stats().DroppedPackets, droppedPackets+1; got != want {
		t.Fatalf("incorrect number of dropped packets, got: %v, want: %v", got, want)
	}

	droppedPackets = c.s.Stats().DroppedPackets
	// Reset the sequence number so that the other endpoint accepts
	// this segment and does not treat it like an out of order delivery.
	rep.nextSeqNum = savedSeqNum
	// Now send a packet with timestamp and we should get the data.
	rep.sendPacketWithTS(data, rep.tsVal+1)

	select {
	case <-ch:
	case <-time.After(1 * time.Second):
		t.Fatalf("Timed out waiting for data to arrive")
	}

	// Assert that DroppedPackets was not incremented by 1.
	if got, want := c.s.Stats().DroppedPackets, droppedPackets; got != want {
		t.Fatalf("incorrect number of dropped packets, got: %v, want: %v", got, want)
	}

	// Issue a read and we should get data.
	got, err := c.ep.Read(nil)
	if err != nil {
		t.Fatalf("Unexpected error from Read: %v", err)
	}
	if want := data; bytes.Compare(got, want) != 0 {
		t.Fatalf("Data is different: got: %v, want: %v", got, want)
	}
}