# Local yggstack fork — madshare patch

This is a vendored copy of
[`github.com/yggdrasil-network/yggstack`](https://github.com/yggdrasil-network/yggstack)
at pseudo-version `v0.0.0-20260619214331-c39db65e5bcc`, wired into the build by a
`replace` directive in the repository-root `go.mod`. It carries **one local
change** on top of upstream. If you re-vendor or bump yggstack, **re-apply this
patch** (or drop the fork entirely once the fix is upstream).

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
