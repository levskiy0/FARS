package processor

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"

	"github.com/h2non/bimg"
)

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
		return nil, fmt.Errorf("source payload is empty")
	}
	img := bimg.NewImage(source)

	size, err := img.Size()
	if err != nil {
		return nil, fmt.Errorf("inspect source size: %w", err)
	}
	switch {
	case opts.Width > 0 && opts.Height > 0:
		if opts.Width > size.Width && opts.Height > size.Height {
			return p.resizeWithCanvas(img, opts)
		}
	case opts.Width > 0 && opts.Height == 0:
		if opts.Width > size.Width {
			canvas := opts
			scale := float64(size.Height) / float64(size.Width)
			canvas.Height = int(math.Round(float64(opts.Width) * scale))
			if canvas.Height < size.Height {
				canvas.Height = size.Height
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
			return p.resizeWithCanvas(img, canvas)
		}
	}
	options, err := buildBaseOptions(opts)
	if err != nil {
		return nil, err
	}
	options.Width = opts.Width
	options.Height = opts.Height
	if opts.Width > 0 && opts.Height > 0 {
		options.Embed = false
		options.Crop = true
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
	})
	if err != nil {
		return nil, fmt.Errorf("prepare source for canvas: %w", err)
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
	if err := png.Encode(&buf, canvas); err != nil {
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
