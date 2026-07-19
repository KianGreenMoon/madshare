//go:build !nofederation && !race

package federation

// testTimeoutScale multiplies the mesh test deadlines. Normal builds run at 1×.
// The race build (racescale_on_test.go) scales them up because the gVisor
// userspace netstack is several times slower under -race.
const testTimeoutScale = 1
