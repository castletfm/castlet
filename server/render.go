package server

import (
	"fmt"
	"html/template"
	"io"
	"time"

	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/web"
)

// Renderer turns a named page plus a ViewData into HTML. It is the seam for an
// alternative frontend: a deployment can implement Renderer (e.g. against a
// different template engine or to emit a JSON API) and pass it via
// WithRenderer without changing any handler.
type Renderer interface {
	Render(w io.Writer, page string, data *ViewData) error
}

// ViewData is the uniform shape handed to every page. Site and User populate
// the shared layout; Data carries page-specific values.
type ViewData struct {
	Site  string
	User  *model.User
	Title string
	Data  any
}

// pageNames are the templates parsed at startup. Each is rendered as the
// "content" block inside base.html.
var pageNames = []string{
	"landing",
	"channel",
	"episode",
	"login",
	"admin_dashboard",
	"admin_channel_form",
	"admin_episodes",
	"admin_episode_form",
	"error",
}

type templateRenderer struct {
	pages map[string]*template.Template
}

func newTemplateRenderer() (*templateRenderer, error) {
	funcs := template.FuncMap{
		"formatDate":        func(t time.Time) string { return t.Format("Jan 2, 2006") },
		"formatDuration":    formatDuration,
		"formatTimecode":    formatTimecode,
		"transcriptMessage": transcriptMessage,
	}
	tr := &templateRenderer{pages: make(map[string]*template.Template, len(pageNames))}
	for _, name := range pageNames {
		// Parse base + this page in isolation so each page's "content" and
		// "head" defines override the base blocks without colliding.
		t, err := template.New("base.html").Funcs(funcs).ParseFS(web.Templates,
			"templates/base.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parse template %q: %w", name, err)
		}
		tr.pages[name] = t
	}
	return tr, nil
}

func (tr *templateRenderer) Render(w io.Writer, page string, data *ViewData) error {
	t, ok := tr.pages[page]
	if !ok {
		return fmt.Errorf("server: unknown page %q", page)
	}
	return t.ExecuteTemplate(w, "base.html", data)
}

// formatDuration renders whole seconds as h:mm:ss or m:ss.
func formatDuration(secs int) string {
	h := secs / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// formatTimecode renders a float-second transcript offset as m:ss (or h:mm:ss).
func formatTimecode(secs float64) string { return formatDuration(int(secs)) }

// transcriptMessage is the human-facing line shown when an episode has no
// transcript segments to display.
func transcriptMessage(status model.TranscriptStatus) string {
	switch status {
	case model.TranscriptPending:
		return "Transcript is queued and will appear once processing finishes."
	case model.TranscriptProcessing:
		return "Transcript is being generated…"
	case model.TranscriptFailed:
		return "Transcription failed for this episode."
	default:
		return "No transcript is available for this episode."
	}
}
