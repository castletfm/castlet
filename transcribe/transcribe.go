// Package transcribe defines the speech-to-text boundary. The default
// implementation (transcribe/null) is a no-op suitable for a standalone
// install; transcribe/command shells out to a local whisper.cpp-style binary,
// and a deployment may implement Transcriber over a hosted API.
package transcribe

import (
	"context"
	"errors"
	"io"
)

// ErrUnsupported indicates the transcriber cannot produce a transcript (for
// example the null transcriber). The worker treats it as "no transcript
// expected" rather than a failure.
var ErrUnsupported = errors.New("transcribe: unsupported")

// Input is the audio to transcribe.
type Input struct {
	Audio    io.Reader
	MIME     string // e.g. "audio/mpeg"
	Filename string // hint for tools that key off extension
	Language string // BCP-47/ISO-639 hint (e.g. "ja"); "" means auto-detect
}

// Segment is one timestamped span of recognized speech. Times are seconds
// from the start of the audio.
type Segment struct {
	StartSecs float64
	EndSecs   float64
	Text      string
}

// Result is a completed transcription.
type Result struct {
	Language string
	Segments []Segment
}

// Transcriber converts audio into a timestamped transcript. Implementations
// must be safe for concurrent use.
type Transcriber interface {
	// Transcribe reads the audio from in and returns its transcript, or
	// ErrUnsupported if it does not perform transcription.
	Transcribe(ctx context.Context, in Input) (*Result, error)
}
