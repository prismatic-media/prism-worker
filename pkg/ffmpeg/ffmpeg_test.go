package ffmpeg

import (
	"strings"
	"testing"
)

func TestBuildTranscodeArgs(t *testing.T) {
	opts := TranscodeOptions{
		InputPath:       "/video.mp4",
		SegmentDuration: 4,
	}
	profile := RenditionProfile{
		Name:          "720p",
		Height:        720,
		VideoBitrateK: 2500,
		AudioBitrateK: 128,
	}
	scaleFilter := "scale=-2:720"

	tests := []struct {
		name         string
		hwaccel      string
		expectedCodec string
		expectDevice bool
		expectHwup   bool
	}{
		{
			name:         "none",
			hwaccel:      "none",
			expectedCodec: "libx264",
			expectDevice: false,
			expectHwup:   false,
		},
		{
			name:         "nvenc",
			hwaccel:      "nvenc",
			expectedCodec: "h264_nvenc",
			expectDevice: false,
			expectHwup:   false,
		},
		{
			name:         "qsv",
			hwaccel:      "qsv",
			expectedCodec: "h264_qsv",
			expectDevice: false,
			expectHwup:   false,
		},
		{
			name:         "videotoolbox",
			hwaccel:      "videotoolbox",
			expectedCodec: "h264_videotoolbox",
			expectDevice: false,
			expectHwup:   false,
		},
		{
			name:         "vaapi",
			hwaccel:      "vaapi",
			expectedCodec: "h264_vaapi",
			expectDevice: true,
			expectHwup:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testOpts := opts
			testOpts.HWAccelType = tt.hwaccel
			codec, args := buildTranscodeArgs(testOpts, profile, scaleFilter)

			if codec != tt.expectedCodec {
				t.Errorf("codec = %q, want %q", codec, tt.expectedCodec)
			}

			// Validate if init_hw_device is present
			hasDevice := false
			for i, arg := range args {
				if arg == "-init_hw_device" {
					hasDevice = true
					if i+2 >= len(args) || args[i+1] != "vaapi=vaapi" {
						t.Errorf("invalid -init_hw_device format: %v", args[i:i+3])
					}
				}
			}
			if hasDevice != tt.expectDevice {
				t.Errorf("hasDevice = %v, want %v", hasDevice, tt.expectDevice)
			}

			// Validate if hwupload/format is present in scale filter
			var scaleArg string
			for i, arg := range args {
				if arg == "-vf" {
					scaleArg = args[i+1]
				}
			}
			hasHwup := strings.Contains(scaleArg, "hwupload")
			if hasHwup != tt.expectHwup {
				t.Errorf("scaleArg %q has hwupload = %v, want %v", scaleArg, hasHwup, tt.expectHwup)
			}
		})
	}
}
