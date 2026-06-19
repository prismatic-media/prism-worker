// Package dash provides MPEG-DASH MPD generation.
package dash

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RenditionInfo describes a single adaptive bitrate rendition.
type RenditionInfo struct {
	Name          string
	Height        int
	VideoBitrateK int
	AudioBitrateK int
	Codec         string // e.g. "h264", "hevc", "av1"
}

// SubtitleInfo describes an extracted subtitle track.
type SubtitleInfo struct {
	Language string // e.g. "eng"
	VTTPath  string // absolute path to the .vtt file
}

// GenerateMPD creates a DASH MPD XML file at mpdPath that references the
// fMP4 segments produced by ffmpeg. Each rendition directory is expected to
// contain init.mp4 and seg_00001.m4s … seg_NNNNN.m4s files.
func GenerateMPD(outputDir, mpdPath string, renditions []RenditionInfo, subtitles []SubtitleInfo, duration float64) error {
	var sb strings.Builder

	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	sb.WriteString(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"` + "\n")
	sb.WriteString(`     profiles="urn:mpeg:dash:profile:isoff-live:2011"` + "\n")
	sb.WriteString(`     type="static"` + "\n")
	sb.WriteString(`     minBufferTime="PT2S"` + "\n")
	sb.WriteString(fmt.Sprintf(`     mediaPresentationDuration="PT%.3fS">`, duration) + "\n")
	sb.WriteString(`  <Period>` + "\n")

	// Group renditions by video codec
	byCodec := make(map[string][]RenditionInfo)
	for _, r := range renditions {
		c := r.Codec
		if c == "" {
			c = "h264"
		}
		byCodec[c] = append(byCodec[c], r)
	}

	// For each codec group, generate an AdaptationSet
	for codecName, group := range byCodec {
		var dashCodec string
		switch codecName {
		case "hevc":
			dashCodec = "hvc1.1.6.L150.B0"
		case "av1":
			dashCodec = "av01.0.08M.08"
		default:
			dashCodec = "avc1.42E01E"
		}

		fmt.Fprintf(&sb, `    <AdaptationSet mimeType="video/mp4" codecs="%s,mp4a.40.2" segmentAlignment="true">`+"\n", dashCodec)
		for _, r := range group {
			bandwidth := (r.VideoBitrateK + r.AudioBitrateK) * 1000
			relDir := r.Name // relative path from MPD location

			fmt.Fprintf(&sb, `      <Representation id=%q bandwidth="%d" width="auto" height="%d">`+"\n",
				r.Name, bandwidth, r.Height)
			fmt.Fprintf(&sb, `        <BaseURL>segments/%s/</BaseURL>`+"\n", relDir)
			sb.WriteString(`        <SegmentTemplate initialization="init.mp4" media="seg_$Number%05d$.m4s" startNumber="1" duration="4"/>` + "\n")
			sb.WriteString(`      </Representation>` + "\n")
		}
		sb.WriteString(`    </AdaptationSet>` + "\n")
	}

	// Text adaptation sets (one per subtitle track).
	for _, sub := range subtitles {
		relVTT := filepath.Base(sub.VTTPath)
		fmt.Fprintf(&sb, `    <AdaptationSet mimeType="text/vtt" lang=%q>`+"\n",
			sub.Language)
		fmt.Fprintf(&sb, `      <Representation id="sub_%s" bandwidth="0">`+"\n", sub.Language)
		fmt.Fprintf(&sb, `        <BaseURL>segments/%s</BaseURL>`+"\n", relVTT)
		sb.WriteString(`      </Representation>` + "\n")
		sb.WriteString(`    </AdaptationSet>` + "\n")
	}

	sb.WriteString(`  </Period>` + "\n")
	sb.WriteString(`</MPD>` + "\n")

	if err := os.MkdirAll(filepath.Dir(mpdPath), 0o755); err != nil {
		return fmt.Errorf("creating mpd directory: %w", err)
	}
	return os.WriteFile(mpdPath, []byte(sb.String()), 0o644)
}
