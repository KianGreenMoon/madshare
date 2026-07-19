module daemonlord.ygg/madshare

go 1.26.1

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/dhowden/tag v0.0.0-20240417053706-3d75831295e8
	github.com/disintegration/imaging v1.6.2
	github.com/go-chi/chi/v5 v5.2.5
	github.com/yggdrasil-network/yggdrasil-go v0.5.14
	github.com/yggdrasil-network/yggstack v0.0.0-20260619214331-c39db65e5bcc
	golang.org/x/crypto v0.53.0
	golang.org/x/text v0.38.0
	modernc.org/sqlite v1.50.1
)

// Local fork of the yggstack netstack wrapper carrying one patch: a data-race
// fix in YggdrasilNIC.writePacket (per-call write buffer instead of a shared
// one), needed because the madnetwork swarm (F4) drives many concurrent mesh
// connections. See third_party/yggstack/src/netstack/yggdrasil.go (LOCAL PATCH)
// and docs/architecture/federation.md §Distribution. Drop this replace if the
// fix lands upstream.
replace github.com/yggdrasil-network/yggstack => ./third_party/yggstack

require (
	github.com/Arceliar/ironwood v0.0.0-20260613025018-d50055b11f5e // indirect
	github.com/Arceliar/phony v0.0.0-20220903101357-530938a4b13d // indirect
	github.com/bits-and-blooms/bitset v1.24.5 // indirect
	github.com/bits-and-blooms/bloom/v3 v3.7.1 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/gologme/log v1.3.0 // indirect
	github.com/google/btree v1.1.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hjson/hjson-go/v4 v4.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/quic-go/quic-go v0.60.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/image v0.0.0-20191009234506-e7c1f5e7dbb8 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/time v0.7.0 // indirect
	gvisor.dev/gvisor v0.0.0-20250812171554-968e93457fe6 // indirect
	modernc.org/libc v1.72.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
