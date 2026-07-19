//go:build !nofederation

package federation

import "time"

// Mesh test deadlines, scaled by testTimeoutScale (1× normally, larger under
// -race). meshDeadline bounds convergence/transfer waits; meshClientTimeout
// bounds a single mesh HTTP probe. Scaling only lengthens how long a failing
// test waits — passing checks return as soon as the condition holds.
const (
	meshDeadline      = 30 * time.Second * testTimeoutScale
	meshClientTimeout = 5 * time.Second * testTimeoutScale
)
