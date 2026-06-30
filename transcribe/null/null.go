// Package null is the default transcribe.Transcriber: it performs no
// transcription. A standalone install ships with it so episodes publish
// immediately and their transcript status settles to "none". Swap in
// transcribe/command or a hosted transcriber to get real transcripts.
package null

import (
	"context"

	"github.com/castletfm/castlet/transcribe"
)

// Transcriber is a no-op transcriber.
type Transcriber struct{}

var _ transcribe.Transcriber = Transcriber{}

// New returns a no-op Transcriber.
func New() Transcriber { return Transcriber{} }

// Transcribe always reports transcribe.ErrUnsupported.
func (Transcriber) Transcribe(ctx context.Context, in transcribe.Input) (*transcribe.Result, error) {
	return nil, transcribe.ErrUnsupported
}
