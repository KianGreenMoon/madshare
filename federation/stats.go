//go:build !nofederation

package federation

import (
	"sync"
	"time"
)

// Transfer diagnostics (mesh-testing T1): the mutable counters behind the
// [TransferStats] snapshot. Until now a test could only assert that a fetch
// *eventually finished* — not that failover actually happened, that seek
// priority beat the sequential prefix, or that the chunk-0 prefetch overlap
// paid off, which are exactly the claims F4 makes. Design:
// docs/plans/mesh-testing.md §Phase T1.

// wholePiece is the synthetic piece index of the F3 whole-file path, which
// fetches one indivisible piece. It lets both fetch modes share the failover
// accounting: a holder that failed the whole file followed by another that
// delivered it is one failover, exactly as for a chunk.
const wholePiece = -1

// transferStats accumulates one transfer's diagnostics. Its lock is its own —
// it must never sit in the path that publishes progress to readers — and every
// method is nil-safe, so a chunkPlan built without a transfer (unit tests) needs
// no sink.
type transferStats struct {
	mu      sync.Mutex
	started time.Time
	ended   time.Time

	mode      string
	firstByte time.Duration

	chunks     int
	chunksDone int
	retries    int
	failovers  int
	stalls     int
	corrupt    int

	// failedBy records which holders have already failed each piece, so a later
	// success by a *different* holder is recognisable as a failover. Cleared per
	// attempt (a fallback restarts the accounting).
	failedBy map[int]map[string]bool

	// prior archives attempts abandoned mid-transfer (see resetAttempt).
	prior []AttemptStats

	order []string                  // provider keys in tracker order (the fetch order)
	prov  map[string]*ProviderStats // by provider key
}

func newTransferStats() *transferStats {
	return &transferStats{
		started:  time.Now(),
		failedBy: map[int]map[string]bool{},
		prov:     map[string]*ProviderStats{},
	}
}

// setMode records which fetch path is running ("local", "swarm", "whole").
func (s *transferStats) setMode(mode string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.mode = mode
	s.mu.Unlock()
}

// setChunks records the manifest's chunk count once the layout is known.
func (s *transferStats) setChunks(n int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.chunks = n
	s.mu.Unlock()
}

// noteFirstByte timestamps the moment the front of the file became readable —
// the streaming time-to-first-byte. First call in an attempt wins.
func (s *transferStats) noteFirstByte() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.firstByte == 0 {
		if d := time.Since(s.started); d > 0 {
			s.firstByte = d
		} else {
			s.firstByte = time.Nanosecond // never report "not yet" for a byte that landed
		}
	}
	s.mu.Unlock()
}

// noteStall counts one idle-read watchdog firing (a holder that accepted the
// connection then went silent).
func (s *transferStats) noteStall() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.stalls++
	s.mu.Unlock()
}

// resetAttempt clears the per-attempt readable state when a failed path
// restarts from zero (the swarm→whole-file fallback, or the next holder in it):
// readers lost the bytes, so the time-to-first-byte must describe the live
// attempt. The cumulative counters — retries, failovers, stalls, corrupt,
// per-provider bytes — are history and stay, and so does failedBy: a piece
// delivered by a holder after another one failed it is a failover no matter
// which attempt each happened in.
//
// What it clears is archived into prior first, so a transfer that failed after a
// fallback can still report what the abandoned phase reached. Clearing without
// archiving is what made a failed swarm fetch read as mode=whole chunks=0/0.
func (s *transferStats) resetAttempt() {
	if s == nil {
		return
	}
	s.mu.Lock()
	// Only an attempt that got somewhere is worth archiving. The mode alone does
	// not count: runWhole sets it once and then walks its holders, resetting
	// between each, so keying off mode would pad the readout with blank entries
	// for holders that never answered. A swarm phase that read a manifest has a
	// chunk count even if it fetched nothing, which is exactly the case that
	// needs recording.
	if s.chunks > 0 || s.chunksDone > 0 || s.firstByte > 0 {
		s.prior = append(s.prior, AttemptStats{
			Mode:       s.mode,
			FirstByte:  s.firstByte,
			Chunks:     s.chunks,
			ChunksDone: s.chunksDone,
		})
	}
	s.firstByte = 0
	s.chunks, s.chunksDone = 0, 0
	s.mu.Unlock()
}

// finish freezes Elapsed at the terminal moment.
func (s *transferStats) finish() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended.IsZero() {
		s.ended = time.Now()
	}
	s.mu.Unlock()
}

// providerKey identifies a holder across a transfer. The public key is the
// identity; the label is a fallback for the synthetic peers unit tests build.
func providerKey(p *Peer) string {
	if p == nil {
		return ""
	}
	if p.PublicKey != "" {
		return p.PublicKey
	}
	return p.Label()
}

// providerLocked returns (creating on first sight) the accounting row for p.
// Caller holds s.mu; nil for an unidentifiable holder.
func (s *transferStats) providerLocked(p *Peer) *ProviderStats {
	key := providerKey(p)
	if key == "" {
		return nil
	}
	ps, ok := s.prov[key]
	if !ok {
		ps = &ProviderStats{Name: p.Display(), PublicKey: p.PublicKey}
		s.prov[key] = ps
		s.order = append(s.order, key)
	}
	return ps
}

// noteBytes credits a holder with bytes as they stream in — the whole-file path
// has no piece boundary to account at, so it reports continuously.
func (s *transferStats) noteBytes(p *Peer, bytes int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if ps := s.providerLocked(p); ps != nil {
		ps.Bytes += bytes
	}
	s.mu.Unlock()
}

// noteSucceed credits a holder with a delivered piece. A piece that some other
// holder already failed counts as a failover — the swarm's headline claim.
func (s *transferStats) noteSucceed(piece int, p *Peer, bytes int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := providerKey(p)
	if ps := s.providerLocked(p); ps != nil {
		ps.Bytes += bytes
		if piece != wholePiece {
			ps.Chunks++
		}
	}
	if piece != wholePiece {
		s.chunksDone++
	}
	for failer := range s.failedBy[piece] {
		if failer != key {
			s.failovers++
			break
		}
	}
	delete(s.failedBy, piece)
}

// noteFail records a failed attempt at a piece: the holder's error, the retry,
// and — for later failover detection — that this holder failed this piece.
func (s *transferStats) noteFail(piece int, p *Peer, err error, corrupt bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retries++
	if corrupt {
		s.corrupt++
	}
	key := providerKey(p)
	if key == "" {
		return // no holder to blame (the tracker ran dry)
	}
	if ps := s.providerLocked(p); ps != nil {
		ps.Failures++
		if err != nil {
			ps.LastError = err.Error()
		}
	}
	if s.failedBy[piece] == nil {
		s.failedBy[piece] = map[string]bool{}
	}
	s.failedBy[piece][key] = true
}

// noteDropped marks a holder taken out of rotation (corrupt bytes, or too many
// consecutive failures).
func (s *transferStats) noteDropped(p *Peer) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if ps := s.providerLocked(p); ps != nil {
		ps.Dropped = true
	}
	s.mu.Unlock()
}

// snapshot renders the current counters as the public [TransferStats]. The
// caller supplies the fields owned by the transfer itself (hash/size/progress),
// so this never has to reach across locks.
func (s *transferStats) snapshot(hash string, size, progress int64) TransferStats {
	out := TransferStats{Hash: hash, Size: size, Progress: progress}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out.Mode = s.mode
	out.FirstByte = s.firstByte
	if s.ended.IsZero() {
		out.Elapsed = time.Since(s.started)
	} else {
		out.Elapsed = s.ended.Sub(s.started)
	}
	out.Chunks, out.ChunksDone = s.chunks, s.chunksDone
	out.Retries, out.Failovers = s.retries, s.failovers
	out.Stalls, out.Corrupt = s.stalls, s.corrupt
	out.Providers = make([]ProviderStats, 0, len(s.order))
	for _, key := range s.order {
		out.Providers = append(out.Providers, *s.prov[key])
	}
	if len(s.prior) > 0 {
		out.Prior = append([]AttemptStats(nil), s.prior...)
	}
	return out
}
