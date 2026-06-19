package ffmpeg

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ProbeResult holds key media information extracted by ffprobe.
type ProbeResult struct {
	Duration        float64
	Width           int
	Height          int
	VideoCodec      string
	AudioCodec      string
	SubtitleStreams []SubtitleStream
}

// SubtitleStream describes an embedded subtitle track.
type SubtitleStream struct {
	Index    int
	Language string // e.g. "eng", "fra" — may be empty
	Codec    string // e.g. "subrip", "ass"
}

// bitmapSubtitleCodecs is the set of subtitle codecs that produce bitmap
// images rather than text. FFmpeg cannot convert these to WebVTT, so they
// are excluded from extraction.
var bitmapSubtitleCodecs = map[string]struct{}{
	"dvd_subtitle":      {},
	"dvdsub":            {},
	"pgssub":            {},
	"hdmv_pgs_subtitle": {},
	"xsub":              {},
	"dvb_subtitle":      {},
	"dvb_teletext":      {},
}

// Probe runs ffprobe on the given file and returns media metadata.
func Probe(ctx context.Context, ffprobePath, filePath string) (*ProbeResult, error) {
	args := []string{
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		filePath,
	}

	out, err := runCmd(ctx, ffprobePath, args)
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}

	var raw struct {
		Streams []struct {
			Index     int    `json:"index"`
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Tags      struct {
				Language string `json:"language"`
			} `json:"tags"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing ffprobe output: %w", err)
	}

	result := &ProbeResult{}

	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			result.VideoCodec = s.CodecName
			result.Width = s.Width
			result.Height = s.Height
		case "audio":
			result.AudioCodec = s.CodecName
		case "subtitle":
			if _, isBitmap := bitmapSubtitleCodecs[s.CodecName]; isBitmap {
				break // skip — cannot convert bitmap subtitles to WebVTT
			}
			result.SubtitleStreams = append(result.SubtitleStreams, SubtitleStream{
				Index:    s.Index,
				Language: s.Tags.Language,
				Codec:    s.CodecName,
			})
		}
	}

	if raw.Format.Duration != "" {
		result.Duration, _ = strconv.ParseFloat(raw.Format.Duration, 64)
	}

	return result, nil
}

// RenditionProfile defines a single DASH quality level.
type RenditionProfile struct {
	Name          string `json:"name"`
	Height        int    `json:"height"`
	Width         int    `json:"width"` // standard reference width (e.g. 1920 for 1080p); used for wide-format sources
	VideoBitrateK int    `json:"video_bitrate_k"`
	AudioBitrateK int    `json:"audio_bitrate_k"`
	Codec         string `json:"codec"` // e.g. "h264", "hevc", "av1"
}

// DefaultProfiles returns the standard set of DASH renditions.
func DefaultProfiles() []RenditionProfile {
	return []RenditionProfile{
		{Name: "360p",  Height: 360,  Width: 640,  VideoBitrateK: 400,  AudioBitrateK: 64,  Codec: "h264"},
		{Name: "480p",  Height: 480,  Width: 854,  VideoBitrateK: 800,  AudioBitrateK: 96,  Codec: "h264"},
		{Name: "720p",  Height: 720,  Width: 1280, VideoBitrateK: 2500, AudioBitrateK: 128, Codec: "h264"},
		{Name: "1080p", Height: 1080, Width: 1920, VideoBitrateK: 8000, AudioBitrateK: 192, Codec: "h264"},
	}
}

// TranscodeOptions configures a DASH transcode operation.
type TranscodeOptions struct {
	InputPath       string
	OutputDir       string
	Profiles        []RenditionProfile
	SegmentDuration int           // seconds, default 4
	Duration        float64       // total input duration in seconds; used for progress
	SourceWidth     int           // source video width in pixels; used to pick scale filter
	SourceHeight    int           // source video height in pixels; used to pick scale filter
	ProgressFn      func(float64) // called with 0–100 overall percent; may be nil
	SubtitleStreams []SubtitleStream
	HWAccelType     string
}

// reProgress matches FFmpeg progress lines: "out_time_ms=12345678"
var reProgress = regexp.MustCompile(`out_time_ms=(\d+)`)

// TranscodeDASH encodes the input file into MPEG-DASH fMP4 segments.
// Subtitle streams are extracted to WebVTT files alongside the segments.
// ProgressFn is called periodically with an estimated 0–100 percent value.
func TranscodeDASH(ctx context.Context, ffmpegPath string, opts TranscodeOptions) error {
	if opts.SegmentDuration == 0 {
		opts.SegmentDuration = 4
	}
	if len(opts.Profiles) == 0 {
		opts.Profiles = DefaultProfiles()
	}

	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	// One FFmpeg call per rendition. FFmpeg is run with its working directory
	// set to the rendition output dir and relative paths for all outputs.
	// This ensures the DASH muxer's .tmp-then-rename pattern stays within a
	// single directory, avoiding the ENOENT rename failure in FFmpeg 8.x that
	// occurs when absolute paths are used.
	nProfiles := len(opts.Profiles)
	for idx, p := range opts.Profiles {
		renditionDir := filepath.Join(opts.OutputDir, p.Name)
		if err := os.MkdirAll(renditionDir, 0o755); err != nil {
			return fmt.Errorf("creating rendition dir %s: %w", p.Name, err)
		}

		// Use the HLS muxer with fmp4 segment type instead of the DASH muxer.
		// The DASH muxer in FFmpeg 8.x uses an atomic tmp-rename pattern that
		// consistently fails with ENOENT due to a threading race when multiple
		// adaptation set segments are written concurrently. The HLS fmp4 muxer
		// writes segments directly to their final filenames with no rename, and
		// its output (init.mp4 + seg_NNNNN.m4s) is CMAF-compatible and valid
		// for use with a DASH MPD. The resulting .m3u8 playlist is ignored.
			// Scale filter: for wide-format sources (e.g. 1920×800) where the
			// profile width is the limiting dimension, scale by width to avoid
			// upscaling vertically. Otherwise scale by height (standard 16:9).
			var scaleFilter string
			if p.Width > 0 && opts.SourceWidth > 0 && opts.SourceHeight > 0 {
				// Height the source would reach if we scaled to profile width.
				scaledH := p.Width * opts.SourceHeight / opts.SourceWidth
				if scaledH < p.Height {
					// Source is wider than the profile's aspect ratio; clamp by width.
					scaleFilter = fmt.Sprintf("scale=%d:-2", p.Width)
				}
			}
			if scaleFilter == "" {
				scaleFilter = fmt.Sprintf("scale=-2:%d", p.Height)
			}

			_, args := buildTranscodeArgs(opts, p, scaleFilter)

		profileIdx := idx // capture for closure
		var progressFn func(float64)
		if opts.ProgressFn != nil && opts.Duration > 0 {
			progressFn = func(secs float64) {
				// Map elapsed seconds within this profile to an overall 0–100 pct.
				perProfile := secs / opts.Duration // fraction through this profile
				pct := (float64(profileIdx) + perProfile) / float64(nProfiles) * 100
				if pct > 99 {
					pct = 99
				}
				opts.ProgressFn(pct)
			}
		}

		if err := runTranscodeCmd(ctx, ffmpegPath, renditionDir, args, progressFn); err != nil {
			return fmt.Errorf("transcoding profile %s: %w", p.Name, err)
		}
	}

	// Extract subtitle streams to WebVTT.
	for _, sub := range opts.SubtitleStreams {
		if err := extractSubtitle(ctx, ffmpegPath, opts.InputPath, opts.OutputDir, sub); err != nil {
			_ = err // non-fatal
		}
	}

	return nil
}

// runTranscodeCmd runs a single FFmpeg encode command with its working
// directory set to dir. progressFn is called with the output time in seconds.
func runTranscodeCmd(ctx context.Context, ffmpegPath, dir string, args []string, progressFn func(float64)) error {
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.Dir = dir
	var stderr strings.Builder

	if progressFn != nil {
		pr, pw, pipeErr := os.Pipe()
		if pipeErr == nil {
			cmd.Stderr = pw
			go func() {
				defer func() { _ = pr.Close() }()
				scanner := bufio.NewScanner(pr)
				for scanner.Scan() {
					line := scanner.Text()
					if m := reProgress.FindStringSubmatch(line); m != nil {
						// out_time_ms is in microseconds despite the name.
						us, _ := strconv.ParseFloat(m[1], 64)
						progressFn(us / 1_000_000.0)
					}
					stderr.WriteString(line + "\n")
				}
			}()
			err := cmd.Run()
			_ = pw.Close()
			if err != nil {
				return fmt.Errorf("transcoding: %w — stderr: %s", err, stderr.String())
			}
			return nil
		}
	}

	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("transcoding: %w — stderr: %s", err, stderr.String())
	}
	return nil
}

// ExtractSubtitles extracts all subtitle streams from a file to WebVTT.
func ExtractSubtitles(ctx context.Context, ffmpegPath, inputPath, outputDir string, streams []SubtitleStream) error {
	for _, s := range streams {
		if err := extractSubtitle(ctx, ffmpegPath, inputPath, outputDir, s); err != nil {
			return err
		}
	}
	return nil
}

func extractSubtitle(ctx context.Context, ffmpegPath, inputPath, outputDir string, s SubtitleStream) error {
	lang := s.Language
	if lang == "" {
		lang = strconv.Itoa(s.Index)
	}
	outPath := filepath.Join(outputDir, "sub_"+lang+".vtt")
	args := []string{
		"-y", "-i", inputPath,
		"-map", fmt.Sprintf("0:%d", s.Index),
		"-c:s", "webvtt",
		outPath,
	}
	if _, err := runCmd(ctx, ffmpegPath, args); err != nil {
		return fmt.Errorf("extracting subtitle stream %d: %w", s.Index, err)
	}
	return nil
}

// SidecarSubtitle describes an external subtitle file found alongside a video.
type SidecarSubtitle struct {
	Language string // derived from filename, e.g. "eng" from "Movie.eng.srt"
	Path     string // absolute path to the sidecar file
	IsVTT    bool   // true = already WebVTT; false = SRT that needs conversion
}

// FindSidecarSubtitles looks for .srt and .vtt files in the same directory as
// inputPath that share the same base name (optionally with a language tag):
//
//	Movie.srt          → language ""
//	Movie.eng.srt      → language "eng"
//	Movie.fra.vtt      → language "fra"
func FindSidecarSubtitles(inputPath string) []SidecarSubtitle {
	dir := filepath.Dir(inputPath)
	base := strings.TrimSuffix(filepath.Base(inputPath), filepath.Ext(inputPath))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var results []SidecarSubtitle
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".srt" && ext != ".vtt" {
			continue
		}
		nameNoExt := strings.TrimSuffix(name, filepath.Ext(name))
		// Must start with the video base name.
		if !strings.HasPrefix(nameNoExt, base) {
			continue
		}

		// Derive language tag from the part after the base name.
		lang := strings.TrimPrefix(nameNoExt, base)
		lang = strings.TrimPrefix(lang, ".")
		lang = strings.ToLower(lang)

		results = append(results, SidecarSubtitle{
			Language: lang,
			Path:     filepath.Join(dir, name),
			IsVTT:    ext == ".vtt",
		})
	}
	return results
}

// CopySidecarSubtitle processes one external sidecar subtitle file into outputDir.
// .vtt files are copied as-is; .srt files are converted to WebVTT via ffmpeg.
// Returns the absolute path to the resulting .vtt file.
func CopySidecarSubtitle(ctx context.Context, ffmpegPath string, s SidecarSubtitle, outputDir string) (string, error) {
	lang := s.Language
	if lang == "" {
		lang = "default"
	}
	outPath := filepath.Join(outputDir, "sub_"+lang+".vtt")

	if s.IsVTT {
		// Copy the .vtt as-is.
		src, err := os.ReadFile(s.Path)
		if err != nil {
			return "", fmt.Errorf("reading sidecar vtt: %w", err)
		}
		if err := os.WriteFile(outPath, src, 0o644); err != nil {
			return "", fmt.Errorf("writing sidecar vtt: %w", err)
		}
		return outPath, nil
	}

	// Convert .srt → .vtt using ffmpeg.
	args := []string{
		"-y", "-i", s.Path,
		"-c:s", "webvtt",
		outPath,
	}
	if _, err := runCmd(ctx, ffmpegPath, args); err != nil {
		return "", fmt.Errorf("converting sidecar srt %s: %w", s.Path, err)
	}
	return outPath, nil
}

func runCmd(ctx context.Context, bin string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, stderr.String())
	}

	return stdout.Bytes(), nil
}

func buildTranscodeArgs(opts TranscodeOptions, p RenditionProfile, scaleFilter string) (string, []string) {
	var codec string
	switch p.Codec {
	case "hevc":
		switch opts.HWAccelType {
		case "nvenc":
			codec = "hevc_nvenc"
		case "qsv":
			codec = "hevc_qsv"
		case "videotoolbox":
			codec = "hevc_videotoolbox"
		case "vaapi":
			codec = "hevc_vaapi"
			scaleFilter += ",format=nv12,hwupload"
		default:
			codec = "libx265"
		}
	case "av1":
		switch opts.HWAccelType {
		case "nvenc":
			codec = "av1_nvenc"
		case "qsv":
			codec = "av1_qsv"
		case "vaapi":
			codec = "av1_vaapi"
			scaleFilter += ",format=nv12,hwupload"
		default:
			codec = "libsvtav1"
		}
	default: // h264
		switch opts.HWAccelType {
		case "nvenc":
			codec = "h264_nvenc"
		case "qsv":
			codec = "h264_qsv"
		case "videotoolbox":
			codec = "h264_videotoolbox"
		case "vaapi":
			codec = "h264_vaapi"
			scaleFilter += ",format=nv12,hwupload"
		default:
			codec = "libx264"
		}
	}

	var args []string
	if opts.HWAccelType == "vaapi" {
		args = append(args, "-init_hw_device", "vaapi=vaapi", "-filter_hw_device", "vaapi")
	}
	args = append(args,
		"-y", "-i", opts.InputPath,
		"-progress", "pipe:2",
		"-map", "0:v:0",
		"-map", "0:a:0",
		"-c:v", codec,
		"-b:v", fmt.Sprintf("%dk", p.VideoBitrateK),
		"-vf", scaleFilter,
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", opts.SegmentDuration),
		"-c:a", "aac",
		"-b:a", fmt.Sprintf("%dk", p.AudioBitrateK),
		"-f", "hls",
		"-hls_time", strconv.Itoa(opts.SegmentDuration),
		"-hls_segment_type", "fmp4",
		"-hls_flags", "independent_segments",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", "seg_%05d.m4s",
		"-start_number", "1",
		"stream.m3u8",
	)
	return codec, args
}
