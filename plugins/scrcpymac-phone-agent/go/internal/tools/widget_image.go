package tools

// The scrcpymac_ui_snapshot image pipeline, replacing Pillow with the standard
// library: image/png decode, a triangle-filter resample, image/jpeg encode.
//
// The Python is three lines (mcp_ui.py:94-102):
//
//	source = Image.open(io.BytesIO(png)).convert("RGB")
//	if source.width > max_width:
//	    source = source.resize((max_width, max(1, round(h * (max_width / w)))),
//	                           Image.Resampling.BILINEAR)
//	source.save(out, format="JPEG", quality=quality, optimize=False)
//
// Each line hides something that matters:
//
//   - convert("RGB") on an RGBA screencap DROPS alpha, it does not composite
//     against anything. Taking the stored R/G/B is the faithful translation.
//   - Pillow's BILINEAR is NOT the naive 2x2 bilinear that the name suggests.
//     Since Pillow 2.7 Image.resize scales the filter's support by the reduction
//     factor, making a downscale a proper area-weighted (antialiased) resample.
//     Sampling four neighbours instead would alias badly at the 2:1 ratio this
//     tool actually runs at (1080 -> 540) and produce a visibly noisier preview.
//     widgetResampleTriangle reproduces Pillow's algorithm — same support
//     scaling, same coefficient normalisation, same two-pass order — in float64
//     rather than Pillow's 22-bit fixed point.
//   - only downscale. A device narrower than max_width is passed through at its
//     own size; the Python never upscales and neither does this.
//
// Byte-identical output is not achievable and is not the goal: Go's JPEG encoder
// is not libjpeg-turbo, so the compressed bytes (and therefore sizeBytes and
// dataBase64) differ from Pillow's for the same input. What is contract — the
// payload keys, deviceWidth/deviceHeight, frameWidth/frameHeight, mimeType — is
// reproduced exactly.

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"

	"github.com/zjywill/scrcpyMac/phone-agent/internal/jsonresult"
)

// widgetRGB is a tightly packed 8-bit RGB image: Pillow's "RGB" mode.
type widgetRGB struct {
	W, H int
	Pix  []uint8 // len == W*H*3, row-major, R,G,B per pixel
}

func newWidgetRGB(w, h int) *widgetRGB {
	return &widgetRGB{W: w, H: h, Pix: make([]uint8, w*h*3)}
}

// ColorModel implements image.Image.
func (r *widgetRGB) ColorModel() color.Model { return color.RGBAModel }

// Bounds implements image.Image.
func (r *widgetRGB) Bounds() image.Rectangle { return image.Rect(0, 0, r.W, r.H) }

// At implements image.Image.
func (r *widgetRGB) At(x, y int) color.Color {
	if x < 0 || y < 0 || x >= r.W || y >= r.H {
		return color.RGBA{}
	}
	i := (y*r.W + x) * 3
	return color.RGBA{R: r.Pix[i], G: r.Pix[i+1], B: r.Pix[i+2], A: 0xff}
}

// widgetDecodePNG decodes a screencap PNG into packed RGB, dropping alpha.
//
// The two fast paths cover everything `adb exec-out screencap -p` produces
// (8-bit RGBA is NRGBA in Go; some devices emit 8-bit RGB, which is also NRGBA
// after decode). The generic path exists so a device with an unusual pixel
// format degrades in quality of implementation, not in behaviour.
func widgetDecodePNG(data []byte) (*widgetRGB, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("cannot decode screenshot PNG: %w", err)
	}
	return widgetToRGB(img), nil
}

func widgetToRGB(img image.Image) *widgetRGB {
	b := img.Bounds()
	out := newWidgetRGB(b.Dx(), b.Dy())
	switch src := img.(type) {
	case *image.NRGBA:
		for y := 0; y < out.H; y++ {
			si := src.PixOffset(b.Min.X, b.Min.Y+y)
			di := y * out.W * 3
			for x := 0; x < out.W; x++ {
				out.Pix[di] = src.Pix[si]
				out.Pix[di+1] = src.Pix[si+1]
				out.Pix[di+2] = src.Pix[si+2]
				si += 4
				di += 3
			}
		}
	case *image.RGBA:
		// Premultiplied in Go, but a screencap is opaque, so the stored values
		// already are the true channels. Pillow drops alpha without dividing it
		// out either.
		for y := 0; y < out.H; y++ {
			si := src.PixOffset(b.Min.X, b.Min.Y+y)
			di := y * out.W * 3
			for x := 0; x < out.W; x++ {
				out.Pix[di] = src.Pix[si]
				out.Pix[di+1] = src.Pix[si+1]
				out.Pix[di+2] = src.Pix[si+2]
				si += 4
				di += 3
			}
		}
	default:
		for y := 0; y < out.H; y++ {
			di := y * out.W * 3
			for x := 0; x < out.W; x++ {
				// At().RGBA() returns 16-bit alpha-premultiplied values; >>8
				// recovers the 8-bit channel for the opaque pixels a screenshot
				// is made of.
				cr, cg, cb, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				out.Pix[di] = uint8(cr >> 8)
				out.Pix[di+1] = uint8(cg >> 8)
				out.Pix[di+2] = uint8(cb >> 8)
				di += 3
			}
		}
	}
	return out
}

// widgetPreviewSize is the target size for a preview frame: only ever a
// downscale, and the height is round(h * (maxWidth / w)) with Python's
// banker's rounding, floored at 1.
func widgetPreviewSize(w, h, maxWidth int) (int, int) {
	if w <= 0 || h <= 0 || maxWidth <= 0 || w <= maxWidth {
		return w, h
	}
	scaled := jsonresult.PyRoundInt(float64(h) * (float64(maxWidth) / float64(w)))
	if scaled < 1 {
		scaled = 1
	}
	return maxWidth, scaled
}

// widgetResizeRGB resamples src to dstW x dstH, one axis at a time, exactly like
// Pillow's ImagingResample.
func widgetResizeRGB(src *widgetRGB, dstW, dstH int) *widgetRGB {
	if src == nil || (dstW == src.W && dstH == src.H) {
		return src
	}
	out := src
	if dstW != out.W {
		out = widgetResampleAxis(out, dstW, true)
	}
	if dstH != out.H {
		out = widgetResampleAxis(out, dstH, false)
	}
	return out
}

// widgetTriangle is Pillow's BILINEAR kernel: support 1.0, f(x) = 1-|x|.
func widgetTriangle(x float64) float64 {
	if x < 0 {
		x = -x
	}
	if x < 1.0 {
		return 1.0 - x
	}
	return 0
}

// widgetPrecisionBits is Pillow's PRECISION_BITS for 8-bit images:
// 32 - 8 - 2 = 22. Coefficients are quantised to this many fractional bits and
// the accumulator is pre-loaded with half an LSB so the final shift rounds.
//
// Working in float64 instead would be off by one in the last place on a small
// fraction of pixels — measurably so: it is what the two-axis colour test caught.
const widgetPrecisionBits = 22

// widgetCoeffs is one axis' precomputed filter: for each output index, the first
// contributing input index, how many contribute, and their normalised weights in
// widgetPrecisionBits fixed point.
type widgetCoeffs struct {
	ksize  int
	min    []int
	count  []int
	weight []int32 // ksize entries per output index
}

// widgetPrecomputeCoeffs is Pillow's precompute_coeffs.
//
// The one line that makes this an antialiased downscale rather than a naive
// bilinear is `if filterScale < 1 { filterScale = 1 }` followed by
// `support = 1.0 * filterScale`: reducing by 2x widens the kernel to 2 input
// pixels either side, so every input pixel contributes to the output instead of
// three quarters of them being skipped.
func widgetPrecomputeCoeffs(inSize, outSize int) widgetCoeffs {
	scale := float64(inSize) / float64(outSize)
	filterScale := scale
	if filterScale < 1.0 {
		filterScale = 1.0
	}
	support := filterScale // triangle support is 1.0
	ksize := int(math.Ceil(support))*2 + 1

	c := widgetCoeffs{
		ksize:  ksize,
		min:    make([]int, outSize),
		count:  make([]int, outSize),
		weight: make([]int32, outSize*ksize),
	}
	invScale := 1.0 / filterScale
	k := make([]float64, ksize)
	for xx := 0; xx < outSize; xx++ {
		center := (float64(xx) + 0.5) * scale
		xmin := int(center - support + 0.5)
		if xmin < 0 {
			xmin = 0
		}
		xmax := int(center + support + 0.5)
		if xmax > inSize {
			xmax = inSize
		}
		count := xmax - xmin
		if count < 0 {
			count = 0
		}
		if count > ksize {
			count = ksize
		}
		total := 0.0
		for i := 0; i < count; i++ {
			w := widgetTriangle((float64(xmin+i) - center + 0.5) * invScale)
			k[i] = w
			total += w
		}
		if total != 0 {
			for i := 0; i < count; i++ {
				k[i] /= total
			}
		}
		// Pillow's normalize_coeffs_8bpc: round away from zero into fixed point.
		out := c.weight[xx*ksize : xx*ksize+ksize]
		for i := 0; i < count; i++ {
			scaled := k[i] * float64(int32(1)<<widgetPrecisionBits)
			if scaled < 0 {
				out[i] = int32(scaled - 0.5)
			} else {
				out[i] = int32(scaled + 0.5)
			}
		}
		c.min[xx] = xmin
		c.count[xx] = count
	}
	return c
}

// widgetResampleAxis resamples one axis. horizontal selects the axis; the two
// passes are separate images, as in Pillow, so each rounds to 8 bits before the
// next reads it.
func widgetResampleAxis(src *widgetRGB, outSize int, horizontal bool) *widgetRGB {
	inSize := src.H
	if horizontal {
		inSize = src.W
	}
	c := widgetPrecomputeCoeffs(inSize, outSize)

	var dst *widgetRGB
	if horizontal {
		dst = newWidgetRGB(outSize, src.H)
	} else {
		dst = newWidgetRGB(src.W, outSize)
	}

	// Pillow pre-loads the accumulator with half an LSB so the final shift
	// rounds instead of truncating.
	const half = int64(1) << (widgetPrecisionBits - 1)

	if horizontal {
		for y := 0; y < src.H; y++ {
			rowBase := y * src.W * 3
			dstBase := y * dst.W * 3
			for xx := 0; xx < outSize; xx++ {
				k := c.weight[xx*c.ksize:]
				si := rowBase + c.min[xx]*3
				r, g, b := half, half, half
				for i := 0; i < c.count[xx]; i++ {
					w := int64(k[i])
					r += w * int64(src.Pix[si])
					g += w * int64(src.Pix[si+1])
					b += w * int64(src.Pix[si+2])
					si += 3
				}
				di := dstBase + xx*3
				dst.Pix[di] = widgetClip8(r)
				dst.Pix[di+1] = widgetClip8(g)
				dst.Pix[di+2] = widgetClip8(b)
			}
		}
		return dst
	}

	stride := src.W * 3
	for yy := 0; yy < outSize; yy++ {
		k := c.weight[yy*c.ksize:]
		dstBase := yy * dst.W * 3
		for x := 0; x < src.W; x++ {
			si := c.min[yy]*stride + x*3
			r, g, b := half, half, half
			for i := 0; i < c.count[yy]; i++ {
				w := int64(k[i])
				r += w * int64(src.Pix[si])
				g += w * int64(src.Pix[si+1])
				b += w * int64(src.Pix[si+2])
				si += stride
			}
			di := dstBase + x*3
			dst.Pix[di] = widgetClip8(r)
			dst.Pix[di+1] = widgetClip8(g)
			dst.Pix[di+2] = widgetClip8(b)
		}
	}
	return dst
}

// widgetClip8 is Pillow's clip8: shift the fixed-point accumulator down and
// clamp to [0,255].
func widgetClip8(acc int64) uint8 {
	v := acc >> widgetPrecisionBits
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v)
}

// widgetEncodeJPEG encodes at the given libjpeg-scale quality. optimize=False in
// the Python means no optimised Huffman tables, which is also what Go's encoder
// does — it always writes the standard tables.
func widgetEncodeJPEG(img *widgetRGB, quality int) ([]byte, error) {
	// image/jpeg has a fast path for *image.RGBA; going through widgetRGB's
	// At() would cost an interface call and a color conversion per pixel.
	rgba := image.NewRGBA(image.Rect(0, 0, img.W, img.H))
	for y := 0; y < img.H; y++ {
		si := y * img.W * 3
		di := rgba.PixOffset(0, y)
		for x := 0; x < img.W; x++ {
			rgba.Pix[di] = img.Pix[si]
			rgba.Pix[di+1] = img.Pix[si+1]
			rgba.Pix[di+2] = img.Pix[si+2]
			rgba.Pix[di+3] = 0xff
			si += 3
			di += 4
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, rgba, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("cannot encode preview JPEG: %w", err)
	}
	return buf.Bytes(), nil
}

// widgetCompressPreview is the whole pipeline: decode, downscale if wider than
// maxWidth, re-encode as JPEG. It returns the JPEG bytes and the frame size.
func widgetCompressPreview(pngBytes []byte, maxWidth, quality int) ([]byte, int, int, error) {
	src, err := widgetDecodePNG(pngBytes)
	if err != nil {
		return nil, 0, 0, err
	}
	if src.W <= 0 || src.H <= 0 {
		return nil, 0, 0, fmt.Errorf("screenshot has no pixels (%dx%d)", src.W, src.H)
	}
	dstW, dstH := widgetPreviewSize(src.W, src.H, maxWidth)
	frame := widgetResizeRGB(src, dstW, dstH)
	encoded, err := widgetEncodeJPEG(frame, quality)
	if err != nil {
		return nil, 0, 0, err
	}
	return encoded, frame.W, frame.H, nil
}
