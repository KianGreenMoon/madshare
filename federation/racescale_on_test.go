//go:build !nofederation && race

package federation

// testTimeoutScale stretches the mesh test deadlines under the race detector,
// where the embedded yggdrasil core + gVisor netstack run several times slower
// (a friendship handshake or a chunked transfer that takes a second normally
// can take tens of seconds under -race). This only lengthens how long a *failing*
// test waits before giving up; passing checks still return immediately.
const testTimeoutScale = 8
