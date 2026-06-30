// Package feed renders an RSS 2.0 document (with the common iTunes podcast
// extensions) for a channel and its published episodes. Podcast clients poll
// this feed; episode enclosures point at Castlet's media endpoint.
package feed

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/castletfm/castlet/model"
)

const itunesNS = "http://www.itunes.com/dtds/podcast-1.0.dtd"

type rss struct {
	XMLName   xml.Name `xml:"rss"`
	Version   string   `xml:"version,attr"`
	ITunesNS  string   `xml:"xmlns:itunes,attr"`
	ChannelEl channel  `xml:"channel"`
}

type channel struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	Language    string `xml:"language,omitempty"`
	Image       *image `xml:"image,omitempty"`
	ITunesImage *iimg  `xml:"itunes:image,omitempty"`
	Items       []item `xml:"item"`
}

type image struct {
	URL   string `xml:"url"`
	Title string `xml:"title"`
	Link  string `xml:"link"`
}

type iimg struct {
	Href string `xml:"href,attr"`
}

type item struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	GUID        guid      `xml:"guid"`
	Description string    `xml:"description"`
	PubDate     string    `xml:"pubDate,omitempty"`
	Enclosure   enclosure `xml:"enclosure"`
	Duration    string    `xml:"itunes:duration,omitempty"`
}

type guid struct {
	Value       string `xml:",chardata"`
	IsPermaLink bool   `xml:"isPermaLink,attr"`
}

type enclosure struct {
	URL    string `xml:"url,attr"`
	Length int64  `xml:"length,attr"`
	Type   string `xml:"type,attr"`
}

// Build renders the feed XML. baseURL is the absolute site root (no trailing
// slash required), used to construct episode and media URLs.
func Build(baseURL string, ch *model.Channel, episodes []*model.Episode) ([]byte, error) {
	base := strings.TrimRight(baseURL, "/")
	channelLink := fmt.Sprintf("%s/%s/", base, ch.Slug)

	doc := rss{
		Version:  "2.0",
		ITunesNS: itunesNS,
		ChannelEl: channel{
			Title:       ch.Title,
			Link:        channelLink,
			Description: ch.Description,
			Language:    ch.Language,
		},
	}
	if ch.ImageKey != "" {
		imgURL := fmt.Sprintf("%s/media/%s", base, ch.ImageKey)
		doc.ChannelEl.Image = &image{URL: imgURL, Title: ch.Title, Link: channelLink}
		doc.ChannelEl.ITunesImage = &iimg{Href: imgURL}
	}

	for _, ep := range episodes {
		link := fmt.Sprintf("%s/%s/%s/", base, ch.Slug, ep.Slug)
		it := item{
			Title:       ep.Title,
			Link:        link,
			GUID:        guid{Value: link, IsPermaLink: true},
			Description: ep.Description,
			Enclosure: enclosure{
				URL:    fmt.Sprintf("%s/media/%s", base, ep.MediaKey),
				Length: ep.MediaBytes,
				Type:   ep.MediaMIME,
			},
		}
		if ep.PublishedAt != nil {
			it.PubDate = ep.PublishedAt.UTC().Format(time.RFC1123Z)
		}
		if ep.DurationSecs > 0 {
			it.Duration = formatDuration(ep.DurationSecs)
		}
		doc.ChannelEl.Items = append(doc.ChannelEl.Items, it)
	}

	body, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("feed: marshal: %w", err)
	}
	return append([]byte(xml.Header), body...), nil
}

func formatDuration(secs int) string {
	h := secs / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}
