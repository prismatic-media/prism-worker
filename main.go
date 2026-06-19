package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/prismatic-media/prism-worker/pkg/dash"
	"github.com/prismatic-media/prism-worker/pkg/ffmpeg"
)

type WorkerConfig struct {
	ServerURL   string `json:"server_url" yaml:"server_url"`
	APIKey      string `json:"api_key" yaml:"api_key"`
	FFmpegPath  string `json:"ffmpeg_path" yaml:"ffmpeg_path"`
	FFprobePath string `json:"ffprobe_path" yaml:"ffprobe_path"`
	ScratchDir  string `json:"scratch_dir" yaml:"scratch_dir"`
	Ephemeral   bool   `json:"ephemeral" yaml:"ephemeral"`
	Token       string `json:"token" yaml:"token"`
}

type TranscodeJob struct {
	ID          uuid.UUID `json:"id"`
	MediaItemID uuid.UUID `json:"media_item_id"`
}

type HeartbeatResponse struct {
	Threads int           `json:"threads"`
	HWAccel string        `json:"hwaccel"`
	Job     *TranscodeJob `json:"job"`
}

type ProgressRequest struct {
	Progress float64 `json:"progress"`
	Status   string  `json:"status"` // "processing" or "failed"
	ErrorMsg string  `json:"error_msg,omitempty"`
}

var (
	config     WorkerConfig
	mu         sync.Mutex
	activeJobs = make(map[uuid.UUID]context.CancelFunc)
)

func parseYAMLConfig(data []byte, cfg *WorkerConfig) error {
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if idx := strings.Index(line, "#"); idx != -1 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if len(val) >= 2 {
			if (strings.HasPrefix(val, "\"") && strings.HasSuffix(val, "\"")) ||
				(strings.HasPrefix(val, "'") && strings.HasSuffix(val, "'")) {
				val = val[1 : len(val)-1]
			}
		}
		switch key {
		case "server_url":
			cfg.ServerURL = val
		case "api_key":
			cfg.APIKey = val
		case "ffmpeg_path":
			cfg.FFmpegPath = val
		case "ffprobe_path":
			cfg.FFprobePath = val
		case "scratch_dir":
			cfg.ScratchDir = val
		case "ephemeral":
			cfg.Ephemeral = (val == "true" || val == "1" || val == "yes")
		case "token":
			cfg.Token = val
		}
	}
	return nil
}

func LoadConfig(args []string, getenv func(string) string, readFile func(string) ([]byte, error)) (WorkerConfig, error) {
	fs := flag.NewFlagSet("prism-worker", flag.ContinueOnError)

	configFlag := fs.String("config", "worker_config.yaml", "Path to worker configuration YAML file")
	ephemeralFlag := fs.Bool("ephemeral", false, "Run worker in ephemeral mode")
	tokenFlag := fs.String("token", "", "Ephemeral registration token")
	serverFlag := fs.String("server", "", "Prism server URL (e.g. http://localhost:8080)")
	serverURLFlag := fs.String("server-url", "", "Prism server URL (alternative)")
	apiKeyFlag := fs.String("api-key", "", "API key for worker authentication")
	ffmpegPathFlag := fs.String("ffmpeg-path", "", "Path to ffmpeg executable")
	ffprobePathFlag := fs.String("ffprobe-path", "", "Path to ffprobe executable")
	scratchDirFlag := fs.String("scratch-dir", "", "Scratch directory path")

	if err := fs.Parse(args); err != nil {
		return WorkerConfig{}, err
	}

	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) {
		setFlags[f.Name] = true
	})

	var cfg WorkerConfig

	// 1. Determine config file path
	configPath := "worker_config.yaml"
	if envVal := getenv("PRISM_CONFIG"); envVal != "" {
		configPath = envVal
	}
	if setFlags["config"] {
		configPath = *configFlag
	}

	// 2. Load YAML file if exists
	data, err := readFile(configPath)
	if err == nil {
		if err := parseYAMLConfig(data, &cfg); err != nil {
			return WorkerConfig{}, fmt.Errorf("parsing yaml config: %w", err)
		}
	} else {
		// If explicitly requested config path, return error. Otherwise ignore missing config file
		configPathExplicit := setFlags["config"] || getenv("PRISM_CONFIG") != ""
		if configPathExplicit {
			return WorkerConfig{}, fmt.Errorf("reading config file %q: %w", configPath, err)
		}
	}

	// 3. Environment Variables
	if envVal := getenv("PRISM_EPHEMERAL"); envVal != "" {
		cfg.Ephemeral = (envVal == "true" || envVal == "1" || envVal == "yes")
	}
	if envVal := getenv("PRISM_TOKEN"); envVal != "" {
		cfg.Token = envVal
	}
	if envVal := getenv("PRISM_SERVER_URL"); envVal != "" {
		cfg.ServerURL = envVal
	}
	if envVal := getenv("PRISM_API_KEY"); envVal != "" {
		cfg.APIKey = envVal
	}
	if envVal := getenv("PRISM_FFMPEG_PATH"); envVal != "" {
		cfg.FFmpegPath = envVal
	}
	if envVal := getenv("PRISM_FFPROBE_PATH"); envVal != "" {
		cfg.FFprobePath = envVal
	}
	if envVal := getenv("PRISM_SCRATCH_DIR"); envVal != "" {
		cfg.ScratchDir = envVal
	}

	// 4. Command Line Flags
	if setFlags["ephemeral"] {
		cfg.Ephemeral = *ephemeralFlag
	}
	if setFlags["token"] {
		cfg.Token = *tokenFlag
	}
	if setFlags["server"] {
		cfg.ServerURL = *serverFlag
	}
	if setFlags["server-url"] {
		cfg.ServerURL = *serverURLFlag
	}
	if setFlags["api-key"] {
		cfg.APIKey = *apiKeyFlag
	}
	if setFlags["ffmpeg-path"] {
		cfg.FFmpegPath = *ffmpegPathFlag
	}
	if setFlags["ffprobe-path"] {
		cfg.FFprobePath = *ffprobePathFlag
	}
	if setFlags["scratch-dir"] {
		cfg.ScratchDir = *scratchDirFlag
	}

	// Set defaults
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	if cfg.FFprobePath == "" {
		cfg.FFprobePath = "ffprobe"
	}

	return cfg, nil
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	var err error
	config, err = LoadConfig(os.Args[1:], os.Getenv, os.ReadFile)
	if err != nil {
		slog.Error("failed to load configuration", "error", err)
		os.Exit(1)
	}

	if config.Ephemeral {
		if config.Token == "" {
			slog.Error("ephemeral mode requires a --token")
			os.Exit(1)
		}
		if config.ServerURL == "" {
			slog.Error("ephemeral mode requires a --server URL")
			os.Exit(1)
		}
		if !strings.HasPrefix(config.ServerURL, "http://") && !strings.HasPrefix(config.ServerURL, "https://") {
			slog.Error("invalid server URL: must start with http:// or https://")
			os.Exit(1)
		}

		name, err := os.Hostname()
		if err != nil {
			name = "ephemeral-worker-" + uuid.New().String()[:8]
		}

		slog.Info("Registering ephemeral worker", "server", config.ServerURL, "name", name)
		apiKey, err := registerEphemeral(config.ServerURL, config.Token, name)
		if err != nil {
			slog.Error("failed to register ephemeral worker", "error", err)
			os.Exit(1)
		}

		config.APIKey = apiKey
	} else {
		if config.ServerURL == "" || config.APIKey == "" {
			slog.Error("invalid configuration: server_url and api_key are required")
			os.Exit(1)
		}
	}

	config.ServerURL = strings.TrimSuffix(config.ServerURL, "/")

	if config.FFmpegPath == "" {
		config.FFmpegPath = "ffmpeg"
	}
	if config.FFprobePath == "" {
		config.FFprobePath = "ffprobe"
	}

	if config.ScratchDir != "" {
		if err := os.MkdirAll(config.ScratchDir, 0755); err != nil {
			slog.Error("failed to create scratch directory", "path", config.ScratchDir, "error", err)
			os.Exit(1)
		}
		slog.Info("Cleaning scratch directory on startup", "path", config.ScratchDir)
		if err := cleanScratchDir(config.ScratchDir); err != nil {
			slog.Warn("failed to clean scratch directory on startup", "path", config.ScratchDir, "error", err)
		}
	}

	slog.Info("Starting Prism Transcoder Worker", "server", config.ServerURL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		slog.Info("Shutting down gracefully...", "signal", sig)
		cancel()
		
		// Cancel all active transcode contexts
		mu.Lock()
		for _, cancelFunc := range activeJobs {
			cancelFunc()
		}
		mu.Unlock()

		os.Exit(0)
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Initial poll
	poll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll(ctx)
		}
	}
}

func poll(ctx context.Context) {
	// Heartbeat request
	url := fmt.Sprintf("%s/api/v1/worker/heartbeat", config.ServerURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		slog.Error("failed to create heartbeat request", "error", err)
		return
	}
	req.Header.Set("X-Worker-API-Key", config.APIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("heartbeat failed", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		slog.Error("Unauthorized: Invalid API Key")
		os.Exit(1)
	}

	if resp.StatusCode != http.StatusOK {
		slog.Error("heartbeat returned non-OK status", "status", resp.Status)
		return
	}

	var hr HeartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&hr); err != nil {
		slog.Error("failed to decode heartbeat response", "error", err)
		return
	}

	if hr.Job != nil {
		slog.Info("Claimed transcode job", "job_id", hr.Job.ID, "media_item_id", hr.Job.MediaItemID, "hwaccel", hr.HWAccel)

		mu.Lock()
		jobCtx, jobCancel := context.WithCancel(ctx)
		activeJobs[hr.Job.ID] = jobCancel
		mu.Unlock()

		go func() {
			defer func() {
				mu.Lock()
				delete(activeJobs, hr.Job.ID)
				mu.Unlock()
				jobCancel()
			}()
			
			err := executeJob(jobCtx, hr.Job, hr.HWAccel)
			if err != nil {
				slog.Error("Job execution failed", "job_id", hr.Job.ID, "error", err)
				reportFailure(ctx, hr.Job.ID, err.Error())
			} else {
				slog.Info("Job execution succeeded", "job_id", hr.Job.ID)
			}
		}()
	}
}

func executeJob(ctx context.Context, job *TranscodeJob, hwaccel string) error {
	// 1. Create temporary directory
	tempDir, err := os.MkdirTemp(config.ScratchDir, fmt.Sprintf("prism-job-%s-", job.ID))
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	sourcePath := filepath.Join(tempDir, "source.tmp")

	// 2. Download media
	slog.Info("Downloading source media file", "job_id", job.ID, "media_item_id", job.MediaItemID)
	if err := downloadFile(ctx, job.MediaItemID, sourcePath); err != nil {
		return fmt.Errorf("downloading source file: %w", err)
	}

	// 3. Probe file
	probe, err := ffmpeg.Probe(ctx, config.FFprobePath, sourcePath)
	if err != nil {
		return fmt.Errorf("probing source file: %w", err)
	}

	// 4. Configure profiles
	profiles := ffmpeg.DefaultProfiles()
	if probe.Height > 0 && probe.Width > 0 {
		var filtered []ffmpeg.RenditionProfile
		for _, prof := range profiles {
			if prof.Height <= probe.Height || (prof.Width > 0 && probe.Width >= prof.Width) {
				filtered = append(filtered, prof)
			}
		}
		if len(filtered) > 0 {
			profiles = filtered
		}
	}

	outputDir := filepath.Join(tempDir, "output")
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	// 5. Setup progress reporting rate limiter
	var lastReport time.Time
	var lastPct float64
	progressFn := func(pct float64) {
		now := time.Now()
		if now.Sub(lastReport) > 2*time.Second || pct - lastPct > 1.0 || pct >= 99.0 {
			lastReport = now
			lastPct = pct
			slog.Info("Transcode progress", "job_id", job.ID, "pct", fmt.Sprintf("%.1f%%", pct))
			reportProgress(ctx, job.ID, pct)
		}
	}

	// 6. Transcode
	slog.Info("Starting transcode process", "job_id", job.ID)
	opts := ffmpeg.TranscodeOptions{
		InputPath:       sourcePath,
		OutputDir:       outputDir,
		Profiles:        profiles,
		Duration:        probe.Duration,
		SourceWidth:     probe.Width,
		SourceHeight:    probe.Height,
		SubtitleStreams: probe.SubtitleStreams,
		ProgressFn:      progressFn,
		HWAccelType:     hwaccel,
	}

	if err := ffmpeg.TranscodeDASH(ctx, config.FFmpegPath, opts); err != nil {
		return fmt.Errorf("transcode process: %w", err)
	}

	// 7. Generate DASH MPD
	slog.Info("Generating MPD manifest", "job_id", job.ID)
	mpdPath := filepath.Join(outputDir, "manifest.mpd")
	renditions := make([]dash.RenditionInfo, len(profiles))
	for i, prof := range profiles {
		renditions[i] = dash.RenditionInfo{
			Name:          prof.Name,
			Height:        prof.Height,
			VideoBitrateK: prof.VideoBitrateK,
			AudioBitrateK: prof.AudioBitrateK,
		}
	}

	var subs []dash.SubtitleInfo
	for _, s := range probe.SubtitleStreams {
		lang := s.Language
		if lang == "" {
			lang = fmt.Sprintf("%d", s.Index)
		}
		vttPath := filepath.Join(outputDir, "sub_"+lang+".vtt")
		subs = append(subs, dash.SubtitleInfo{Language: lang, VTTPath: vttPath})
	}

	if err := dash.GenerateMPD(outputDir, mpdPath, renditions, subs, probe.Duration); err != nil {
		return fmt.Errorf("generating MPD manifest: %w", err)
	}

	// 8. Zip output directory
	slog.Info("Zipping transcode output bundle", "job_id", job.ID)
	zipPath := filepath.Join(tempDir, "bundle.zip")
	if err := zipDir(outputDir, zipPath); err != nil {
		return fmt.Errorf("zipping outputs: %w", err)
	}

	// 9. Upload ZIP
	slog.Info("Uploading transcode output bundle to server", "job_id", job.ID)
	if err := uploadBundle(ctx, job.ID, zipPath); err != nil {
		return fmt.Errorf("uploading bundle: %w", err)
	}

	slog.Info("Transcode job completed successfully", "job_id", job.ID)
	return nil
}

func downloadFile(ctx context.Context, mediaID uuid.UUID, destPath string) error {
	url := fmt.Sprintf("%s/api/v1/worker/media/%s/download", config.ServerURL, mediaID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Worker-API-Key", config.APIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned status %s", resp.Status)
	}

	out, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	_, err = io.Copy(out, resp.Body)
	return err
}

func reportProgress(ctx context.Context, jobID uuid.UUID, progress float64) {
	url := fmt.Sprintf("%s/api/v1/worker/jobs/%s/progress", config.ServerURL, jobID)
	payload := ProgressRequest{
		Progress: progress,
		Status:   "processing",
	}

	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("X-Worker-API-Key", config.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

func reportFailure(ctx context.Context, jobID uuid.UUID, errMsg string) {
	url := fmt.Sprintf("%s/api/v1/worker/jobs/%s/progress", config.ServerURL, jobID)
	payload := ProgressRequest{
		Progress: 0,
		Status:   "failed",
		ErrorMsg: errMsg,
	}

	data, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("X-Worker-API-Key", config.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

func uploadBundle(ctx context.Context, jobID uuid.UUID, zipPath string) error {
	url := fmt.Sprintf("%s/api/v1/worker/jobs/%s/bundle", config.ServerURL, jobID)

	file, err := os.Open(zipPath)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)

	go func() {
		err := func() error {
			defer func() { _ = writer.Close() }()
			part, err := writer.CreateFormFile("bundle", "bundle.zip")
			if err != nil {
				return err
			}
			_, err = io.Copy(part, file)
			return err
		}()
		if err != nil {
			pw.CloseWithError(err)
		} else {
			_ = pw.Close()
		}
	}()

	req, err := http.NewRequestWithContext(ctx, "POST", url, pr)
	if err != nil {
		return err
	}
	req.Header.Set("X-Worker-API-Key", config.APIKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned status %s: %s", resp.Status, string(bodyBytes))
	}

	return nil
}

func zipDir(src string, dest string) error {
	zipfile, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() { _ = zipfile.Close() }()

	archive := zip.NewWriter(zipfile)
	defer func() { _ = archive.Close() }()

	err = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		header.Name = relPath
		header.Method = zip.Deflate

		writer, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		_, err = io.Copy(writer, file)
		return err
	})

	return err
}

func cleanScratchDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			slog.Warn("failed to clean scratch path", "path", path, "error", err)
		}
	}
	return nil
}

type RegisterPayload struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

type RegisterResponse struct {
	APIKey string `json:"api_key"`
}

func registerEphemeral(serverURL, token, name string) (string, error) {
	payload := RegisterPayload{
		Name:  name,
		Token: token,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s/api/v1/worker/register", strings.TrimSuffix(serverURL, "/"))
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("server returned status %s: %s", resp.Status, string(bodyBytes))
	}

	var res RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}

	return res.APIKey, nil
}
