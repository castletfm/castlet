// Package model holds Castlet's domain entities. It is pure data: no I/O,
// no persistence, no framework types, so every other package can depend on it
// without taking on heavier dependencies.
package model

import (
	"strings"
	"time"
)

// User is a person who creates podcasts. A user owns channels and
// authenticates to the admin area, by password and/or an OIDC identity.
type User struct {
	ID           string
	Email        string
	DisplayName  string
	PasswordHash string // empty for OIDC-only accounts
	OIDCIssuer   string // identity provider issuer, empty if not linked
	OIDCSubject  string // stable subject within the issuer, empty if not linked
	CreatedAt    time.Time
}

// Channel groups episodes. A user may own multiple channels. ID is the opaque,
// URL-facing identifier (channels are addressed as /c/{id}/).
type Channel struct {
	ID          string
	UserID      string
	Title       string
	Description string
	Language    string // BCP-47 tag, e.g. "en"; used in the RSS feed
	ImageKey    string // blob key for cover art, empty if none
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// EpisodeStatus is the publication state of an episode.
type EpisodeStatus string

const (
	EpisodeDraft     EpisodeStatus = "draft"
	EpisodePublished EpisodeStatus = "published"
)

// MediaKind distinguishes an episode's uploaded media so the episode page can
// render the right player. It is derived from the upload's content type.
type MediaKind string

const (
	MediaAudio MediaKind = "audio"
	MediaVideo MediaKind = "video"
)

// DetectMediaKind classifies a MIME type. Anything that is not video/* is
// treated as audio (the common case and a safe default for the <audio> player).
func DetectMediaKind(mime string) MediaKind {
	if strings.HasPrefix(mime, "video/") {
		return MediaVideo
	}
	return MediaAudio
}

// TranscriptStatus tracks the asynchronous transcription lifecycle of an
// episode independently of its publication state.
type TranscriptStatus string

const (
	TranscriptNone       TranscriptStatus = "none"       // no transcript and none expected
	TranscriptPending    TranscriptStatus = "pending"    // queued, not yet started
	TranscriptProcessing TranscriptStatus = "processing" // a worker is transcribing
	TranscriptDone       TranscriptStatus = "done"       // transcript available
	TranscriptFailed     TranscriptStatus = "failed"     // transcription failed permanently
)

// Episode is a single podcast item (audio or video) belonging to one channel.
// ID is the opaque, URL-facing identifier (episodes are addressed as /e/{id}/).
type Episode struct {
	ID               string
	ChannelID        string
	Title            string
	Description      string
	MediaKey         string    // content-addressed blob key (sha256 of the media); immutable
	MediaMIME        string    // e.g. "audio/mpeg" or "video/mp4"
	MediaKind        MediaKind // audio or video, drives the player choice
	MediaBytes       int64
	DurationSecs     int
	Language         string // spoken-language hint for transcription (ISO-639, e.g. "ja"); "" = auto-detect
	Position         int    // manual sort order within the channel (ascending); ties fall back to newest-first
	Status           EpisodeStatus
	TranscriptStatus TranscriptStatus
	PublishedAt      *time.Time // set when Status becomes published
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// IsVideo reports whether the episode's media is video.
func (e *Episode) IsVideo() bool { return e.MediaKind == MediaVideo }

// Segment is one timestamped span of a transcript. Times are seconds from the
// start of the audio and drive the click-to-seek UI.
type Segment struct {
	StartSecs float64
	EndSecs   float64
	Text      string
}

// Transcript is the ordered set of segments produced for an episode.
type Transcript struct {
	EpisodeID string
	Language  string
	Segments  []Segment
	CreatedAt time.Time
}

// JobKind enumerates the kinds of asynchronous work the worker performs.
type JobKind string

const (
	// JobTranscribe requests transcription of an episode. Its payload is a
	// TranscribePayload encoded as JSON.
	JobTranscribe JobKind = "transcribe"
)

// JobStatus is the lifecycle state of a queued job.
type JobStatus string

const (
	JobPending    JobStatus = "pending"
	JobProcessing JobStatus = "processing"
	JobDone       JobStatus = "done"
	JobFailed     JobStatus = "failed"
)

// Job is a unit of asynchronous work persisted by the queue backend.
type Job struct {
	ID        string
	Kind      JobKind
	Payload   string // opaque JSON understood by the handler for Kind
	Status    JobStatus
	Attempts  int
	LastError string
	RunAfter  time.Time // earliest time the job may be claimed
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TranscribePayload is the JSON payload of a JobTranscribe job.
type TranscribePayload struct {
	EpisodeID string `json:"episode_id"`
}
