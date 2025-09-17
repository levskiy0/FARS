package processor

import (
	"fmt"

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
	options := bimg.Options{
		Width:         opts.Width,
		Height:        opts.Height,
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
	default:
		return nil, fmt.Errorf("unsupported format %q", opts.Format)
	}
	result, err := img.Process(options)
	if err != nil {
		return nil, fmt.Errorf("process image: %w", err)
	}
	return result, nil
}
