package netfault

// The wire format for a fault, shared by every control surface: netfaultd's
// JSON API and meshlab's. One codec rather than one per tool, so a fault typed
// at a meshlab prompt and a fault curled at netfaultd mean the same thing.
//
// Two properties it has to have, both learned from the fault model itself:
//
//   - Durations are strings ("200ms"), not numbers. A bare JSON number would
//     have to mean nanoseconds, which nobody types correctly under pressure.
//   - The two transports get two types, mirroring Fault and DatagramFault.
//     Decoding with DisallowUnknownFields then turns "loss on a tcp link" into
//     an error instead of a knob that silently does nothing — which is the
//     failure mode that makes a chaos session draw conclusions from a link that
//     was never degraded.

import (
	"fmt"
	"time"
)

// DirJSON is one direction of a stream fault.
type DirJSON struct {
	Latency    string `json:"latency,omitempty"`
	Jitter     string `json:"jitter,omitempty"`
	Bandwidth  int64  `json:"bandwidth,omitempty"` // bytes/sec
	Slice      int    `json:"slice,omitempty"`
	SliceDelay string `json:"slice_delay,omitempty"`
}

// FaultJSON is the wire form of Fault.
type FaultJSON struct {
	Up             DirJSON `json:"up"`
	Down           DirJSON `json:"down"`
	Partition      bool    `json:"partition,omitempty"`
	KillAfterBytes int64   `json:"kill_after_bytes,omitempty"`
	KillAfterTime  string  `json:"kill_after_time,omitempty"`
}

// Fault converts the wire form, reporting the first malformed field by name.
func (f FaultJSON) Fault() (Fault, error) {
	up, err := f.Up.dir("up")
	if err != nil {
		return Fault{}, err
	}
	down, err := f.Down.dir("down")
	if err != nil {
		return Fault{}, err
	}
	kill, err := ParseDuration(f.KillAfterTime, "kill_after_time")
	if err != nil {
		return Fault{}, err
	}
	return Fault{
		Up: up, Down: down, Partition: f.Partition,
		KillAfterBytes: f.KillAfterBytes, KillAfterTime: kill,
	}, nil
}

func (d DirJSON) dir(which string) (Dir, error) {
	lat, err := ParseDuration(d.Latency, which+".latency")
	if err != nil {
		return Dir{}, err
	}
	jit, err := ParseDuration(d.Jitter, which+".jitter")
	if err != nil {
		return Dir{}, err
	}
	sd, err := ParseDuration(d.SliceDelay, which+".slice_delay")
	if err != nil {
		return Dir{}, err
	}
	if d.Bandwidth < 0 {
		return Dir{}, fmt.Errorf("%s.bandwidth = %d, want a non-negative byte rate (0 = unlimited)", which, d.Bandwidth)
	}
	if d.Slice < 0 {
		return Dir{}, fmt.Errorf("%s.slice = %d, want a non-negative size (0 = whole writes)", which, d.Slice)
	}
	return Dir{Latency: lat, Jitter: jit, Bandwidth: d.Bandwidth, Slice: d.Slice, SliceDelay: sd}, nil
}

// ToJSON renders a Fault in the wire form.
func (f Fault) ToJSON() FaultJSON {
	conv := func(d Dir) DirJSON {
		return DirJSON{
			Latency: DurationString(d.Latency), Jitter: DurationString(d.Jitter),
			Bandwidth: d.Bandwidth, Slice: d.Slice, SliceDelay: DurationString(d.SliceDelay),
		}
	}
	return FaultJSON{
		Up: conv(f.Up), Down: conv(f.Down), Partition: f.Partition,
		KillAfterBytes: f.KillAfterBytes, KillAfterTime: DurationString(f.KillAfterTime),
	}
}

// DatagramDirJSON is one direction of a datagram fault.
type DatagramDirJSON struct {
	Latency      string  `json:"latency,omitempty"`
	Jitter       string  `json:"jitter,omitempty"`
	Bandwidth    int64   `json:"bandwidth,omitempty"`
	Loss         float64 `json:"loss,omitempty"`
	Duplicate    float64 `json:"duplicate,omitempty"`
	Reorder      float64 `json:"reorder,omitempty"`
	ReorderDelay string  `json:"reorder_delay,omitempty"`
}

// DatagramFaultJSON is the wire form of DatagramFault.
type DatagramFaultJSON struct {
	Up        DatagramDirJSON `json:"up"`
	Down      DatagramDirJSON `json:"down"`
	Partition bool            `json:"partition,omitempty"`
}

// Fault converts the wire form, reporting the first malformed field by name.
func (f DatagramFaultJSON) Fault() (DatagramFault, error) {
	up, err := f.Up.dir("up")
	if err != nil {
		return DatagramFault{}, err
	}
	down, err := f.Down.dir("down")
	if err != nil {
		return DatagramFault{}, err
	}
	return DatagramFault{Up: up, Down: down, Partition: f.Partition}, nil
}

func (d DatagramDirJSON) dir(which string) (DatagramDir, error) {
	lat, err := ParseDuration(d.Latency, which+".latency")
	if err != nil {
		return DatagramDir{}, err
	}
	jit, err := ParseDuration(d.Jitter, which+".jitter")
	if err != nil {
		return DatagramDir{}, err
	}
	rd, err := ParseDuration(d.ReorderDelay, which+".reorder_delay")
	if err != nil {
		return DatagramDir{}, err
	}
	for _, p := range []struct {
		name string
		v    float64
	}{{"loss", d.Loss}, {"duplicate", d.Duplicate}, {"reorder", d.Reorder}} {
		if p.v < 0 || p.v > 1 {
			return DatagramDir{}, fmt.Errorf("%s.%s = %v, want a probability in [0,1]", which, p.name, p.v)
		}
	}
	if d.Bandwidth < 0 {
		return DatagramDir{}, fmt.Errorf("%s.bandwidth = %d, want a non-negative byte rate (0 = unlimited)", which, d.Bandwidth)
	}
	if d.Reorder > 0 && rd == 0 {
		return DatagramDir{}, fmt.Errorf("%s.reorder = %v with no reorder_delay — a packet held back "+
			"by zero is not held back at all", which, d.Reorder)
	}
	return DatagramDir{
		Latency: lat, Jitter: jit, Bandwidth: d.Bandwidth,
		Loss: d.Loss, Duplicate: d.Duplicate, Reorder: d.Reorder, ReorderDelay: rd,
	}, nil
}

// ToJSON renders a DatagramFault in the wire form.
func (f DatagramFault) ToJSON() DatagramFaultJSON {
	conv := func(d DatagramDir) DatagramDirJSON {
		return DatagramDirJSON{
			Latency: DurationString(d.Latency), Jitter: DurationString(d.Jitter), Bandwidth: d.Bandwidth,
			Loss: d.Loss, Duplicate: d.Duplicate, Reorder: d.Reorder,
			ReorderDelay: DurationString(d.ReorderDelay),
		}
	}
	return DatagramFaultJSON{Up: conv(f.Up), Down: conv(f.Down), Partition: f.Partition}
}

// ParseDuration parses an optional duration field, naming it on failure. Empty
// is zero, and a negative duration is refused — every knob here is a delay, and
// a negative delay is a typo rather than an intent.
func ParseDuration(s, field string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("%s: %v is negative", field, d)
	}
	return d, nil
}

// DurationString is the inverse, rendering zero as empty so omitempty drops it.
func DurationString(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}
