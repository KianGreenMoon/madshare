package media

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"strings"

	"github.com/disintegration/imaging"
)

// Variant names. Each maps to one output file inside the base_key directory.
// Crop variants are center-cropped squares. Fit variants preserve aspect ratio
// inside a square, padded with white (JPEG) or transparent (PNG).
const (
	VariantOriginal   = "original"
	VariantThumbCrop  = "thumb_crop"  // 64×64, crop
	VariantThumbFit   = "thumb_fit"   // 64×64, fit
	VariantSmallCrop  = "small_crop"  // 150×150, crop
	VariantSmallFit   = "small_fit"   // 150×150, fit
	VariantMediumCrop = "medium_crop" // 300×300, crop
	VariantMediumFit  = "medium_fit"  // 300×300, fit
	VariantLargeCrop  = "large_crop"  // 600×600, crop
	VariantLargeFit   = "large_fit"   // 600×600, fit
)

// variantSizes maps each non-original variant name to its target pixel dimension.
var variantSizes = map[string]int{
	VariantThumbCrop:  64,
	VariantThumbFit:   64,
	VariantSmallCrop:  150,
	VariantSmallFit:   150,
	VariantMediumCrop: 300,
	VariantMediumFit:  300,
	VariantLargeCrop:  600,
	VariantLargeFit:   600,
}

// AllVariants lists every variant name in a stable order.
var AllVariants = []string{
	VariantOriginal,
	VariantThumbCrop, VariantThumbFit,
	VariantSmallCrop, VariantSmallFit,
	VariantMediumCrop, VariantMediumFit,
	VariantLargeCrop, VariantLargeFit,
}

// maxImageDimension rejects images wider or taller than this (defends against
// decompression bombs and absurd resize work).
const maxImageDimension = 8000

// ImageSet holds the encoded bytes for each variant, keyed by variant name.
// Call ProcessImage to obtain one.
type ImageSet map[string][]byte

// ProcessImage decodes data (JPEG or PNG only) and generates all image
// variants. The output format matches the input: PNG in → PNG variants,
// JPEG in → JPEG variants.
//
// The second return value is the canonical extension: ".jpg" or ".png".
//
// Returns an error if the image cannot be decoded, if sourceMIME is not
// image/jpeg or image/png, if it exceeds maxImageDimension in either dimension,
// or if any variant cannot be encoded.
func ProcessImage(data []byte, sourceMIME string) (ImageSet, string, error) {
	var (
		format imaging.Format
		ext    string
		bg     color.Color
		encOpt []imaging.EncodeOption
	)
	switch sourceMIME {
	case "image/jpeg":
		format, ext, bg = imaging.JPEG, ".jpg", color.NRGBA{R: 255, G: 255, B: 255, A: 255}
		encOpt = []imaging.EncodeOption{imaging.JPEGQuality(85)}
	case "image/png":
		format, ext, bg = imaging.PNG, ".png", color.NRGBA{}
	default:
		return nil, "", fmt.Errorf("unsupported image type %q", sourceMIME)
	}

	img, err := imaging.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("decode image: %w", err)
	}
	b := img.Bounds()
	if b.Dx() > maxImageDimension || b.Dy() > maxImageDimension {
		return nil, "", fmt.Errorf("image %dx%d exceeds max dimension %d", b.Dx(), b.Dy(), maxImageDimension)
	}

	set := ImageSet{VariantOriginal: data}
	for name, size := range variantSizes {
		var out *image.NRGBA
		switch {
		case strings.HasSuffix(name, "_crop"):
			out = imaging.Fill(img, size, size, imaging.Center, imaging.Lanczos)
		case strings.HasSuffix(name, "_fit"):
			fit := imaging.Fit(img, size, size, imaging.Lanczos)
			out = imaging.PasteCenter(imaging.New(size, size, bg), fit)
		default:
			return nil, "", fmt.Errorf("variant %q has no crop/fit suffix", name)
		}
		var buf bytes.Buffer
		if err := imaging.Encode(&buf, out, format, encOpt...); err != nil {
			return nil, "", fmt.Errorf("encode variant %q: %w", name, err)
		}
		set[name] = buf.Bytes()
	}
	return set, ext, nil
}
