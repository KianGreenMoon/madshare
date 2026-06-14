package media

import (
	"reflect"
	"testing"
)

func TestParseFFprobe_Lossless(t *testing.T) {
	// FLAC: bit_rate / duration on the format block, bits_per_raw_sample on the
	// stream — the common lossless shape.
	in := []byte(`{
	  "streams": [
	    {"codec_type":"video","codec_name":"mjpeg"},
	    {"codec_type":"audio","codec_name":"flac","sample_rate":"44100","channels":2,"bits_per_raw_sample":"16"}
	  ],
	  "format": {"duration":"212.480000","bit_rate":"889000"}
	}`)
	ti, err := parseFFprobe(in)
	if err != nil {
		t.Fatalf("parseFFprobe: %v", err)
	}
	want := &TechInfo{
		DurationSeconds: 212.48,
		Bitrate:         889000,
		SampleRate:      44100,
		Channels:        2,
		BitDepth:        16,
		Codec:           "flac",
	}
	if !reflect.DeepEqual(ti, want) {
		t.Errorf("parseFFprobe = %+v, want %+v", ti, want)
	}
}

func TestParseFFprobe_LossyVBR(t *testing.T) {
	// VBR MP3: no bits_per_raw_sample, bit_rate on the stream; bit_depth stays 0.
	in := []byte(`{
	  "streams": [
	    {"codec_type":"audio","codec_name":"mp3","sample_rate":"44100","channels":2,"bit_rate":"320000","duration":"180.10"}
	  ],
	  "format": {"duration":"180.10","bit_rate":"321000"}
	}`)
	ti, err := parseFFprobe(in)
	if err != nil {
		t.Fatalf("parseFFprobe: %v", err)
	}
	if ti.Codec != "mp3" || ti.Bitrate != 320000 || ti.BitDepth != 0 {
		t.Errorf("got %+v, want codec=mp3 bitrate=320000 bit_depth=0", ti)
	}
}

func TestParseFFprobe_NoAudioStream(t *testing.T) {
	in := []byte(`{"streams":[{"codec_type":"video","codec_name":"png"}],"format":{}}`)
	ti, err := parseFFprobe(in)
	if err != nil {
		t.Fatalf("parseFFprobe: %v", err)
	}
	if *ti != (TechInfo{}) {
		t.Errorf("no audio stream should yield zero TechInfo, got %+v", ti)
	}
}

func TestParseFFprobe_NAFields(t *testing.T) {
	// ffprobe emits "N/A" for fields it cannot determine; those must parse to 0,
	// not error.
	in := []byte(`{"streams":[{"codec_type":"audio","codec_name":"opus","sample_rate":"48000","channels":2,"bit_rate":"N/A"}],"format":{"duration":"N/A","bit_rate":"96000"}}`)
	ti, err := parseFFprobe(in)
	if err != nil {
		t.Fatalf("parseFFprobe: %v", err)
	}
	if ti.SampleRate != 48000 || ti.Bitrate != 96000 || ti.DurationSeconds != 0 {
		t.Errorf("got %+v, want sample_rate=48000 bitrate=96000 (format fallback) duration=0", ti)
	}
}

func TestParseFpcalc(t *testing.T) {
	in := []byte(`{"duration":212.48,"fingerprint":[1,2,4294967295,3]}`)
	fp, err := parseFpcalc(in, "1.5.1")
	if err != nil {
		t.Fatalf("parseFpcalc: %v", err)
	}
	if fp.Algo != "chromaprint" || fp.AlgoVersion != "1.5.1" {
		t.Errorf("algo=%q version=%q, want chromaprint/1.5.1", fp.Algo, fp.AlgoVersion)
	}
	if fp.Duration != 212.48 {
		t.Errorf("duration = %v, want 212.48", fp.Duration)
	}
	want := []uint32{1, 2, 4294967295, 3} // verifies the >int32 value survives
	if !reflect.DeepEqual(fp.Raw, want) {
		t.Errorf("raw = %v, want %v", fp.Raw, want)
	}
}

func TestParseFpcalc_Empty(t *testing.T) {
	if _, err := parseFpcalc([]byte(`{"duration":1,"fingerprint":[]}`), ""); err == nil {
		t.Error("empty fingerprint should error")
	}
}

func TestFingerprintPackRoundTrip(t *testing.T) {
	fp := &Fingerprint{Raw: []uint32{0, 1, 4294967295, 123456789}}
	got := DecodeFingerprint(fp.Packed())
	if !reflect.DeepEqual(got, fp.Raw) {
		t.Errorf("round trip = %v, want %v", got, fp.Raw)
	}
	if len(fp.Packed()) != 4*len(fp.Raw) {
		t.Errorf("packed length = %d, want %d", len(fp.Packed()), 4*len(fp.Raw))
	}
}

func TestDecodeFingerprint_TruncatesPartialTail(t *testing.T) {
	// 6 bytes = one whole uint32 plus a 2-byte tail, which is dropped.
	got := DecodeFingerprint([]byte{1, 0, 0, 0, 9, 9})
	if !reflect.DeepEqual(got, []uint32{1}) {
		t.Errorf("got %v, want [1]", got)
	}
}
