package media

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/bits"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// analyzeTimeout bounds a single ffprobe/fpcalc invocation so a malformed file
// can never wedge an analysis worker.
const analyzeTimeout = 60 * time.Second

// TechInfo holds the codec / quality columns ffprobe extracts. A zero-valued
// field means "ffprobe did not report it" and is persisted as NULL.
type TechInfo struct {
	DurationSeconds float64
	Bitrate         int // bits per second
	SampleRate      int // Hz
	Channels        int
	BitDepth        int // bits per sample; 0 for lossy formats that don't report it
	Codec           string
}

// Fingerprint is a Chromaprint acoustic fingerprint produced by fpcalc.
type Fingerprint struct {
	Algo        string   // always "chromaprint"
	AlgoVersion string   // fpcalc/chromaprint version, best-effort ("" if unknown)
	Duration    float64  // fingerprinted duration in seconds
	Raw         []uint32 // raw sub-fingerprints, for Hamming matching (P1)
}

// Packed returns the raw fingerprint as a little-endian uint32 byte slice for
// BLOB storage. DecodeFingerprint is the inverse.
func (f *Fingerprint) Packed() []byte {
	b := make([]byte, 4*len(f.Raw))
	for i, v := range f.Raw {
		binary.LittleEndian.PutUint32(b[i*4:], v)
	}
	return b
}

// BitErrorRate returns the fraction of differing bits between two raw
// fingerprints, compared positionally over their common prefix: 0 = identical,
// 1 = every compared bit differs. Empty input (no overlap) returns 1.
//
// Positional only — v0 does no offset-alignment search, so a frame-shifted
// re-encode reads as more different than it truly is. That biases toward false
// negatives (the file lands as its own recording), which the design accepts: a
// missed grouping loses nothing, whereas a wrong merge would (see
// docs/architecture/recordings.md, Identity).
func BitErrorRate(a, b []uint32) float64 {
	n := min(len(a), len(b))
	if n == 0 {
		return 1
	}
	var diff int
	for i := 0; i < n; i++ {
		diff += bits.OnesCount32(a[i] ^ b[i])
	}
	return float64(diff) / float64(n*32)
}

// DecodeFingerprint unpacks a little-endian uint32 BLOB written by Packed back
// into the raw sub-fingerprint slice. A length that is not a multiple of 4 is
// truncated to the largest whole number of uint32s.
func DecodeFingerprint(b []byte) []uint32 {
	out := make([]uint32, len(b)/4)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(b[i*4:])
	}
	return out
}

// Tools is the ingest analysis a node can perform on an audio file: the tech
// columns and the acoustic fingerprint. A server gets both by running ffprobe
// and fpcalc, which is what ExecTools does and what everything here has always
// meant.
//
// It is an interface because "run the binary on PATH" is a fact about a SERVER,
// not about analysis. An embedder can be somewhere with no PATH to install onto
// and no way to execute what it ships — Android refuses to run anything the app
// wrote, and the app is a library inside somebody else's process, so there is
// not even a self to re-exec. Everything downstream is dead there: no tech
// columns, no fingerprints, and therefore no federation, since a node that
// cannot fingerprint must not redistribute audio. Such an embedder brings its
// own implementation instead (docs/architecture/embedding.md §"Analysis an
// embedder brings itself").
//
// The gate does not move: fingerprinting is still REQUIRED of a federated node.
// This only widens what may satisfy it.
type Tools interface {
	// Available reports which of the two this installation can perform. Read
	// once at startup, to warn about each absence exactly once, so it must be
	// cheap and must not depend on a particular file.
	Available() (probe, fingerprint bool)

	// ProbeTech fills the codec / quality columns for one file.
	ProbeTech(ctx context.Context, path string) (*TechInfo, error)

	// ComputeFingerprint computes one file's Chromaprint fingerprint. Whatever
	// produces it must agree with fpcalc to within database's match threshold,
	// because these are compared ACROSS nodes: a phone's fingerprint of a track
	// meets a server's fingerprint of the same track.
	ComputeFingerprint(ctx context.Context, path string) (*Fingerprint, error)
}

// ExecTools is the default Tools and the only one a server uses: ffprobe and
// fpcalc, found on PATH and run as child processes.
type ExecTools struct{}

// Available reports which of the two binaries are on PATH.
func (ExecTools) Available() (probe, fingerprint bool) { return ToolStatus() }

// ProbeTech runs ffprobe. See the package function of the same name.
func (ExecTools) ProbeTech(ctx context.Context, path string) (*TechInfo, error) {
	return ProbeTech(ctx, path)
}

// ComputeFingerprint runs fpcalc. See the package function of the same name.
func (ExecTools) ComputeFingerprint(ctx context.Context, path string) (*Fingerprint, error) {
	return ComputeFingerprint(ctx, path)
}

// ToolStatus reports which optional analysis binaries are on PATH. Called once
// at startup so the operator gets a single warning per missing tool; absence is
// never fatal (see docs/architecture/recordings.md, Graceful degradation).
func ToolStatus() (ffprobe, fpcalc bool) {
	_, ffErr := exec.LookPath("ffprobe")
	_, fpErr := exec.LookPath("fpcalc")
	return ffErr == nil, fpErr == nil
}

// ffprobeStreams / ffprobeFormat mirror the subset of ffprobe -show_streams /
// -show_format JSON we read.
type ffprobeOutput struct {
	Streams []struct {
		CodecType        string `json:"codec_type"`
		CodecName        string `json:"codec_name"`
		SampleRate       string `json:"sample_rate"`
		Channels         int    `json:"channels"`
		BitsPerSample    int    `json:"bits_per_sample"`
		BitsPerRawSample string `json:"bits_per_raw_sample"`
		BitRate          string `json:"bit_rate"`
		Duration         string `json:"duration"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
		BitRate  string `json:"bit_rate"`
	} `json:"format"`
}

// ProbeTech runs ffprobe on the file at path and returns its tech info. The
// caller must have confirmed ffprobe is present (see ToolStatus); a missing
// binary surfaces as an error here. Fields ffprobe omits stay zero.
func ProbeTech(ctx context.Context, path string) (*TechInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, analyzeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe %s: %w", path, err)
	}
	return parseFFprobe(out)
}

// parseFFprobe turns ffprobe -show_streams/-show_format JSON into a TechInfo.
// Split out from ProbeTech so it is testable without the binary.
func parseFFprobe(out []byte) (*TechInfo, error) {
	var parsed ffprobeOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("ffprobe parse: %w", err)
	}

	ti := &TechInfo{}
	for _, s := range parsed.Streams {
		if s.CodecType != "audio" {
			continue
		}
		ti.Codec = s.CodecName
		ti.Channels = s.Channels
		ti.SampleRate = atoiSafe(s.SampleRate)
		// bits_per_raw_sample is the lossless source depth; bits_per_sample is the
		// decoded depth. Prefer the raw value (FLAC reports it), fall back.
		if d := atoiSafe(s.BitsPerRawSample); d > 0 {
			ti.BitDepth = d
		} else {
			ti.BitDepth = s.BitsPerSample
		}
		ti.Bitrate = atoiSafe(s.BitRate)
		ti.DurationSeconds = atofSafe(s.Duration)
		break // first audio stream only
	}
	// Duration and bitrate frequently live on the container, not the stream
	// (e.g. VBR MP3, FLAC). Fall back to the format block when the stream lacked
	// them.
	if ti.DurationSeconds == 0 {
		ti.DurationSeconds = atofSafe(parsed.Format.Duration)
	}
	if ti.Bitrate == 0 {
		ti.Bitrate = atoiSafe(parsed.Format.BitRate)
	}
	return ti, nil
}

// fpcalcOutput mirrors `fpcalc -json -raw` output: a raw integer fingerprint
// plus the fingerprinted duration. Raw ints can exceed int32, so they decode
// into int64 before narrowing to uint32.
type fpcalcOutput struct {
	Duration    float64 `json:"duration"`
	Fingerprint []int64 `json:"fingerprint"`
}

// ComputeFingerprint runs fpcalc on the file at path and returns its acoustic
// fingerprint. The caller must have confirmed fpcalc is present (see
// ToolStatus); a missing binary surfaces as an error here.
func ComputeFingerprint(ctx context.Context, path string) (*Fingerprint, error) {
	ctx, cancel := context.WithTimeout(ctx, analyzeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "fpcalc", "-json", "-raw", path)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("fpcalc %s: %w", path, err)
	}
	return parseFpcalc(out, fpcalcVersion())
}

// parseFpcalc turns `fpcalc -json -raw` JSON into a Fingerprint. Split out from
// ComputeFingerprint so it is testable without the binary; version is passed in
// (captured separately) to keep it pure.
func parseFpcalc(out []byte, version string) (*Fingerprint, error) {
	var parsed fpcalcOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("fpcalc parse: %w", err)
	}
	if len(parsed.Fingerprint) == 0 {
		return nil, fmt.Errorf("fpcalc: empty fingerprint")
	}
	raw := make([]uint32, len(parsed.Fingerprint))
	for i, v := range parsed.Fingerprint {
		raw[i] = uint32(v)
	}
	return &Fingerprint{
		Algo:        "chromaprint",
		AlgoVersion: version,
		Duration:    parsed.Duration,
		Raw:         raw,
	}, nil
}

// fpcalcVersion returns the fpcalc version string, captured once and cached.
// Best-effort: on any error it returns "" (algo_version is nullable).
var (
	fpcalcVersionOnce sync.Once
	fpcalcVersionStr  string
)

func fpcalcVersion() string {
	fpcalcVersionOnce.Do(func() {
		out, err := exec.Command("fpcalc", "-version").Output()
		if err != nil {
			return
		}
		// Output looks like "fpcalc version 1.5.1"; keep the trailing token.
		fields := strings.Fields(strings.TrimSpace(string(out)))
		if len(fields) > 0 {
			fpcalcVersionStr = fields[len(fields)-1]
		}
	})
	return fpcalcVersionStr
}

// atoiSafe parses an int, returning 0 for empty or unparseable input (ffprobe
// emits "N/A" / "" for fields it cannot determine).
func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// atofSafe parses a float, returning 0 for empty or unparseable input.
func atofSafe(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}
