package tools

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// rgbFrom builds a widgetRGB from a flat list of grey/RGB triples.
func rgbFrom(w, h int, pix []uint8) *widgetRGB {
	img := newWidgetRGB(w, h)
	copy(img.Pix, pix)
	return img
}

func greyRamp(values ...uint8) []uint8 {
	out := make([]uint8, 0, len(values)*3)
	for _, v := range values {
		out = append(out, v, v, v)
	}
	return out
}

// TestResampleMatchesPillow pins the resampler against real Pillow output.
//
// Every expectation was produced by running PIL 12.0.0:
//
//	Image.new("RGB", size); im.putdata(px)
//	im.resize(target, Image.Resampling.BILINEAR).getdata()
//
// The cases cover the shapes that matter: an exact 2:1 reduction (what a
// 1080-wide phone actually hits), a non-integer reduction, a reduction below
// 2:1 where the filter is NOT widened, an upscale, and a two-axis reduction that
// exercises the horizontal-then-vertical pass order.
//
// If this test fails, the preview is no longer the image Pillow produced — most
// likely because the support scaling was dropped, which silently turns an
// antialiased downscale into an aliased point sample.
func TestResampleMatchesPillow(t *testing.T) {
	cases := []struct {
		name         string
		w, h         int
		pix          []uint8
		dstW, dstH   int
		want         []uint8
		wantW, wantH int
	}{
		{
			name: "ramp 4x1 to 2x1", w: 4, h: 1,
			pix:  greyRamp(0, 85, 170, 255),
			dstW: 2, dstH: 1, wantW: 2, wantH: 1,
			want: greyRamp(61, 194),
		},
		{
			name: "ramp 8x1 to 3x1", w: 8, h: 1,
			pix:  greyRamp(0, 32, 64, 96, 128, 160, 192, 224),
			dstW: 3, dstH: 1, wantW: 3, wantH: 1,
			want: greyRamp(35, 112, 189),
		},
		{
			name: "ramp 5x1 to 2x1", w: 5, h: 1,
			pix:  greyRamp(0, 64, 128, 192, 255),
			dstW: 2, dstH: 1, wantW: 2, wantH: 1,
			want: greyRamp(64, 192),
		},
		{
			// Reduction factor 1.5: filterScale is still > 1 so the kernel widens.
			name: "ramp 3x1 to 2x1", w: 3, h: 1,
			pix:  greyRamp(0, 128, 255),
			dstW: 2, dstH: 1, wantW: 2, wantH: 1,
			want: greyRamp(48, 207),
		},
		{
			// Upscale: filterScale is clamped to 1, so the kernel stays narrow.
			name: "upscale 2x1 to 3x1", w: 2, h: 1,
			pix:  greyRamp(0, 255),
			dstW: 3, dstH: 1, wantW: 3, wantH: 1,
			want: greyRamp(0, 128, 255),
		},
		{
			name: "checkerboard 4x4 to 2x2", w: 4, h: 4,
			pix: greyRamp(
				255, 0, 255, 0,
				0, 255, 0, 255,
				255, 0, 255, 0,
				0, 255, 0, 255,
			),
			dstW: 2, dstH: 2, wantW: 2, wantH: 2,
			want: greyRamp(130, 125, 125, 130),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := widgetResizeRGB(rgbFrom(tc.w, tc.h, tc.pix), tc.dstW, tc.dstH)
			if got.W != tc.wantW || got.H != tc.wantH {
				t.Fatalf("size = %dx%d, want %dx%d", got.W, got.H, tc.wantW, tc.wantH)
			}
			if !bytes.Equal(got.Pix, tc.want) {
				t.Errorf("pixels = %v\nwant     %v", got.Pix, tc.want)
			}
		})
	}
}

// TestResampleMatchesPillowTwoAxisColour is the same pin for a colour image
// reduced on both axes at once, which is the only case that can catch a
// horizontal/vertical pass swap.
func TestResampleMatchesPillowTwoAxisColour(t *testing.T) {
	const w, h = 9, 16
	src := newWidgetRGB(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 3
			src.Pix[i] = uint8((x * 28) % 256)
			src.Pix[i+1] = uint8((y * 16) % 256)
			src.Pix[i+2] = uint8(((x + y) * 13) % 256)
		}
	}
	// PIL 12.0.0: im.resize((4, 7), BILINEAR).getdata()
	want := []uint8{
		23, 13, 22, 80, 13, 48, 144, 13, 78, 201, 13, 104,
		23, 47, 49, 80, 47, 75, 144, 47, 105, 201, 47, 131,
		23, 84, 79, 80, 84, 105, 144, 84, 135, 201, 84, 161,
		23, 120, 109, 80, 120, 135, 144, 120, 165, 201, 120, 191,
		23, 156, 138, 80, 156, 164, 144, 156, 194, 201, 156, 218,
		23, 193, 168, 80, 193, 194, 144, 193, 213, 201, 193, 142,
		23, 227, 195, 80, 227, 219, 144, 227, 139, 201, 227, 32,
	}
	got := widgetResizeRGB(src, 4, 7)
	if got.W != 4 || got.H != 7 {
		t.Fatalf("size = %dx%d, want 4x7", got.W, got.H)
	}
	if !bytes.Equal(got.Pix, want) {
		t.Errorf("pixels = %v\nwant     %v", got.Pix, want)
	}
}

func TestPreviewSizeOnlyDownscales(t *testing.T) {
	cases := []struct {
		name           string
		w, h, maxWidth int
		wantW, wantH   int
	}{
		// The real device: 1080x2280 at the default max_width.
		{"oneplus 6 at 540", 1080, 2280, 540, 540, 1140},
		{"already narrow", 480, 1000, 540, 480, 1000},
		{"exactly max width", 540, 1140, 540, 540, 1140},
		{"tall clamp floor", 4000, 3, 320, 320, 0 + 1},
		// round() is banker's: 2281 * (540/1080) = 1140.5 -> 1140, not 1141.
		{"banker's rounding on the tie", 1080, 2281, 540, 540, 1140},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, h := widgetPreviewSize(tc.w, tc.h, tc.maxWidth)
			if w != tc.wantW || h != tc.wantH {
				t.Errorf("widgetPreviewSize(%d,%d,%d) = %dx%d, want %dx%d",
					tc.w, tc.h, tc.maxWidth, w, h, tc.wantW, tc.wantH)
			}
		})
	}
}

// makeTestPNG builds an NRGBA PNG the way `adb exec-out screencap -p` does.
func makeTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{
				R: uint8((x * 255) / max(w-1, 1)),
				G: uint8((y * 255) / max(h-1, 1)),
				B: 0x40,
				A: 0xff,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestCompressPreviewProducesDecodableJPEG(t *testing.T) {
	source := makeTestPNG(t, 1080, 2280)

	encoded, w, h, err := widgetCompressPreview(source, 540, 60)
	if err != nil {
		t.Fatalf("widgetCompressPreview: %v", err)
	}
	if w != 540 || h != 1140 {
		t.Errorf("frame size = %dx%d, want 540x1140", w, h)
	}
	if len(encoded) < 1000 {
		t.Errorf("JPEG is suspiciously small: %d bytes", len(encoded))
	}
	decoded, err := jpeg.Decode(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("the encoded preview is not decodable JPEG: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != 540 || b.Dy() != 1140 {
		t.Errorf("decoded JPEG is %dx%d, want 540x1140", b.Dx(), b.Dy())
	}
}

func TestCompressPreviewSkipsUpscale(t *testing.T) {
	source := makeTestPNG(t, 320, 700)
	_, w, h, err := widgetCompressPreview(source, 540, 60)
	if err != nil {
		t.Fatalf("widgetCompressPreview: %v", err)
	}
	if w != 320 || h != 700 {
		t.Errorf("frame size = %dx%d, want the source size 320x700 (never upscale)", w, h)
	}
}

func TestCompressPreviewRejectsGarbage(t *testing.T) {
	if _, _, _, err := widgetCompressPreview([]byte("not a png"), 540, 60); err == nil {
		t.Fatal("want an error for a non-PNG payload")
	}
}

// TestQualityAffectsSize guards the plumbing of the quality argument: a low
// quality must produce a materially smaller file than a high one.
func TestQualityAffectsSize(t *testing.T) {
	source := makeTestPNG(t, 540, 960)
	low, _, _, err := widgetCompressPreview(source, 540, 45)
	if err != nil {
		t.Fatalf("quality 45: %v", err)
	}
	high, _, _, err := widgetCompressPreview(source, 540, 90)
	if err != nil {
		t.Fatalf("quality 90: %v", err)
	}
	if len(low) >= len(high) {
		t.Errorf("quality 45 produced %d bytes, quality 90 produced %d; the quality "+
			"argument is not reaching the encoder", len(low), len(high))
	}
}

// TestDecodePNGDropsAlphaWithoutCompositing mirrors Pillow's convert("RGB"),
// which takes the stored channels and discards alpha rather than blending
// against a background.
func TestDecodePNGDropsAlphaWithoutCompositing(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	img.SetNRGBA(0, 0, color.NRGBA{R: 10, G: 20, B: 30, A: 0})
	img.SetNRGBA(1, 0, color.NRGBA{R: 200, G: 100, B: 50, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}

	got, err := widgetDecodePNG(buf.Bytes())
	if err != nil {
		t.Fatalf("widgetDecodePNG: %v", err)
	}
	want := []uint8{10, 20, 30, 200, 100, 50}
	if !bytes.Equal(got.Pix, want) {
		t.Errorf("pixels = %v, want %v (alpha must be dropped, not composited)", got.Pix, want)
	}
}

// TestClip8RoundsAndClamps checks the fixed-point accumulator conversion.
// Inputs are pixel values scaled by 2^22 plus the half-LSB Pillow pre-loads.
func TestClip8RoundsAndClamps(t *testing.T) {
	const one = int64(1) << widgetPrecisionBits
	const half = one / 2
	cases := []struct {
		name string
		in   int64
		want uint8
	}{
		{"negative clamps to 0", -10 * one, 0},
		{"zero", half, 0},
		{"0.4 rounds down", half + 4*one/10, 0},
		{"0.5 rounds up", half + half, 1},
		{"127.5 rounds up", half + 127*one + half, 128},
		{"254.4 rounds down", half + 254*one + 4*one/10, 254},
		{"254.5 rounds up", half + 254*one + half, 255},
		{"255 stays", half + 255*one, 255},
		{"overshoot clamps", 1000 * one, 255},
	}
	for _, tc := range cases {
		if got := widgetClip8(tc.in); got != tc.want {
			t.Errorf("%s: widgetClip8(%d) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
}
