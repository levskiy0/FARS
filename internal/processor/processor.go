package processor

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"

	"github.com/h2non/bimg"
)

// ErrDimensionsTooLarge indicates that the requested geometry, or an axis
// derived from it, would exceed the caller-supplied MaxWidth/MaxHeight
// envelope. Callers map this to a client error.
var ErrDimensionsTooLarge = errors.New("requested dimensions exceed configured limits")

// ErrDegenerateGeometry indicates the requested geometry would shrink the
// source below one pixel on an axis — libvips refuses to produce that image
// ("shrunk to nothing"), so it is reported as a client error rather than a
// service failure.
var ErrDegenerateGeometry = errors.New("requested geometry shrinks the source below one pixel")

// ErrUnsupportedSource indicates libvips could not read the payload as an
// image at all (e.g. an image-extension file whose bytes are not an image).
var ErrUnsupportedSource = errors.New("source is not a supported image")

// Format enumerates supported output formats.
type Format string

const (
	FormatJPEG Format = "jpeg"
	FormatPNG  Format = "png"
	FormatWEBP Format = "webp"
	FormatAVIF Format = "avif"
)

// Options describe a resize request.
type Options struct {
	Width          int
	Height         int
	Format         Format
	JPEGQuality    int
	WebPQuality    int
	AVIFQuality    int
	AVIFSpeed      int
	PNGCompression int
	EnsureOpaque   bool
	// MaxWidth and MaxHeight cap the canvas this request may produce,
	// including an axis derived from the source aspect ratio when the other
	// is 0. A value <= 0 disables the corresponding check (used by tests
	// that construct Options directly, without going through the handler).
	MaxWidth  int
	MaxHeight int
}

// Processor wraps libvips via bimg to transform images.
type Processor struct{}

// New creates a new Processor instance.
func New() *Processor {
	return &Processor{}
}

// Resize applies the provided options to the source payload.
func (p *Processor) Resize(source []byte, opts Options) ([]byte, error) {
	if len(source) == 0 {
		// A truncated or zero-length original is undecodable input, not a
		// service failure: same class as bytes libvips cannot read at all.
		return nil, fmt.Errorf("%w: source payload is empty", ErrUnsupportedSource)
	}
	if converted := toSRGB(source); converted != nil {
		source = converted
	}
	img := bimg.NewImage(source)

	size, err := img.Size()
	if err != nil {
		return nil, fmt.Errorf("inspect source size: %w: %w", ErrUnsupportedSource, err)
	}
	if size.Width <= 0 || size.Height <= 0 {
		return nil, fmt.Errorf("%w: source reports %dx%d", ErrUnsupportedSource, size.Width, size.Height)
	}
	if opts.Width == 0 && opts.Height == 0 {
		opts = fitEnvelope(opts, size)
	}
	switch {
	case opts.Width > 0 && opts.Height > 0:
		widthRatio := float64(opts.Width) / float64(size.Width)
		heightRatio := float64(opts.Height) / float64(size.Height)
		scale := math.Min(widthRatio, heightRatio)
		if scale > 1 {
			scale = 1
		}
		if scale < 1 {
			contentWidth := int(math.Round(float64(size.Width) * scale))
			contentHeight := int(math.Round(float64(size.Height) * scale))
			if contentWidth < 1 || contentHeight < 1 {
				// e.g. a 40x4000 source asked for 200x20: the content would
				// be 0.2px wide. libvips fails this with "shrunk to nothing";
				// the geometry is unrealizable for this source, so say so.
				return nil, fmt.Errorf("%w: %dx%d source scaled to %dx%d for a %dx%d request",
					ErrDegenerateGeometry, size.Width, size.Height, contentWidth, contentHeight, opts.Width, opts.Height)
			}
			limitByWidth := widthRatio <= heightRatio
			stageWidth := 0
			stageHeight := 0
			if limitByWidth {
				stageWidth = contentWidth
			} else {
				stageHeight = contentHeight
			}
			srcImg := img
			if needsFlatten(source, opts) {
				flattened, flatErr := p.flattenToWhite(source)
				if flatErr != nil {
					return nil, fmt.Errorf("flatten source: %w", flatErr)
				}
				srcImg = bimg.NewImage(flattened)
			}
			stage, err := srcImg.Process(bimg.Options{
				Type:          bimg.PNG,
				StripMetadata: true,
				NoAutoRotate:  false,
				Width:         stageWidth,
				Height:        stageHeight,
				Embed:         false,
				Force:         true,
				// The staging buffer is decoded again a few lines down and
				// never leaves the process, so deflating it is paid twice for
				// nothing. PNG is lossless at every level: same pixels.
				Compression: 0,
			})
			if err != nil {
				return nil, fmt.Errorf("shrink source: %w", err)
			}
			return p.renderCanvas(stage, opts)
		}
		return p.resizeWithCanvas(img, opts)
	case opts.Width > 0 && opts.Height == 0:
		if opts.Width > size.Width {
			canvas := opts
			scale := float64(size.Height) / float64(size.Width)
			canvas.Height = int(math.Round(float64(opts.Width) * scale))
			if canvas.Height < size.Height {
				canvas.Height = size.Height
			}
			// The explicit axis was already checked by the caller; only the
			// derived one can still exceed its cap. We reject rather than
			// clamp so the response dimensions always match what was asked
			// for instead of silently shrinking one axis but not the other.
			if err := checkAxisCap(canvas.Height, opts.MaxHeight, "height"); err != nil {
				return nil, err
			}
			return p.resizeWithCanvas(img, canvas)
		}
	case opts.Height > 0 && opts.Width == 0:
		if opts.Height > size.Height {
			canvas := opts
			scale := float64(size.Width) / float64(size.Height)
			canvas.Width = int(math.Round(float64(opts.Height) * scale))
			if canvas.Width < size.Width {
				canvas.Width = size.Width
			}
			if err := checkAxisCap(canvas.Width, opts.MaxWidth, "width"); err != nil {
				return nil, err
			}
			return p.resizeWithCanvas(img, canvas)
		}
	}
	// Everything that reaches here hands bimg at most one axis and lets it
	// derive the other from the source aspect ratio. That derived axis is
	// just as client-controlled as the explicit one, so it gets the same cap
	// and the same whole-canvas budget check before any pixels are allocated.
	outWidth, outHeight := opts.Width, opts.Height
	switch {
	case outWidth > 0 && outHeight == 0:
		outHeight = deriveAxis(outWidth, size.Width, size.Height)
	case outHeight > 0 && outWidth == 0:
		outWidth = deriveAxis(outHeight, size.Height, size.Width)
	case outWidth == 0 && outHeight == 0:
		// No envelope to fit into (MaxWidth/MaxHeight unset): the source is
		// only transcoded, so it keeps its own size.
		outWidth, outHeight = size.Width, size.Height
	}
	if outWidth < 1 || outHeight < 1 {
		// The free axis scaled to less than a pixel (a very tall source asked
		// for a small height, or the mirror of it). libvips refuses it, so
		// this is reported as the client error it is instead of a 500.
		return nil, fmt.Errorf("%w: %dx%d source scaled to %dx%d for a %dx%d request",
			ErrDegenerateGeometry, size.Width, size.Height, outWidth, outHeight, opts.Width, opts.Height)
	}
	if err := checkAxisCap(outWidth, opts.MaxWidth, "width"); err != nil {
		return nil, err
	}
	if err := checkAxisCap(outHeight, opts.MaxHeight, "height"); err != nil {
		return nil, err
	}
	if err := checkCanvasBudget(outWidth, outHeight, opts.MaxWidth, opts.MaxHeight); err != nil {
		return nil, err
	}

	options, err := buildBaseOptions(opts)
	if err != nil {
		return nil, err
	}
	options.Width = opts.Width
	options.Height = opts.Height
	if opts.Width > 0 && opts.Height > 0 {
		options.Embed = true
		options.Crop = false
		options.Gravity = bimg.GravityCentre
	}
	result, err := img.Process(options)
	if err != nil {
		return nil, fmt.Errorf("process image: %w", err)
	}
	return result, nil
}

func (p *Processor) resizeWithCanvas(img *bimg.Image, opts Options) ([]byte, error) {
	stage, err := img.Process(bimg.Options{
		Type:          bimg.PNG,
		StripMetadata: true,
		NoAutoRotate:  false,
		Embed:         false,
		Force:         false,
		Compression:   0, // intermediate only, see the shrink path
	})
	if err != nil {
		return nil, fmt.Errorf("prepare source for canvas: %w", err)
	}
	return p.renderCanvas(stage, opts)
}

func (p *Processor) renderCanvas(stage []byte, opts Options) ([]byte, error) {
	// Belt-and-braces: whatever path computed opts.Width/opts.Height, never
	// let image.NewNRGBA allocate more than the configured envelope allows.
	if err := checkCanvasBudget(opts.Width, opts.Height, opts.MaxWidth, opts.MaxHeight); err != nil {
		return nil, err
	}

	decoded, err := png.Decode(bytes.NewReader(stage))
	if err != nil {
		return nil, fmt.Errorf("decode intermediate image: %w", err)
	}

	canvas := image.NewNRGBA(image.Rect(0, 0, opts.Width, opts.Height))
	if opts.Format == FormatJPEG {
		draw.Draw(canvas, canvas.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
	}

	sourceBounds := decoded.Bounds()
	contentWidth := sourceBounds.Dx()
	contentHeight := sourceBounds.Dy()
	left := int(math.Max(0, float64(opts.Width-contentWidth)/2))
	top := int(math.Max(0, float64(opts.Height-contentHeight)/2))
	position := image.Rect(left, top, left+contentWidth, top+contentHeight)
	draw.Draw(canvas, position, decoded, sourceBounds.Min, draw.Over)

	var buf bytes.Buffer
	// This PNG exists only to hand the canvas to libvips for the final
	// encode. BestSpeed is the same image in a fraction of the CPU.
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&buf, canvas); err != nil {
		return nil, fmt.Errorf("encode canvas: %w", err)
	}

	finalOptions, err := buildBaseOptions(opts)
	if err != nil {
		return nil, err
	}
	finalOptions.Width = 0
	finalOptions.Height = 0
	finalOptions.Embed = false

	result, err := bimg.NewImage(buf.Bytes()).Process(finalOptions)
	if err != nil {
		return nil, fmt.Errorf("render final image: %w", err)
	}
	return result, nil
}

// fitEnvelope expands a request with both axes zero — what the PrestaShop
// module emits when it knows no dimensions ("0x0") — into the largest box
// inside MaxWidth x MaxHeight that keeps the source aspect ratio, never
// upscaling. Without an envelope there is nothing to fit into, so the request
// stays a plain transcode of the source at its own size.
func fitEnvelope(opts Options, size bimg.ImageSize) Options {
	if opts.MaxWidth <= 0 || opts.MaxHeight <= 0 {
		return opts
	}
	scale := math.Min(
		float64(opts.MaxWidth)/float64(size.Width),
		float64(opts.MaxHeight)/float64(size.Height),
	)
	if scale >= 1 {
		opts.Width = size.Width
		opts.Height = size.Height
		return opts
	}
	opts.Width = max(1, int(math.Round(float64(size.Width)*scale)))
	opts.Height = max(1, int(math.Round(float64(size.Height)*scale)))
	return opts
}

// deriveAxis returns the length of the free axis when only `target` is given:
// srcOther scaled by target/srcTarget, the same ratio libvips applies. It can
// come back as 0 for a degenerate request; the caller checks for that.
func deriveAxis(target, srcTarget, srcOther int) int {
	return int(math.Round(float64(srcOther) * float64(target) / float64(srcTarget)))
}

// checkAxisCap rejects a derived axis that exceeds its configured maximum.
// max <= 0 means the caller did not supply a limit (e.g. a unit test
// constructing Options directly), so the check is skipped.
func checkAxisCap(value, max int, axis string) error {
	if max > 0 && value > max {
		return fmt.Errorf("%w: derived %s %d exceeds limit %d", ErrDimensionsTooLarge, axis, value, max)
	}
	return nil
}

// checkCanvasBudget rejects a canvas whose total pixel count exceeds
// maxWidth*maxHeight, independent of how each axis was arrived at.
func checkCanvasBudget(width, height, maxWidth, maxHeight int) error {
	if maxWidth <= 0 || maxHeight <= 0 {
		return nil
	}
	budget := int64(maxWidth) * int64(maxHeight)
	if int64(width)*int64(height) > budget {
		return fmt.Errorf("%w: canvas %dx%d exceeds pixel budget %d", ErrDimensionsTooLarge, width, height, budget)
	}
	return nil
}

func buildBaseOptions(opts Options) (bimg.Options, error) {
	options := bimg.Options{
		StripMetadata: true,
		Embed:         true,
		Force:         false,
		NoAutoRotate:  false,
		Interlace:     true,
	}
	if opts.EnsureOpaque {
		options.Background = bimg.Color{R: 255, G: 255, B: 255}
		options.Extend = bimg.ExtendBackground
	}
	switch opts.Format {
	case FormatJPEG:
		options.Type = bimg.JPEG
		options.Quality = opts.JPEGQuality
	case FormatPNG:
		options.Type = bimg.PNG
		options.Compression = opts.PNGCompression
	case FormatWEBP:
		options.Type = bimg.WEBP
		options.Quality = opts.WebPQuality
	case FormatAVIF:
		options.Type = bimg.AVIF
		options.Quality = opts.AVIFQuality
		options.Speed = opts.AVIFSpeed
	default:
		return bimg.Options{}, fmt.Errorf("unsupported format %q", opts.Format)
	}
	return options, nil
}

// toSRGB converts a source that libvips does not read as RGB — in practice a
// CMYK JPEG straight out of a print workflow — into sRGB, and returns nil when
// the source is already fine or cannot be converted.
//
// bimg only applies the interpretation at save time, so every step before that
// (flatten, embed, background) sees the raw band count. A CMYK image has four
// bands, which makes bimg's vipsHasAlpha treat it as having alpha and hand
// vips_flatten a three-element background vector; libvips then fails the whole
// render with "linear: vector must have 1 or 4 elements" and the request 500s.
func toSRGB(source []byte) []byte {
	interpretation, err := bimg.NewImage(source).Interpretation()
	if err != nil {
		return nil
	}

	switch interpretation {
	case bimg.InterpretationSRGB, bimg.InterpretationRGB, bimg.InterpretationBW:
		return nil
	}

	// Best effort: an unconvertible source is no worse off than before.
	converted, err := bimg.NewImage(source).Colourspace(bimg.InterpretationSRGB)
	if err != nil {
		return nil
	}

	return converted
}

// needsFlatten reports whether the source has to be composited onto white
// before it is shrunk.
//
// Only a source that actually carries an alpha channel does. For an opaque
// one — every JPEG in a product catalogue — the flatten was a detour through
// a full-resolution PNG whose result is the source again, and it cost more
// than the resize it was preparing for: 1.36s and 114MB of allocation for a
// 3000x2000 photo to 600x600, against 83ms and 6.7MB when the shrink reads
// the original directly.
//
// The shrink then resamples from the original rather than from that PNG, so
// libvips can scale on load. The output is no longer byte-identical to what
// the detour produced (mean absolute difference 1.5/255 on a catalogue photo,
// i.e. invisible, but a different ETag), which is why this is its own commit.
func needsFlatten(source []byte, opts Options) bool {
	if !opts.EnsureOpaque && opts.Format != FormatJPEG {
		return false
	}
	metadata, err := bimg.NewImage(source).Metadata()
	if err != nil {
		// Unreadable metadata is not a licence to skip the flatten.
		return true
	}
	return metadata.Alpha
}

// flattenToWhite composites the image onto a white background, removing
// transparency.
func (p *Processor) flattenToWhite(source []byte) ([]byte, error) {
	pngData, err := bimg.NewImage(source).Process(bimg.Options{
		Type:        bimg.PNG,
		Compression: 0, // intermediate only
	})
	if err != nil {
		return nil, fmt.Errorf("convert to png: %w", err)
	}

	decoded, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil, fmt.Errorf("decode png: %w", err)
	}

	bounds := decoded.Bounds()
	flat := image.NewNRGBA(bounds)

	draw.Draw(flat, bounds, &image.Uniform{color.White}, image.Point{}, draw.Src)
	draw.Draw(flat, bounds, decoded, bounds.Min, draw.Over)

	var buf bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&buf, flat); err != nil {
		return nil, fmt.Errorf("encode flattened: %w", err)
	}
	return buf.Bytes(), nil
}
