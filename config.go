package whisper

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The values New applies when Config leaves a field at its zero value.
const (
	// DefaultLanguage is the spoken language assumed when Config.Language is
	// empty: Brazilian Portuguese.
	DefaultLanguage = "pt"
	// DefaultTimeout bounds one Transcribe call, conversion included.
	DefaultTimeout = 3 * time.Minute
	// DefaultMaxDuration is the longest audio Transcribe accepts.
	DefaultMaxDuration = 5 * time.Minute
	// DefaultMaxBytes is the largest input Transcribe accepts, in bytes.
	DefaultMaxBytes int64 = 32 << 20
)

var languagePattern = regexp.MustCompile(`^(auto|[a-z]{2,3})$`)

// Config is what the application passes when it builds the client. Every path
// is checked by New, so a wrong deployment fails at boot and not on the first
// voice note. This package never reads the environment: the application reads
// it and passes the values here.
type Config struct {
	// BinaryPath is the whisper.cpp command-line program ("whisper-cli"): an
	// absolute path, a relative one, or a bare name looked up in PATH.
	// Required, unless ServerURL is set.
	BinaryPath string
	// ModelPath is a ggml model file, for example "ggml-small.bin". Required,
	// unless ServerURL is set (the server holds the model).
	ModelPath string
	// ServerURL, when set, sends the converted audio to a resident
	// whisper.cpp "whisper-server" (its /inference endpoint) instead of
	// starting whisper-cli for every call. The server loads the model once,
	// which is what makes a GPU worth having: whisper-cli would load the model
	// again on every call. The conversion, the limits and the cleanup stay
	// here; BinaryPath and ModelPath are then ignored. An http or https URL,
	// such as "http://127.0.0.1:8080".
	ServerURL string
	// FFmpegPath is the ffmpeg program used to convert the audio to WAV. Empty
	// means "ffmpeg" in PATH.
	FFmpegPath string
	// FFprobePath is the ffprobe program used to read the duration before
	// converting. Empty means the one that sits beside FFmpegPath, then
	// "ffprobe" in PATH.
	FFprobePath string
	// Language is the spoken language as a two or three letter code ("pt",
	// "en"), or "auto" to let whisper detect it. Empty means DefaultLanguage.
	Language string
	// Prompt is optional text whisper reads before the audio, to bias its
	// spelling toward a vocabulary (product names, technical terms). It is
	// passed as one argument, never through a shell.
	Prompt string
	// Threads is how many CPU threads whisper uses. Zero leaves whisper's own
	// default, which is the smaller of 4 and the number of cores.
	Threads int
	// Timeout bounds one Transcribe call: probe, conversion and whisper
	// together. Zero means DefaultTimeout.
	Timeout time.Duration
	// MaxDuration is the longest audio accepted; longer audio is refused with
	// ErrTooLong before whisper runs. Zero means DefaultMaxDuration.
	MaxDuration time.Duration
	// MaxBytes is the largest input accepted; larger input is refused with
	// ErrTooLarge before anything is written to disk. Zero means
	// DefaultMaxBytes.
	MaxBytes int64
	// TempDir is the directory under which each call makes its own private
	// working directory, removed when the call returns. Empty means the
	// system's temporary directory.
	TempDir string
}

// Client runs whisper.cpp on the audio it is given. It is safe for concurrent
// use; every call works in its own directory. Transcription is CPU bound, so
// the application decides how many calls may run at once.
type Client struct {
	binary      string
	model       string
	server      string
	ffmpeg      string
	ffprobe     string
	language    string
	prompt      string
	threads     int
	timeout     time.Duration
	maxDuration time.Duration
	maxBytes    int64
	tempDir     string
}

var _ Transcriber = (*Client)(nil)

// New validates cfg and returns a client. It refuses a program or model that
// is missing or is not a regular file, a program that is not executable, an
// empty model, an unrecognized language, a negative number, and a TempDir
// that is not a directory. Nothing is started here: the programs are only
// looked at, not run.
func New(cfg Config) (*Client, error) {
	var binary, model, server string
	var err error
	if strings.TrimSpace(cfg.ServerURL) != "" {
		server, err = resolveServerURL(cfg.ServerURL)
		if err != nil {
			return nil, fmt.Errorf("whisper: ServerURL: %w", err)
		}
	} else {
		binary, err = resolveProgram(cfg.BinaryPath)
		if err != nil {
			return nil, fmt.Errorf("whisper: BinaryPath: %w", err)
		}
		model, err = resolveFile(cfg.ModelPath)
		if err != nil {
			return nil, fmt.Errorf("whisper: ModelPath: %w", err)
		}
	}

	ffmpegName := cfg.FFmpegPath
	if ffmpegName == "" {
		ffmpegName = "ffmpeg"
	}
	ffmpeg, err := resolveProgram(ffmpegName)
	if err != nil {
		return nil, fmt.Errorf("whisper: FFmpegPath: %w", err)
	}

	ffprobe, err := resolveFFprobe(cfg.FFprobePath, ffmpeg)
	if err != nil {
		return nil, fmt.Errorf("whisper: FFprobePath: %w", err)
	}

	language := strings.ToLower(strings.TrimSpace(cfg.Language))
	if language == "" {
		language = DefaultLanguage
	}
	if !languagePattern.MatchString(language) {
		return nil, fmt.Errorf("whisper: Language %q is neither \"auto\" nor a two or three letter code", cfg.Language)
	}

	if cfg.Threads < 0 {
		return nil, errors.New("whisper: Threads must not be negative")
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("whisper: Timeout must not be negative")
	}
	if cfg.MaxDuration < 0 {
		return nil, errors.New("whisper: MaxDuration must not be negative")
	}
	if cfg.MaxBytes < 0 {
		return nil, errors.New("whisper: MaxBytes must not be negative")
	}

	tempDir := cfg.TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	tempDir, err = filepath.Abs(tempDir)
	if err != nil {
		return nil, fmt.Errorf("whisper: TempDir: %w", err)
	}
	info, err := os.Stat(tempDir)
	if err != nil {
		return nil, fmt.Errorf("whisper: TempDir: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("whisper: TempDir %q is not a directory", tempDir)
	}

	c := &Client{
		binary:      binary,
		model:       model,
		server:      server,
		ffmpeg:      ffmpeg,
		ffprobe:     ffprobe,
		language:    language,
		prompt:      cfg.Prompt,
		threads:     cfg.Threads,
		timeout:     cfg.Timeout,
		maxDuration: cfg.MaxDuration,
		maxBytes:    cfg.MaxBytes,
		tempDir:     tempDir,
	}
	if c.timeout == 0 {
		c.timeout = DefaultTimeout
	}
	if c.maxDuration == 0 {
		c.maxDuration = DefaultMaxDuration
	}
	if c.maxBytes == 0 {
		c.maxBytes = DefaultMaxBytes
	}
	return c, nil
}

// resolveServerURL checks the address of a whisper-server: an http or https
// URL with a host, and nothing that could carry a credential or a query.
func resolveServerURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("is not a URL")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("must be an http or https URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("must not carry credentials, a query or a fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// resolveProgram turns a path or a bare name into the absolute path of an
// executable regular file.
func resolveProgram(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("is required")
	}
	if !strings.ContainsRune(name, filepath.Separator) {
		found, err := exec.LookPath(name)
		if err != nil {
			return "", fmt.Errorf("%q was not found in PATH", name)
		}
		name = found
	}
	path, err := resolveFile(name)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%q is not executable", path)
	}
	return path, nil
}

// resolveFile returns the absolute path of a non-empty regular file.
func resolveFile(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("is required")
	}
	path, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%q cannot be read: %w", path, errors.Unwrap(err))
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%q is not a regular file", path)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("%q is empty", path)
	}
	return path, nil
}

// resolveFFprobe finds ffprobe: the configured path, else the program beside
// ffmpeg, else PATH.
func resolveFFprobe(configured, ffmpeg string) (string, error) {
	if configured != "" {
		return resolveProgram(configured)
	}
	if beside, err := resolveProgram(filepath.Join(filepath.Dir(ffmpeg), "ffprobe")); err == nil {
		return beside, nil
	}
	return resolveProgram("ffprobe")
}
