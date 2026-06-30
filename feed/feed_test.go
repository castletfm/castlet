package feed_test

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/castletfm/castlet/feed"
	"github.com/castletfm/castlet/model"
	"github.com/stretchr/testify/require"
)

func TestBuild(t *testing.T) {
	pub := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	ch := &model.Channel{ID: "chan1", Title: "My Show", Description: "desc", Language: "en"}
	eps := []*model.Episode{
		{ID: "ep1", Title: "Episode 1", Description: "first", MediaKey: "k1",
			MediaMIME: "audio/mpeg", MediaBytes: 1234, DurationSecs: 95, PublishedAt: &pub},
		{ID: "vid", Title: "Video", Description: "vod", MediaKey: "k2",
			MediaMIME: "video/mp4", MediaBytes: 999, PublishedAt: &pub},
	}

	out, err := feed.Build("https://example.com/", ch, eps)
	require.NoError(t, err)

	// well-formed XML
	require.NoError(t, xml.Unmarshal(out, new(struct {
		XMLName xml.Name `xml:"rss"`
	})))

	s := string(out)
	require.Contains(t, s, "<title>My Show</title>")
	require.Contains(t, s, "https://example.com/e/ep1/")
	require.Contains(t, s, "<guid isPermaLink=\"false\">urn:castlet:episode:ep1</guid>")
	require.Contains(t, s, `url="https://example.com/media/k1"`)
	require.Contains(t, s, `type="audio/mpeg"`)
	require.Contains(t, s, `type="video/mp4"`)
	require.Contains(t, s, "<itunes:duration>1:35</itunes:duration>")
	require.True(t, strings.HasPrefix(s, "<?xml"))
}
