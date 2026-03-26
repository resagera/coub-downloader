package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "1.5"

type Config struct {
	Dir      string
	CacheDir string

	Force   bool
	Debug   bool
	Follow  bool
	Timeout time.Duration
	Workers int

	ModeAudio   bool
	ModeShare   bool
	ModeVideo   bool
	ModePicture bool
	ModeAll     bool
	ModeCompare bool
}

type CoubResponse struct {
	Permalink    string `json:"permalink"`
	Title        string `json:"title"`
	Picture      string `json:"picture"`
	FileVersions struct {
		Share struct {
			Default string `json:"default"`
		} `json:"share"`
	} `json:"file_versions"`
}

type mediaCandidate struct {
	URL       string
	Score     int
	Path      string
	SizeBytes int64
}

type FollowState struct {
	mu         sync.Mutex
	inProgress map[string]struct{}
}

type DownloadSummary struct {
	Permalink string
	Title     string
	JSONPath  string

	AudioURL  string
	AudioPath string

	ShareURL  string
	SharePath string

	VideoURL  string
	VideoPath string

	PictureURL  string
	PicturePath string

	ComparePath string
}

func main() {
	cfg := parseFlags()
	fmt.Println("Starting with version", version)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		exitErr(fmt.Errorf("create cache dir: %w", err))
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		exitErr(fmt.Errorf("create target dir: %w", err))
	}

	if cfg.ModeCompare {
		if err := ensureFFmpegAvailable(); err != nil {
			exitErr(err)
		}
	}

	if cfg.Follow {
		if err := runFollowMode(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
			exitErr(err)
		}
		return
	}

	args := flag.Args()
	if len(args) < 1 {
		printUsage()
		os.Exit(1)
	}

	summary, err := processOne(ctx, cfg, args[0])
	if err != nil {
		exitErr(err)
	}
	printSummary(summary)
}

func parseFlags() Config {
	var cfg Config

	flag.StringVar(&cfg.Dir, "dir", "music", "directory for downloaded files")
	flag.StringVar(&cfg.CacheDir, "cache", "cache", "directory for cached API json")

	flag.BoolVar(&cfg.Force, "force", false, "force refresh API cache and re-download files")
	flag.BoolVar(&cfg.Debug, "debug", false, "enable debug logs")
	flag.BoolVar(&cfg.Follow, "follow", false, "read URLs line-by-line from stdin and download concurrently")
	flag.DurationVar(&cfg.Timeout, "timeout", 30*time.Second, "HTTP request timeout")
	flag.IntVar(&cfg.Workers, "workers", 3, "max concurrent downloads in --follow mode")

	flag.BoolVar(&cfg.ModeAudio, "audio", false, "download best audio")
	flag.BoolVar(&cfg.ModeShare, "share", false, "download share video from file_versions.share.default")
	flag.BoolVar(&cfg.ModeVideo, "video", false, "download best video")
	flag.BoolVar(&cfg.ModePicture, "picture", false, "download picture")
	flag.BoolVar(&cfg.ModeAll, "all", false, "download audio + share + video + picture")
	flag.BoolVar(&cfg.ModeCompare, "compare", false, "merge video + audio with ffmpeg and try to add picture as cover art")

	flag.Parse()

	if cfg.Workers < 1 {
		cfg.Workers = 1
	}

	if cfg.ModeAll {
		cfg.ModeAudio = true
		cfg.ModeShare = true
		cfg.ModeVideo = true
		cfg.ModePicture = true
	}

	if cfg.ModeCompare {
		cfg.ModeAudio = true
		cfg.ModeVideo = true
		cfg.ModePicture = true
	}

	if !cfg.ModeAudio && !cfg.ModeShare && !cfg.ModeVideo && !cfg.ModePicture && !cfg.ModeCompare {
		cfg.ModeAudio = true
	}

	return cfg
}

func printUsage() {
	name := filepath.Base(os.Args[0])
	fmt.Fprintf(os.Stderr, "usage:\n")
	fmt.Fprintf(os.Stderr, "  %s [flags] <coub_url>\n", name)
	fmt.Fprintf(os.Stderr, "  %s --follow [flags]\n", name)
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "download modes:\n")
	fmt.Fprintf(os.Stderr, "  --audio      download best audio\n")
	fmt.Fprintf(os.Stderr, "  --share      download share video (file_versions.share.default)\n")
	fmt.Fprintf(os.Stderr, "  --video      download best video\n")
	fmt.Fprintf(os.Stderr, "  --picture    download picture\n")
	fmt.Fprintf(os.Stderr, "  --all        same as: --audio --share --video --picture\n")
	fmt.Fprintf(os.Stderr, "  --compare    download audio + video + picture and create merged mp4 with ffmpeg\n")
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "examples:\n")
	fmt.Fprintf(os.Stderr, "  %s \"https://coub.com/view/3d19t8\"\n", name)
	fmt.Fprintf(os.Stderr, "  %s --video --picture --dir media \"https://coub.com/view/3d19t8\"\n", name)
	fmt.Fprintf(os.Stderr, "  %s --all --dir media \"https://coub.com/view/3d19t8\"\n", name)
	fmt.Fprintf(os.Stderr, "  %s --compare --dir media \"https://coub.com/view/3d19t8\"\n", name)
	fmt.Fprintf(os.Stderr, "  %s --follow --compare --workers 5\n", name)
}

func runFollowMode(ctx context.Context, cfg Config) error {
	type job struct {
		rawURL    string
		permalink string
	}

	jobs := make(chan job, cfg.Workers*4)
	state := &FollowState{
		inProgress: make(map[string]struct{}),
	}

	var wg sync.WaitGroup

	for i := 0; i < cfg.Workers; i++ {
		wg.Add(1)

		go func(workerID int) {
			defer wg.Done()

			for {
				select {
				case <-ctx.Done():
					return
				case j, ok := <-jobs:
					if !ok {
						return
					}

					logInfo("worker-%d: start %s", workerID+1, j.rawURL)

					summary, err := processOne(ctx, cfg, j.rawURL)
					if err != nil {
						logError("worker-%d: %s -> %v", workerID+1, j.rawURL, err)
					} else {
						printSummary(summary)
						logInfo("worker-%d: done %s", workerID+1, j.rawURL)
					}

					state.mu.Lock()
					delete(state.inProgress, j.permalink)
					state.mu.Unlock()
				}
			}
		}(i)
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ctx.Err()
		default:
		}

		if !scanner.Scan() {
			break
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			logWarning("empty input")
			continue
		}

		permalink, err := extractPermalink(line)
		if err != nil {
			logWarning("bad url: %s | err=%v", line, err)
			continue
		}

		state.mu.Lock()
		_, exists := state.inProgress[permalink]
		if !exists {
			state.inProgress[permalink] = struct{}{}
		}
		state.mu.Unlock()

		if exists {
			logWarning("skip duplicate in progress: %s", line)
			continue
		}

		select {
		case <-ctx.Done():
			state.mu.Lock()
			delete(state.inProgress, permalink)
			state.mu.Unlock()
			close(jobs)
			wg.Wait()
			return ctx.Err()
		case jobs <- job{rawURL: line, permalink: permalink}:
		}
	}

	close(jobs)
	wg.Wait()

	if err := scanner.Err(); err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("stdin scan: %w", err)
	}

	return ctx.Err()
}

func processOne(ctx context.Context, cfg Config, rawURL string) (*DownloadSummary, error) {
	permalink, err := extractPermalink(rawURL)
	if err != nil {
		return nil, fmt.Errorf("extract permalink: %w", err)
	}

	cachePath := filepath.Join(cfg.CacheDir, permalink+".json")

	rawJSON, err := loadOrFetchCoubJSON(ctx, cfg, permalink, cachePath)
	if err != nil {
		return nil, err
	}

	var meta CoubResponse
	if err := json.Unmarshal(rawJSON, &meta); err != nil {
		return nil, fmt.Errorf("parse coub json: %w", err)
	}

	if meta.Permalink == "" {
		return nil, errors.New("api response does not contain permalink")
	}
	if meta.Permalink != permalink {
		return nil, fmt.Errorf("api returned different permalink: requested=%q got=%q", permalink, meta.Permalink)
	}

	baseName := sanitizeFilename(meta.Title)
	if baseName == "" {
		baseName = permalink
	}
	baseName = baseName + "_" + permalink

	s := &DownloadSummary{
		Permalink: permalink,
		Title:     meta.Title,
		JSONPath:  cachePath,
	}

	if cfg.ModeAudio {
		audioURL, err := selectBestAudioURL(rawJSON, cfg.Debug)
		if err != nil {
			return nil, fmt.Errorf("select audio: %w", err)
		}
		audioPath := filepath.Join(cfg.Dir, baseName+".mp3")
		s.AudioURL = audioURL
		s.AudioPath = audioPath

		if shouldSkipExisting(cfg, audioPath) {
			logWarning("already exists: %s", audioPath)
		} else {
			if err := downloadFile(ctx, cfg, audioURL, audioPath); err != nil {
				return nil, fmt.Errorf("download audio: %w", err)
			}
			logOK("%s", audioPath)
		}
	}

	if cfg.ModeShare {
		shareURL := strings.TrimSpace(meta.FileVersions.Share.Default)
		if shareURL == "" {
			return nil, errors.New("share video url not found in file_versions.share.default")
		}
		ext := extensionFromURL(shareURL, ".mp4")
		sharePath := filepath.Join(cfg.Dir, baseName+"_share"+ext)
		s.ShareURL = shareURL
		s.SharePath = sharePath

		if shouldSkipExisting(cfg, sharePath) {
			logWarning("already exists: %s", sharePath)
		} else {
			if err := downloadFile(ctx, cfg, shareURL, sharePath); err != nil {
				return nil, fmt.Errorf("download share video: %w", err)
			}
			logOK("%s", sharePath)
		}
	}

	if cfg.ModeVideo {
		videoURL, err := selectBestVideoURL(rawJSON, cfg.Debug)
		if err != nil {
			return nil, fmt.Errorf("select video: %w", err)
		}
		videoExt := extensionFromURL(videoURL, ".mp4")
		videoPath := filepath.Join(cfg.Dir, baseName+videoExt)
		s.VideoURL = videoURL
		s.VideoPath = videoPath

		if shouldSkipExisting(cfg, videoPath) {
			logWarning("already exists: %s", videoPath)
		} else {
			if err := downloadFile(ctx, cfg, videoURL, videoPath); err != nil {
				return nil, fmt.Errorf("download video: %w", err)
			}
			logOK("%s", videoPath)
		}
	}

	if cfg.ModePicture {
		pictureURL := strings.TrimSpace(meta.Picture)
		if pictureURL == "" {
			return nil, errors.New("picture url not found")
		}
		pictureExt := extensionFromURL(pictureURL, ".jpg")
		picturePath := filepath.Join(cfg.Dir, baseName+pictureExt)
		s.PictureURL = pictureURL
		s.PicturePath = picturePath

		if shouldSkipExisting(cfg, picturePath) {
			logWarning("already exists: %s", picturePath)
		} else {
			if err := downloadFile(ctx, cfg, pictureURL, picturePath); err != nil {
				return nil, fmt.Errorf("download picture: %w", err)
			}
			logOK("%s", picturePath)
		}
	}

	if cfg.ModeCompare {
		comparePath, err := runFFmpegCompare(ctx, cfg, s)
		if err != nil {
			return nil, err
		}
		s.ComparePath = comparePath
	}

	return s, nil
}

func runFFmpegCompare(ctx context.Context, cfg Config, s *DownloadSummary) (string, error) {
	if s.VideoPath == "" || s.AudioPath == "" || s.PicturePath == "" {
		return "", errors.New("compare requires downloaded video, audio and picture")
	}

	base := strings.TrimSuffix(filepath.Base(s.VideoPath), filepath.Ext(s.VideoPath))
	outPath := filepath.Join(cfg.Dir, base+"_compare.mp4")
	tmpMux := filepath.Join(cfg.Dir, base+"_compare.tmp.mp4")

	if shouldSkipExisting(cfg, outPath) {
		logWarning("already exists: %s", outPath)
		return outPath, nil
	}

	_ = os.Remove(tmpMux)
	_ = os.Remove(outPath)

	argsMux := []string{
		"-y",
		"-stream_loop", "-1",
		"-i", s.VideoPath,
		"-i", s.AudioPath,
		"-filter_complex", "[0:v]scale=trunc(iw/2)*2:trunc(ih/2)*2,format=yuv420p[v]",
		"-map", "[v]",
		"-map", "1:a:0",
		"-c:v", "libx264",
		"-preset", "medium",
		"-crf", "18",
		"-c:a", "aac",
		"-b:a", "192k",
		"-shortest",
		tmpMux,
	}

	if cfg.Debug {
		logInfo("ffmpeg mux: ffmpeg %s", strings.Join(quoteArgs(argsMux), " "))
	}

	if err := runCommand(ctx, "ffmpeg", argsMux...); err != nil {
		return "", fmt.Errorf("ffmpeg mux failed: %w", err)
	}

	// Попытка добавить picture как attached_pic.
	argsCover := []string{
		"-y",
		"-i", tmpMux,
		"-i", s.PicturePath,
		"-map", "0",
		"-map", "1",
		"-c", "copy",
		"-c:v:1", "mjpeg",
		"-disposition:v:1", "attached_pic",
		outPath,
	}

	if cfg.Debug {
		logInfo("ffmpeg cover: ffmpeg %s", strings.Join(quoteArgs(argsCover), " "))
	}

	if err := runCommand(ctx, "ffmpeg", argsCover...); err != nil {
		logWarning("cover art was not embedded, keeping plain merged video: %v", err)
		if err := os.Rename(tmpMux, outPath); err != nil {
			return "", fmt.Errorf("finalize merged file: %w", err)
		}
		logOK("%s", outPath)
		return outPath, nil
	}

	_ = os.Remove(tmpMux)
	logOK("%s", outPath)
	return outPath, nil
}

func runCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}

func ensureFFmpegAvailable() error {
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		return nil
	}

	return errors.New(
		"ffmpeg not found in PATH. install it first.\n" +
			"Ubuntu/Debian: sudo apt update && sudo apt install ffmpeg\n" +
			"macOS (Homebrew): brew install ffmpeg\n" +
			"Windows (winget): winget install Gyan.FFmpeg",
	)
}

func printSummary(s *DownloadSummary) {
	fmt.Println("done")
	fmt.Printf("permalink: %s\n", s.Permalink)
	fmt.Printf("title: %s\n", s.Title)

	if s.AudioURL != "" {
		fmt.Printf("audio: %s\n", s.AudioURL)
	}
	if s.VideoURL != "" {
		fmt.Printf("video: %s\n", s.VideoURL)
	}
	if s.ShareURL != "" {
		fmt.Printf("share: %s\n", s.ShareURL)
	}
	if s.PictureURL != "" {
		fmt.Printf("picture: %s\n", s.PictureURL)
	}

	fmt.Printf("json:  %s\n", s.JSONPath)

	if s.AudioPath != "" {
		fmt.Printf("mp3:   %s\n", s.AudioPath)
	}
	if s.VideoPath != "" {
		fmt.Printf("video_file:   %s\n", s.VideoPath)
	}
	if s.SharePath != "" {
		fmt.Printf("share_file:   %s\n", s.SharePath)
	}
	if s.PicturePath != "" {
		fmt.Printf("picture_file: %s\n", s.PicturePath)
	}
	if s.ComparePath != "" {
		fmt.Printf("compare_file: %s\n", s.ComparePath)
	}
}

func shouldSkipExisting(cfg Config, filePath string) bool {
	if cfg.Force {
		return false
	}
	st, err := os.Stat(filePath)
	return err == nil && st.Size() > 0
}

func extractPermalink(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}

	if u.Host == "" {
		return "", errors.New("empty host")
	}

	host := strings.ToLower(u.Host)
	if host != "coub.com" && host != "www.coub.com" {
		return "", fmt.Errorf("unsupported host: %s", u.Host)
	}

	p := strings.Trim(u.Path, "/")
	parts := strings.Split(p, "/")
	if len(parts) != 2 || parts[0] != "view" || parts[1] == "" {
		return "", errors.New("expected URL like https://coub.com/view/<permalink>")
	}

	return parts[1], nil
}

func loadOrFetchCoubJSON(ctx context.Context, cfg Config, permalink, cachePath string) ([]byte, error) {
	if !cfg.Force {
		if data, err := os.ReadFile(cachePath); err == nil && len(data) > 0 {
			debugf(cfg, "using cache: %s", cachePath)
			return data, nil
		}
	}

	client := &http.Client{
		Timeout: cfg.Timeout,
	}

	endpoints := []string{
		fmt.Sprintf("https://coub.com/api/v2/coubs/%s.json", permalink),
		fmt.Sprintf("https://coub.com/api/v2/coubs/%s", permalink),
	}

	var lastErr error
	for _, endpoint := range endpoints {
		debugf(cfg, "requesting api: %s", endpoint)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; coub-downloader/1.5)")
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("api status %d: %s", resp.StatusCode, truncate(string(body), 300))
			continue
		}

		if !json.Valid(body) {
			lastErr = errors.New("api returned non-json response")
			continue
		}

		if err := os.WriteFile(cachePath, body, 0o644); err != nil {
			return nil, fmt.Errorf("write cache: %w", err)
		}
		return body, nil
	}

	if lastErr == nil {
		lastErr = errors.New("unknown api error")
	}
	return nil, fmt.Errorf("fetch coub api: %w", lastErr)
}

func selectBestAudioURL(rawJSON []byte, debug bool) (string, error) {
	return selectBestMediaURL(rawJSON, "audio", debug)
}

func selectBestVideoURL(rawJSON []byte, debug bool) (string, error) {
	return selectBestMediaURL(rawJSON, "video", debug)
}

func selectBestMediaURL(rawJSON []byte, kind string, debug bool) (string, error) {
	var root any
	if err := json.Unmarshal(rawJSON, &root); err != nil {
		return "", err
	}

	candidates := collectMediaCandidates(root, nil, kind)
	if len(candidates) == 0 {
		return "", fmt.Errorf("no %s urls found in API response", kind)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		if candidates[i].SizeBytes != candidates[j].SizeBytes {
			return candidates[i].SizeBytes > candidates[j].SizeBytes
		}
		return candidates[i].Path < candidates[j].Path
	})

	seen := make(map[string]struct{})
	uniq := make([]mediaCandidate, 0, len(candidates))
	for _, c := range candidates {
		if _, ok := seen[c.URL]; ok {
			continue
		}
		seen[c.URL] = struct{}{}
		uniq = append(uniq, c)
	}
	candidates = uniq

	if debug {
		fmt.Printf("%s candidates:\n", kind)
		for _, c := range candidates {
			if c.SizeBytes > 0 {
				fmt.Printf("  score=%d size=%d path=%s url=%s\n", c.Score, c.SizeBytes, c.Path, c.URL)
			} else {
				fmt.Printf("  score=%d path=%s url=%s\n", c.Score, c.Path, c.URL)
			}
		}
	}

	return candidates[0].URL, nil
}

func collectMediaCandidates(v any, pathParts []string, kind string) []mediaCandidate {
	var out []mediaCandidate

	switch x := v.(type) {
	case map[string]any:
		if c, ok := buildMediaCandidate(x, pathParts, kind); ok {
			out = append(out, c)
		}

		for k, child := range x {
			out = append(out, collectMediaCandidates(child, append(pathParts, k), kind)...)
		}
	case []any:
		for i, child := range x {
			out = append(out, collectMediaCandidates(child, append(pathParts, fmt.Sprintf("[%d]", i)), kind)...)
		}
	}

	return out
}

func buildMediaCandidate(node map[string]any, pathParts []string, kind string) (mediaCandidate, bool) {
	urlVal, ok := node["url"]
	if !ok {
		return mediaCandidate{}, false
	}

	mediaURL, ok := urlVal.(string)
	if !ok || mediaURL == "" {
		return mediaCandidate{}, false
	}

	p := strings.Join(pathParts, ".")
	if !looksLikeMediaPath(p, mediaURL, kind) {
		return mediaCandidate{}, false
	}

	var sizeBytes int64
	switch v := node["size"].(type) {
	case float64:
		sizeBytes = int64(v)
	case json.Number:
		n, _ := v.Int64()
		sizeBytes = n
	}

	return mediaCandidate{
		URL:       mediaURL,
		Score:     scoreMediaPath(p, kind),
		Path:      p,
		SizeBytes: sizeBytes,
	}, true
}

func looksLikeMediaPath(pathStr, mediaURL, kind string) bool {
	lp := strings.ToLower(pathStr)
	lu := strings.ToLower(mediaURL)

	switch kind {
	case "audio":
		if strings.Contains(lp, "audio") {
			return true
		}
		if strings.HasSuffix(lu, ".mp3") || strings.Contains(lu, ".mp3?") {
			return true
		}
	case "video":
		if strings.Contains(lp, "video") {
			return true
		}
		if strings.HasSuffix(lu, ".mp4") || strings.Contains(lu, ".mp4?") {
			return true
		}
	}
	return false
}

func scoreMediaPath(pathStr, kind string) int {
	p := strings.ToLower(pathStr)
	score := 0

	switch kind {
	case "audio":
		if strings.Contains(p, "file_versions.html5.audio") {
			score += 1000
		}
		if strings.Contains(p, "audio") {
			score += 200
		}
	case "video":
		if strings.Contains(p, "file_versions.html5.video") {
			score += 1000
		}
		if strings.Contains(p, "video") {
			score += 200
		}
	}

	switch {
	case strings.Contains(p, ".high"):
		score += 100
	case strings.Contains(p, ".hq"):
		score += 95
	case strings.Contains(p, ".med"):
		score += 70
	case strings.Contains(p, ".medium"):
		score += 70
	case strings.Contains(p, ".big"):
		score += 65
	case strings.Contains(p, ".default"):
		score += 50
	case strings.Contains(p, ".small"):
		score += 30
	case strings.Contains(p, ".low"):
		score += 20
	}

	if strings.Contains(p, "mobile") {
		score -= 5
	}
	if kind == "video" && strings.Contains(p, "share") {
		score -= 20
	}

	return score
}

func downloadFile(ctx context.Context, cfg Config, srcURL, outPath string) error {
	client := &http.Client{
		Timeout: 0,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srcURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; coub-downloader/1.5)")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("download status %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	tmpPath := outPath + ".part"

	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()

	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}

	if err := os.Rename(tmpPath, outPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	debugf(cfg, "saved file: %s", outPath)
	return nil
}

func extensionFromURL(rawURL, fallback string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fallback
	}

	ext := strings.ToLower(path.Ext(u.Path))
	if ext == "" || len(ext) > 10 {
		return fallback
	}
	return ext
}

func sanitizeFilename(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	re := regexp.MustCompile(`[<>:"/\\|?*\x00-\x1F]`)
	s = re.ReplaceAllString(s, "_")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, ". ")

	if len(s) > 160 {
		s = s[:160]
		s = strings.TrimRight(s, ". ")
	}

	return s
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func quoteArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if strings.ContainsAny(a, " \t\"'") {
			out = append(out, fmt.Sprintf("%q", a))
		} else {
			out = append(out, a)
		}
	}
	return out
}

func debugf(cfg Config, format string, args ...any) {
	if cfg.Debug {
		fmt.Printf("[DEBUG] "+format+"\n", args...)
	}
}

func logInfo(format string, args ...any) {
	fmt.Printf("[INFO] "+format+"\n", args...)
}

func logOK(format string, args ...any) {
	fmt.Printf("[OK] "+format+"\n", args...)
}

func logWarning(format string, args ...any) {
	fmt.Printf("[WARNING] "+format+"\n", args...)
}

func logError(format string, args ...any) {
	fmt.Printf("[ERROR] "+format+"\n", args...)
}

func exitErr(err error) {
	fmt.Printf("[ERROR] %v\n", err)
	os.Exit(1)
}