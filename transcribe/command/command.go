// Package command is an opt-in transcribe.Transcriber that shells out to an
// external program (for example a whisper.cpp wrapper). The audio is written to
// a temporary file; the configured argument template is expanded and executed;
// and the program's stdout is parsed as JSON in the schema below.
//
// Expected stdout JSON:
//
//	{
//	  "language": "en",
//	  "segments": [
//	    {"start": 0.0, "end": 3.2, "text": "hello world"},
//	    ...
//	  ]
//	}
//
// This schema is intentionally simple; a one-line jq/wrapper can adapt most
// transcription tools to it.
package command

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/castletfm/castlet/transcribe"
	"github.com/lestrrat-go/option/v3"
)

// Transcriber runs an external command to transcribe audio.
type Transcriber struct {
	name    string   // executable
	args    []string // argument template; the token {{audio}} is replaced with the temp file path
	tempDir string   // staging dir for the audio temp file; "" means os.TempDir()
}

var _ transcribe.Transcriber = (*Transcriber)(nil)

// Option configures New.
type Option = option.Interface

type identArgs struct{}

type identTempDir struct{}

// WithArgs sets the argument template. The literal token "{{audio}}" in any
// argument is replaced with the path to the temporary audio file. The default
// is []string{"{{audio}}"}.
func WithArgs(args ...string) Option { return option.New(identArgs{}, args) }

// WithTempDir sets the directory in which the audio is staged to a temporary
// file before the command runs. When unset the OS default (os.TempDir) is used,
// so existing callers are unaffected. Point this at a directory on the volume
// provisioned for media so large or concurrent transcriptions cannot exhaust
// the system /tmp.
func WithTempDir(dir string) Option { return option.New(identTempDir{}, dir) }

// New returns a Transcriber that invokes the program name. By default the only
// argument is the audio file path.
func New(name string, options ...Option) *Transcriber {
	t := &Transcriber{name: name, args: []string{"{{audio}}"}}
	for _, o := range options {
		switch o.Ident().(type) {
		case identArgs:
			t.args = option.MustGet[[]string](o)
		case identTempDir:
			t.tempDir = option.MustGet[string](o)
		}
	}
	return t
}

func (t *Transcriber) Transcribe(ctx context.Context, in transcribe.Input) (*transcribe.Result, error) {
	tmp, err := os.CreateTemp(t.tempDir, "castlet-audio-*"+extFor(in))
	if err != nil {
		return nil, fmt.Errorf("command: temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in.Audio); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("command: stage audio: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("command: close audio: %w", err)
	}

	args := make([]string, len(t.args))
	for i, a := range t.args {
		args[i] = strings.ReplaceAll(a, "{{audio}}", tmp.Name())
	}

	// Bound the captured output so a verbose or stuck process can't exhaust the
	// worker's memory. stdout carries the transcript JSON (generous cap); stderr
	// is only diagnostic (small cap).
	stdout := &cappedBuffer{limit: maxStdoutBytes}
	stderr := &cappedBuffer{limit: maxStderrBytes}
	cmd := exec.CommandContext(ctx, t.name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Run in its own process group and kill the whole group on context
	// cancellation. exec.CommandContext otherwise signals only the direct child,
	// so grandchildren spawned by a wrapper (e.g. `docker run` or a whisper
	// subprocess) would keep consuming resources after a timed-out job is retried.
	setProcessGroup(cmd)
	// If a grandchild inherits and keeps the stdout/stderr pipes open past the
	// kill, don't let cmd.Wait block forever waiting for EOF. Set on all platforms
	// (the non-unix fallback relies on exec's default direct-child kill).
	cmd.WaitDelay = 10 * time.Second
	// Expose the episode's language to the command (e.g. to pick whisper's -l
	// flag and a matching punctuation prompt). Empty means auto-detect.
	if in.Language != "" {
		cmd.Env = append(os.Environ(), "CASTLET_LANGUAGE="+in.Language)
	}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("command: %s failed: %w: %s", t.name, err, strings.TrimSpace(stderr.String()))
	}
	// Fail loudly rather than parse a truncated transcript into a mis-parsed
	// (or, worse, silently short) result.
	if stdout.overflow {
		return nil, fmt.Errorf("command: %s produced more than %d bytes of stdout", t.name, maxStdoutBytes)
	}

	var out struct {
		Language string `json:"language"`
		Segments []struct {
			Start float64 `json:"start"`
			End   float64 `json:"end"`
			Text  string  `json:"text"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("command: parse output: %w", err)
	}

	res := &transcribe.Result{Language: out.Language}
	for _, s := range out.Segments {
		res.Segments = append(res.Segments, transcribe.Segment{
			StartSecs: s.Start,
			EndSecs:   s.End,
			Text:      strings.TrimSpace(s.Text),
		})
	}
	return res, nil
}

// Caps on captured child output. stdout holds the transcript JSON, so it gets a
// generous cap; stderr is only used for diagnostics.
const (
	maxStdoutBytes = 64 << 20 // 64 MiB
	maxStderrBytes = 1 << 20  // 1 MiB
)

// cappedBuffer is an io.Writer that accumulates at most limit bytes and then
// discards the rest, recording that an overflow happened. It reports every write
// as fully consumed so the child process is never blocked or errored by a short
// write; the excess is simply dropped to keep memory bounded.
type cappedBuffer struct {
	buf      bytes.Buffer
	limit    int
	overflow bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := c.limit - c.buf.Len(); remaining > 0 {
		if len(p) <= remaining {
			c.buf.Write(p)
			return len(p), nil
		}
		c.buf.Write(p[:remaining])
	}
	if len(p) > 0 {
		c.overflow = true
	}
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte  { return c.buf.Bytes() }
func (c *cappedBuffer) String() string { return c.buf.String() }

// extFor picks a file extension so tools that sniff by extension behave.
func extFor(in transcribe.Input) string {
	if ext := filepath.Ext(in.Filename); ext != "" {
		return ext
	}
	switch in.MIME {
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4", "audio/x-m4a":
		return ".m4a"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/ogg":
		return ".ogg"
	default:
		return ".bin"
	}
}
