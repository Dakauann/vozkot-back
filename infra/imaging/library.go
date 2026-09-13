package imaging

import mediadomain "vozkot/domain/media"

// Library adapts the processor to the media domain's port.
//
// The adapter lives here rather than in the domain because the translation is
// this package's concern: the domain describes what it needs, and infra says
// how it is met.
type Library struct {
	processor *Processor
}

var _ mediadomain.ImageProcessor = (*Library)(nil)

// NewLibrary bounds concurrent decodes. Two is deliberate: a pure-Go resample
// of a large photo is hundreds of milliseconds and tens of megabytes, and the
// point of the bound is that a burst of uploads queues instead of exhausting
// memory.
func NewLibrary(concurrency int) *Library {
	return &Library{processor: NewProcessor(concurrency)}
}

func (l *Library) Process(data []byte) (*mediadomain.Derived, error) {
	result, err := l.processor.Process(data)
	if err != nil {
		return nil, err
	}
	variants := make([]mediadomain.DerivedVariant, 0, len(result.Variants))
	for _, variant := range result.Variants {
		variants = append(variants, mediadomain.DerivedVariant{
			Width:  variant.Width,
			Height: variant.Height,
			Data:   variant.Data,
		})
	}
	return &mediadomain.Derived{
		Width:         result.Width,
		Height:        result.Height,
		BlurDataURL:   result.BlurDataURL,
		DominantColor: result.DominantColor,
		Variants:      variants,
	}, nil
}
