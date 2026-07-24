package netstack

import (
	"errors"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Arceliar/ironwood/types"
	"github.com/yggdrasil-network/yggdrasil-go/src/core"
	"github.com/yggdrasil-network/yggdrasil-go/src/ipv6rwc"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

type YggdrasilNIC struct {
	stack   *YggdrasilNetstack
	ipv6rwc *ipv6rwc.ReadWriteCloser
	// dispatcher is atomic because tearing the NIC down writes it from a
	// different goroutine than the inbound reader reads it: RemoveNIC calls
	// Attach(nil) (gVisor stack/nic.go), which races the reader's
	// deliverInbound. Upstream stores it as a plain field, which was safe only
	// because nothing ever removed the NIC. LOCAL PATCH (madshare) — see the
	// teardown note above Close().
	dispatcher atomic.Pointer[stack.NetworkDispatcher]
	readBuf    []byte
	// writePool hands each concurrent writePacket call its own MTU-sized scratch
	// buffer. gVisor invokes WritePackets from several goroutines at once, so a
	// single shared writeBuf (the upstream layout) is a data race — see the local
	// patch note at the top of writePacket. The mesh write path
	// (ipv6rwc.Write → keyStore, core.WriteTo) is already mutex-guarded, so
	// per-call buffers are sufficient and keep sends parallel.
	writePool  sync.Pool
	rstPackets chan *stack.PacketBuffer
	// closed is set by Close() so the single inbound reader goroutine
	// (runInboundReader) exits cleanly on shutdown instead of mistaking a
	// deliberate teardown for a recoverable read error. See the LOCAL PATCH
	// note above runInboundReader.
	closed atomic.Bool
	// done is closed by Close() to stop the RST drain goroutine, which upstream
	// starts with no exit path at all. closeOnce guards both, because gVisor can
	// call Close() itself: removeNICLocked hands back ep.Close as a deferred
	// action, so our own Close → RemoveNIC can re-enter here.
	// LOCAL PATCH (madshare) — see the teardown note above Close().
	done      chan struct{}
	closeOnce sync.Once
	// readerAlive is true while the inbound reader goroutine runs; it flips to
	// false in the goroutine's defer when it returns for any reason. Read via
	// YggdrasilNetstack.InboundReaderAlive by the availability watchdog.
	readerAlive atomic.Bool
}

// packetReader is the inbound half of *ipv6rwc.ReadWriteCloser. Declaring it as
// an interface lets runInboundReader be unit-tested with an injected fake reader
// (see yggdrasil_test.go) — the concrete ReadWriteCloser needs a live core.
type packetReader interface {
	Read(p []byte) (int, error)
}

const (
	inboundBackoffMin = 50 * time.Millisecond
	inboundBackoffMax = time.Second
)

// LOCAL PATCH (madshare): inbound-reader resilience — issue #398.
//
// Upstream ran the netstack's ENTIRE inbound path in one goroutine that did
// `for { rx, err := Read(buf); if err != nil { log; break } ... }`. A single
// Read error `break`s the loop for good, so one hiccup permanently kills ALL
// inbound mesh traffic on the node (friend pings, catalog/holdings sync,
// blob/manifest/chunk fetches, and serving other nodes) until the process is
// restarted — a silent single point of failure.
//
// Error characterisation (the issue's prerequisite): ipv6rwc.Read →
// keyStore.readPC → core.ReadFrom → ironwood PacketConn.ReadFrom. The only
// errors that path surfaces are ironwood's types.ErrClosed (PacketConn/core was
// closed — the definitive shutdown signal) and types.ErrTimeout (only if a read
// deadline is set on the PacketConn, which this wrapper never does, so it does
// not occur here). core.ReadFrom manufactures no errors of its own; it filters
// non-traffic frames and otherwise blocks. So in this version the sole reachable
// error is terminal-and-expected: types.ErrClosed at shutdown. This node stops
// the mesh via core.Stop() (federation/node.go), which never calls
// YggdrasilNIC.Close(), so the `closed` flag alone would NOT be set on the
// normal shutdown path — hence ErrClosed must itself be treated as terminal, or
// the loop would backoff-spin forever after Stop().
//
// runInboundReader therefore exits cleanly on (a) our own close signal or
// (b) types.ErrClosed, and treats anything else — nothing today, but any future
// yggstack/ironwood error, injected fault, or unanticipated transient — as
// recoverable: log it and continue after a capped exponential backoff (50 ms →
// 1 s) so a genuinely permanent error cannot hot-spin a CPU core.
//
// Documented in third_party/yggstack/MADSHARE-PATCH.md; prefer upstreaming
// (issue #398 option 5) over carrying a second local patch indefinitely.
func runInboundReader(r packetReader, buf []byte, closing func() bool, deliver func([]byte)) {
	backoff := inboundBackoffMin
	for {
		rx, err := r.Read(buf)
		if err != nil {
			if closing() || errors.Is(err, types.ErrClosed) {
				return // clean shutdown — exiting is correct
			}
			log.Printf("madnetwork: netstack inbound read error (recovering after %s): %v", backoff, err)
			time.Sleep(backoff)
			if backoff *= 2; backoff > inboundBackoffMax {
				backoff = inboundBackoffMax
			}
			continue
		}
		backoff = inboundBackoffMin
		deliver(buf[:rx])
	}
}

func (s *YggdrasilNetstack) NewYggdrasilNIC(ygg *core.Core) tcpip.Error {
	rwc := ipv6rwc.NewReadWriteCloser(ygg)
	mtu := rwc.MTU()
	nic := &YggdrasilNIC{
		// LOCAL PATCH (madshare): upstream never sets this back-reference, so
		// Close()'s RemoveNIC call nil-derefs. Latent upstream because nothing
		// ever called Close().
		stack:      s,
		ipv6rwc:    rwc,
		readBuf:    make([]byte, mtu),
		writePool:  sync.Pool{New: func() any { return make([]byte, mtu) }},
		rstPackets: make(chan *stack.PacketBuffer, 100),
		done:       make(chan struct{}),
	}
	if err := s.stack.CreateNIC(1, nic); err != nil {
		return err
	}
	s.nic = nic
	// Mark alive before launching so InboundReaderAlive can't observe a false
	// "dead" between here and the goroutine starting; the defer flips it back.
	nic.readerAlive.Store(true)
	go func() {
		defer nic.readerAlive.Store(false)
		runInboundReader(nic.ipv6rwc, nic.readBuf, nic.closed.Load, nic.deliverInbound)
	}()
	// LOCAL PATCH (madshare): the RST drain goroutine gets an exit. Upstream
	// loops forever, so every stopped node leaks it — invisible in a process
	// that runs one node until exit, fatal in a test suite that starts dozens.
	go func() {
		for {
			select {
			case <-nic.done:
				// Release anything still queued; WritePackets holds a ref per
				// packet it enqueued.
				for {
					select {
					case pkt := <-nic.rstPackets:
						if pkt != nil {
							pkt.DecRef()
						}
					default:
						return
					}
				}
			case pkt := <-nic.rstPackets:
				if pkt == nil {
					continue
				}
				_ = nic.writePacket(pkt)
				pkt.DecRef()
			}
		}
	}()
	_, snet, err := net.ParseCIDR("0200::/7")
	if err != nil {
		return &tcpip.ErrBadAddress{}
	}
	subnet, err := tcpip.NewSubnet(
		tcpip.AddrFromSlice(snet.IP.To16()),
		tcpip.MaskFrom(string(snet.Mask)),
	)
	if err != nil {
		return &tcpip.ErrBadAddress{}
	}
	s.stack.AddRoute(tcpip.Route{
		Destination: subnet,
		NIC:         1,
	})
	if s.stack.HandleLocal() {
		ip := ygg.Address()
		if err := s.stack.AddProtocolAddress(
			1,
			tcpip.ProtocolAddress{
				Protocol:          ipv6.ProtocolNumber,
				AddressWithPrefix: tcpip.AddrFromSlice(ip.To16()).WithPrefix(),
			},
			stack.AddressProperties{},
		); err != nil {
			return err
		}
	}
	return nil
}

// deliverInbound wraps one received frame and hands it up to the stack. The
// detach that teardown performs (Attach(nil)) can land between any two frames,
// so load the dispatcher once and drop the frame if it is gone — a shutdown
// racing the reader must not panic on a nil DeliverNetworkPacket. A frame that
// wins the race and is delivered to an already-removed NIC is dropped safely by
// gVisor itself (nic.DeliverNetworkPacket returns early unless the NIC is
// enabled).
func (e *YggdrasilNIC) deliverInbound(b []byte) {
	d := e.dispatcher.Load()
	if d == nil {
		return
	}
	pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(b),
	})
	(*d).DeliverNetworkPacket(ipv6.ProtocolNumber, pkb)
	pkb.DecRef()
}

// Attach stores the dispatcher; gVisor passes nil to detach when the NIC is
// removed, which is why this is a typed atomic rather than a plain field.
func (e *YggdrasilNIC) Attach(dispatcher stack.NetworkDispatcher) {
	if dispatcher == nil {
		e.dispatcher.Store(nil)
		return
	}
	e.dispatcher.Store(&dispatcher)
}

func (e *YggdrasilNIC) IsAttached() bool { return e.dispatcher.Load() != nil }

func (e *YggdrasilNIC) MTU() uint32 { return uint32(e.ipv6rwc.MTU()) }

func (e *YggdrasilNIC) SetMTU(uint32) {}

func (*YggdrasilNIC) Capabilities() stack.LinkEndpointCapabilities { return stack.CapabilityNone }

func (*YggdrasilNIC) MaxHeaderLength() uint16 { return 40 }

func (*YggdrasilNIC) LinkAddress() tcpip.LinkAddress { return "" }

func (*YggdrasilNIC) SetLinkAddress(tcpip.LinkAddress) {}

func (*YggdrasilNIC) Wait() {}

func (e *YggdrasilNIC) writePacket(
	pkt *stack.PacketBuffer,
) tcpip.Error {
	// We need to recover from panic() here because
	// parser in ToView() gets confused on some packets
	// without payload and panics
	defer func() {
		r := recover()
		if r != nil {
		}
	}()
	// LOCAL PATCH (madshare): the upstream code read into a single shared
	// e.writeBuf, which races when gVisor drives WritePackets concurrently
	// (multiple TCP connections at once — e.g. the madnetwork swarm's parallel
	// chunk fetches). Take a per-call buffer from the pool instead.
	writeBuf := e.writePool.Get().([]byte)
	defer e.writePool.Put(writeBuf)
	vv := pkt.ToView()
	n, err := vv.Read(writeBuf)
	if err != nil {
		return &tcpip.ErrAborted{}
	}
	_, err = e.ipv6rwc.Write(writeBuf[:n])
	if err != nil {
		return &tcpip.ErrAborted{}
	}
	return nil
}

func (e *YggdrasilNIC) WritePackets(
	list stack.PacketBufferList,
) (int, tcpip.Error) {
	var i int = 0
	var err tcpip.Error = nil
	for i, pkt := range list.AsSlice() {
		if pkt.Data().Size() == 0 {
			if pkt.Network().TransportProtocol() == tcp.ProtocolNumber {
				tcpHeader := header.TCP(pkt.TransportHeader().Slice())
				if (tcpHeader.Flags() & header.TCPFlagRst) == header.TCPFlagRst {
					// LOCAL PATCH (madshare): once the NIC is closing nothing
					// drains rstPackets, so queueing here would strand the ref.
					// A packet that slips through a concurrent Close is bounded
					// by the channel's capacity and released when the drain
					// goroutine empties the queue on its way out.
					if e.closed.Load() {
						continue
					}
					pkt.IncRef()
					select {
					case e.rstPackets <- pkt:
						// Packet queued successfully
					default:
						// Channel full, drop packet and release ref
						pkt.DecRef()
					}
					continue
				}
			}
		}
		err = e.writePacket(pkt)
		if err != nil {
			log.Println(err)
			return i - 1, err
		}
	}

	return i, nil
}

func (e *YggdrasilNIC) WriteRawPacket(*stack.PacketBuffer) tcpip.Error {
	panic("not implemented")
}

func (*YggdrasilNIC) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}

func (e *YggdrasilNIC) AddHeader(*stack.PacketBuffer) {
}

func (e *YggdrasilNIC) ParseHeader(*stack.PacketBuffer) bool {
	return true
}

// Close tears the NIC down. LOCAL PATCH (madshare): upstream's version leaves
// the RST drain goroutine running and nils the dispatcher with a plain write.
//
// Two things make this re-entrant, so both are guarded by closeOnce: gVisor's
// removeNICLocked returns ep.Close as a deferred action, so RemoveNIC below can
// call straight back into here; and YggdrasilNetstack.Close calls it directly.
// The second pass finds the NIC already gone and RemoveNIC reports
// ErrUnknownNICID, which is the expected outcome, not a failure.
//
// The inbound reader is NOT waited for here. It is parked in ipv6rwc.Read until
// the yggdrasil core stops, and nothing this method does can wake it; it exits
// on its own once the caller stops the core (see runInboundReader, which treats
// types.ErrClosed as terminal). Frames it delivers in the meantime are dropped
// safely — see deliverInbound.
func (e *YggdrasilNIC) Close() {
	e.closeOnce.Do(func() {
		// Signal the inbound reader (runInboundReader) to exit cleanly rather
		// than treat the teardown as a recoverable read error, and stop the RST
		// drain goroutine.
		e.closed.Store(true)
		close(e.done)
	})
	// Removing the NIC detaches the dispatcher (gVisor calls Attach(nil)) and
	// terminates the qDisc goroutines.
	e.stack.stack.RemoveNIC(1)
}

func (e *YggdrasilNIC) SetOnCloseAction(func()) {}
