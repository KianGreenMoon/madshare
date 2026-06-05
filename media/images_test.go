package media

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	"github.com/disintegration/imaging"
)

// makeImage builds a solid-colour test image of the given size and encodes it in
// the requested format, returning the encoded bytes.
func makeImage(t *testing.T, w, h int, format imaging.Format) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := imaging.Encode(&buf, img, format); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

func assertAllVariants(t *testing.T, set ImageSet) {
	t.Helper()
	if len(set) != len(AllVariants) {
		t.Fatalf("variant count = %d, want %d", len(set), len(AllVariants))
	}
	for _, name := range AllVariants {
		data, ok := set[name]
		if !ok {
			t.Errorf("variant %q missing", name)
			continue
		}
		if len(data) == 0 {
			t.Errorf("variant %q is empty", name)
			continue
		}
		if name == VariantOriginal {
			continue // original is the raw input bytes, validated by the caller
		}
		if _, err := imaging.Decode(bytes.NewReader(data)); err != nil {
			t.Errorf("variant %q does not decode: %v", name, err)
		}
	}
}

func TestProcessImage_JPEG(t *testing.T) {
	data := makeImage(t, 400, 300, imaging.JPEG)
	set, ext, err := ProcessImage(data, "image/jpeg")
	if err != nil {
		t.Fatalf("ProcessImage: %v", err)
	}
	if ext != ".jpg" {
		t.Errorf("ext = %q, want .jpg", ext)
	}
	if !bytes.Equal(set[VariantOriginal], data) {
		t.Error("original variant should be the raw input bytes unchanged")
	}
	assertAllVariants(t, set)
}

func TestProcessImage_PNG(t *testing.T) {
	data := makeImage(t, 256, 256, imaging.PNG)
	set, ext, err := ProcessImage(data, "image/png")
	if err != nil {
		t.Fatalf("ProcessImage: %v", err)
	}
	if ext != ".png" {
		t.Errorf("ext = %q, want .png", ext)
	}
	assertAllVariants(t, set)
	// Variants must be PNG-encoded: check the magic bytes.
	pngMagic := []byte("\x89PNG\r\n\x1a\n")
	if got := set[VariantSmallCrop]; !bytes.HasPrefix(got, pngMagic) {
		t.Errorf("small_crop is not PNG-encoded (prefix %x)", got[:min(8, len(got))])
	}
}

func TestProcessImage_OversizeRejected(t *testing.T) {
	data := makeImage(t, maxImageDimension+1, 100, imaging.JPEG)
	if _, _, err := ProcessImage(data, "image/jpeg"); err == nil {
		t.Fatal("expected error for oversize image, got nil")
	}
}

func TestProcessImage_RejectsUnsupportedMIME(t *testing.T) {
	data := makeImage(t, 100, 100, imaging.JPEG)
	if _, _, err := ProcessImage(data, "image/webp"); err == nil {
		t.Fatal("expected error for unsupported MIME, got nil")
	}
}

func TestVariantPath(t *testing.T) {
	if got := VariantPath("abc", "small_crop", ".jpg"); got != "abc/small_crop.jpg" {
		t.Errorf("VariantPath = %q, want abc/small_crop.jpg", got)
	}
	if got := VariantURL("abc", "small_crop", ".jpg"); got != "/images/abc/small_crop.jpg" {
		t.Errorf("VariantURL = %q, want /images/abc/small_crop.jpg", got)
	}
}
