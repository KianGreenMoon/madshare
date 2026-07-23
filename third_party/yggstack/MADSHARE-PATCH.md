# Local yggstack fork — madshare patch

This is a vendored copy of
[`github.com/yggdrasil-network/yggstack`](https://github.com/yggdrasil-network/yggstack)
at pseudo-version `v0.0.0-20260619214331-c39db65e5bcc`, wired into the build by a
`replace` directive in the repository-root `go.mod`. It carries **two local
changes** on top of upstream, both in `src/netstack/yggdrasil.go`. If you
re-vendor or bump yggstack, **re-apply both patches** (or drop the fork entirely
once the fixes are upstream). Grep for `LOCAL PATCH (madshare)` to find them.

1. **Data race in `YggdrasilNIC.writePacket`** (below).
2. **Inbound-reader resilience — issue #398** (further below): a single
   `ipv6rwc.Read` error no longer permanently kills all inbound mesh traffic.

## The patch — data race in `YggdrasilNIC.writePacket`

File: `src/netstack/yggdrasil.go`

Upstream reads every outbound packet into a **single shared `writeBuf`** field on
the NIC:

```go
type YggdrasilNIC struct {
    ...
    writeBuf []byte   // shared across all writePacket calls
}

func (e *YggdrasilNIC) writePacket(pkt *stack.PacketBuffer) tcpip.Error {
    vv := pkt.ToView()
    n, err := vv.Read(e.writeBuf)          // race
    _, err = e.ipv6rwc.Write(e.writeBuf[:n])
}
```

gVisor drives `WritePackets` from **several goroutines concurrently** (one per TCP
connection / processor). With a single active connection the calls effectively
serialize, so the shared buffer never overlaps — which is why upstream and our
own F0–F3 rarely saw it. The **madnetwork swarm (F4)** opens many mesh
connections at once (parallel chunk fetches), so multiple goroutines run
`writePacket` simultaneously and race on `writeBuf`. `go test -race ./federation/`
reports it as a `WARNING: DATA RACE` inside `buffer.View.Read` / `core.WriteTo`.

The fix gives each call its own buffer from a `sync.Pool` (MTU-sized, so no
per-packet allocation). The mesh write path below it — `ipv6rwc.Write` →
`keyStore` → `core.WriteTo` — is already mutex-guarded, so per-call buffers are
sufficient and keep sends parallel:

```go
type YggdrasilNIC struct {
    ...
    writePool sync.Pool   // replaces writeBuf
}
// constructor: writePool: sync.Pool{New: func() any { return make([]byte, mtu) }}

func (e *YggdrasilNIC) writePacket(pkt *stack.PacketBuffer) tcpip.Error {
    writeBuf := e.writePool.Get().([]byte)
    defer e.writePool.Put(writeBuf)
    vv := pkt.ToView()
    n, err := vv.Read(writeBuf)
    _, err = e.ipv6rwc.Write(writeBuf[:n])
}
```

The buffer is returned to the pool only after `ipv6rwc.Write` returns — the same
"buffer free to reuse after Write" assumption upstream already relied on when it
reused `writeBuf` on the very next call, minus the cross-goroutine sharing.

Grep for `LOCAL PATCH (madshare)` in `src/netstack/yggdrasil.go` to find the
exact lines. Design context: `docs/architecture/federation.md` §Identity &
transport. The rest of the tree is an unmodified copy (LICENSE preserved).

## The patch — inbound-reader resilience (issue #398)

File: `src/netstack/yggdrasil.go`

Upstream runs the netstack's **entire inbound path in one goroutine** whose loop
`break`s on the first read error:

```go
go func() {
    for {
        rx, err := nic.ipv6rwc.Read(nic.readBuf)
        if err != nil {
            log.Println(err)
            break            // kills ALL inbound mesh traffic, forever
        }
        pkb := stack.NewPacketBuffer(...)
        nic.dispatcher.DeliverNetworkPacket(ipv6.ProtocolNumber, pkb)
        pkb.DecRef()
    }
}()
```

One `Read` error ends the loop permanently, so a single hiccup stops packet
delivery for the **whole node** — friend pings, catalog/holdings sync,
blob/manifest/chunk fetches, and serving other nodes all hang until the process
restarts. Federation dies silently while the rest of madshare keeps serving; the
only trace is one `log.Println`. This is upstream behaviour, logged as issue #398
(`.issues/open-issues.md`) and the netstack half of the madnetwork availability
plan (`docs/plans/availability.md` §Phase 0).

### Error characterisation (the prerequisite)

`ipv6rwc.ReadWriteCloser.Read` → `keyStore.readPC` → `core.ReadFrom` → ironwood
`PacketConn.ReadFrom`. Following that chain in the pinned deps
(`yggdrasil-go@v0.5.14`, `ironwood@…d50055b11f5e`):

- **`PacketConn.ReadFrom`** returns exactly two error values: `types.ErrClosed`
  (the `pc.closed` channel is closed — the PacketConn/core was shut down) and
  `types.ErrTimeout` (a read-deadline cancel fired). It otherwise blocks on
  `<-pc.recv` and returns a `nil` error.
- **`core.ReadFrom`** manufactures **no** errors of its own — it propagates the
  `PacketConn.ReadFrom` error verbatim and otherwise loops, filtering
  non-traffic frames.
- The netstack wrapper **never sets a read deadline** on this PacketConn, so
  `ErrTimeout` cannot occur here.

**Conclusion:** in this version the *only* error `ipv6rwc.Read` can surface is
`types.ErrClosed`, and it happens precisely at shutdown. It is
terminal-and-expected. Everything else is unreachable today, but the whole point
of the fix is to survive an *unanticipated* future error rather than die on it.

One code fact drives the design: this node stops the mesh via `core.Stop()`
(`federation/node.go` `Node.Stop`), which does **not** call
`YggdrasilNIC.Close()`. So on the normal shutdown path our own close flag is
never set — the reader learns of shutdown only via `types.ErrClosed`. Treating
`ErrClosed` as terminal is therefore required, not just defensive; without it the
reader would backoff-spin forever after `Stop()`.

### The fix

The loop (`runInboundReader`, now a standalone, unit-tested function) exits
cleanly on **(a)** an explicit close signal — a new `closed atomic.Bool` on the
NIC, set by `Close()` — **or (b)** `errors.Is(err, types.ErrClosed)`. Any other
error is treated as **transient**: log it and continue after a **capped
exponential backoff** (`inboundBackoffMin` 50 ms, doubling, `inboundBackoffMax`
1 s) so a genuinely permanent error cannot hot-spin a CPU core. Delivery reads
the dispatcher once into a local and nil-guards it (`Close()` sets
`dispatcher = nil`) so a shutdown race can't panic on `DeliverNetworkPacket`.

```go
func runInboundReader(r packetReader, buf []byte, closing func() bool, deliver func([]byte)) {
    backoff := inboundBackoffMin
    for {
        rx, err := r.Read(buf)
        if err != nil {
            if closing() || errors.Is(err, types.ErrClosed) {
                return
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
```

`packetReader` is a one-method interface (`Read([]byte) (int, error)`) that
`*ipv6rwc.ReadWriteCloser` already satisfies; it exists so the loop can be tested
with an injected fake reader (`src/netstack/yggdrasil_test.go`) — the concrete
type needs a live core. The tests cover: a transient error recovers and later
packets still deliver; `ErrClosed` exits cleanly with the flag unset; the close
flag terminates the loop even on a non-`ErrClosed` error; the backoff actually
delays a retry; and the liveness flag flips true→false across the reader's life.

### Liveness accessor for the availability watchdog

Alongside the reader fix, the NIC carries a second `atomic.Bool`, `readerAlive`,
set true just before the reader goroutine launches and cleared in the
goroutine's `defer` when it returns (for any reason). `YggdrasilNetstack` now
keeps a reference to its NIC and exposes:

```go
func (s *YggdrasilNetstack) InboundReaderAlive() bool
```

which reports whether that goroutine is still running (false once it has exited
on Close() or a terminal error). The madshare availability feature wires
`node.readerAlive = stack.InboundReaderAlive` so the browse layer can **fail
open** — stop hiding friends' tracks — when the local inbound mesh path is dead.
This is part of the same local patch (`docs/plans/availability.md` §Phase 0/1).

### Upstreaming (preferred)

Each local patch raises the cost of bumping yggstack. The right long-term home is
upstream yggstack (issue #398 option 5); the independent availability watchdog
(`docs/plans/availability.md` Phase 1) is the local safety net meanwhile. Prefer
upstreaming this over carrying it indefinitely.
