package whisper

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// The values NewHandler applies when HandlerConfig leaves a field at zero.
const (
	// DefaultMaxConcurrent is how many transcriptions run at once. The
	// resident whisper-server answers one at a time, so a second call only
	// overlaps with the conversion of the first.
	DefaultMaxConcurrent = 2
	// DefaultQueueWait is how long a request waits for a free slot before it
	// is answered 429, so a caller with another node tries it at once.
	DefaultQueueWait = 2 * time.Second
	// MinTokenLength is the shortest token NewHandler accepts.
	MinTokenLength = 32
)

// Error codes of the wire contract. The body of every non-200 answer is
// {"error": code, "message": text}; Remote maps each code back to the error
// the local client would have returned.
const (
	codeEmptyAudio  = "empty_audio"
	codeTooLarge    = "too_large"
	codeTooLong     = "too_long"
	codeUnsupported = "unsupported_audio"
	codeTimeout     = "timeout"
	codeBusy        = "busy"
	codeFailed      = "failed"
	codeBadOutput   = "bad_output"
	codeUnauth      = "unauthorized"
	codeMethod      = "method_not_allowed"
	codeCanceled    = "canceled"
)

// wireSegment and wireTranscript are the JSON of a successful POST: the same
// fields as Transcript, with the times in milliseconds.
type wireSegment struct {
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Text    string `json:"text"`
}

type wireTranscript struct {
	Text       string        `json:"text"`
	Language   string        `json:"language"`
	DurationMS int64         `json:"duration_ms"`
	Segments   []wireSegment `json:"segments,omitempty"`
}

type wireError struct {
	Code    string `json:"error"`
	Message string `json:"message"`
}

// HandlerConfig is what NewHandler needs.
type HandlerConfig struct {
	// Transcriber does the work: a *Client in production, a *Fake in tests.
	// Required.
	Transcriber Transcriber
	// Tokens are the clients allowed to call, by name: the name is only for
	// the log, the token is what the caller sends as "Authorization: Bearer
	// <token>". At least one is required and each token has at least
	// MinTokenLength characters. A token is never logged.
	Tokens map[string]string
	// MaxConcurrent is how many calls transcribe at once; the rest wait up to
	// QueueWait. Zero means DefaultMaxConcurrent.
	MaxConcurrent int
	// QueueWait is how long a call waits for a slot before it is answered
	// 429 with Retry-After. Zero means DefaultQueueWait.
	QueueWait time.Duration
	// MaxBytes is the largest body accepted, checked before it is read.
	// Zero means DefaultMaxBytes.
	MaxBytes int64
	// Logger receives one line per call: the client's name, the status, the
	// size in bytes, the audio's duration and the time spent. Never the audio
	// or the text. Nil logs nothing.
	Logger *slog.Logger
}

// Handler serves a Transcriber over HTTP, for another application's Remote to
// call. It answers POST with the audio as the body (its Content-Type is the
// audio's MIME type) and the transcript as JSON, on whatever path it is
// mounted. It checks the token, limits the body and the concurrency, and
// writes nothing to disk itself: the Transcriber's private directory is the
// only place the audio touches, and it is removed before the call returns.
type Handler struct {
	transcriber Transcriber
	tokens      []namedToken
	slots       chan struct{}
	queueWait   time.Duration
	maxBytes    int64
	log         *slog.Logger
	inFlight    atomic.Int64
	served      atomic.Int64
	refused     atomic.Int64
}

type namedToken struct {
	name string
	sum  [sha256.Size]byte
}

var _ http.Handler = (*Handler)(nil)

// NewHandler validates cfg and returns the handler.
func NewHandler(cfg HandlerConfig) (*Handler, error) {
	if cfg.Transcriber == nil {
		return nil, errors.New("whisper: HandlerConfig.Transcriber is required")
	}
	if len(cfg.Tokens) == 0 {
		return nil, errors.New("whisper: HandlerConfig.Tokens needs at least one client")
	}
	if cfg.MaxConcurrent < 0 || cfg.QueueWait < 0 || cfg.MaxBytes < 0 {
		return nil, errors.New("whisper: HandlerConfig limits must not be negative")
	}
	h := &Handler{
		transcriber: cfg.Transcriber,
		queueWait:   cfg.QueueWait,
		maxBytes:    cfg.MaxBytes,
		log:         cfg.Logger,
	}
	for name, token := range cfg.Tokens {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("whisper: a token has no client name")
		}
		if len(token) < MinTokenLength {
			return nil, fmt.Errorf("whisper: the token of client %q is shorter than %d characters", name, MinTokenLength)
		}
		h.tokens = append(h.tokens, namedToken{name: name, sum: sha256.Sum256([]byte(token))})
	}
	n := cfg.MaxConcurrent
	if n == 0 {
		n = DefaultMaxConcurrent
	}
	h.slots = make(chan struct{}, n)
	if h.queueWait == 0 {
		h.queueWait = DefaultQueueWait
	}
	if h.maxBytes == 0 {
		h.maxBytes = DefaultMaxBytes
	}
	return h, nil
}

// InFlight is how many calls are transcribing right now.
func (h *Handler) InFlight() int { return int(h.inFlight.Load()) }

// authenticate returns the client the bearer token belongs to.
func (h *Handler) authenticate(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || token == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	found := ""
	for _, t := range h.tokens {
		if subtle.ConstantTimeCompare(sum[:], t.sum[:]) == 1 {
			found = t.name
		}
	}
	return found, found != ""
}

// ServeHTTP answers one transcription.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.fail(w, "", http.StatusMethodNotAllowed, codeMethod, "use POST", 0, start)
		return
	}
	client, ok := h.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		h.fail(w, "", http.StatusUnauthorized, codeUnauth, "a valid bearer token is required", 0, start)
		return
	}
	if r.ContentLength > h.maxBytes {
		h.fail(w, client, http.StatusRequestEntityTooLarge, codeTooLarge, "the audio is larger than the limit", 0, start)
		return
	}

	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		timer := time.NewTimer(h.queueWait)
		defer timer.Stop()
		select {
		case h.slots <- struct{}{}:
			defer func() { <-h.slots }()
		case <-timer.C:
			w.Header().Set("Retry-After", "1")
			h.fail(w, client, http.StatusTooManyRequests, codeBusy, "all slots are busy", 0, start)
			return
		case <-r.Context().Done():
			return
		}
	}

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.fail(w, client, http.StatusRequestEntityTooLarge, codeTooLarge, "the audio is larger than the limit", 0, start)
			return
		}
		h.fail(w, client, http.StatusBadRequest, codeEmptyAudio, "the body could not be read", 0, start)
		return
	}

	h.inFlight.Add(1)
	tr, err := h.transcriber.Transcribe(r.Context(), Audio{Data: data, MIMEType: r.Header.Get("Content-Type")})
	h.inFlight.Add(-1)
	if err != nil {
		status, code := classify(err)
		h.fail(w, client, status, code, publicMessage(err), len(data), start)
		return
	}

	out := wireTranscript{Text: tr.Text, Language: tr.Language, DurationMS: tr.Duration.Milliseconds()}
	for _, s := range tr.Segments {
		out.Segments = append(out.Segments, wireSegment{StartMS: s.Start.Milliseconds(), EndMS: s.End.Milliseconds(), Text: s.Text})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
	h.served.Add(1)
	h.record(client, http.StatusOK, "", len(data), tr.Duration, start)
}

// classify maps an error of Transcribe to the status and code of the answer.
func classify(err error) (int, string) {
	switch {
	case errors.Is(err, ErrEmptyAudio):
		return http.StatusBadRequest, codeEmptyAudio
	case errors.Is(err, ErrTooLarge):
		return http.StatusRequestEntityTooLarge, codeTooLarge
	case errors.Is(err, ErrTooLong):
		return http.StatusUnprocessableEntity, codeTooLong
	case errors.Is(err, ErrUnsupportedAudio):
		return http.StatusUnprocessableEntity, codeUnsupported
	case errors.Is(err, ErrTimeout):
		return http.StatusGatewayTimeout, codeTimeout
	case errors.Is(err, context.Canceled):
		return 499, codeCanceled
	case errors.Is(err, ErrBadOutput):
		return http.StatusBadGateway, codeBadOutput
	default:
		return http.StatusBadGateway, codeFailed
	}
}

// publicMessage is what the caller is told: the sentinel's own text, which
// never carries what was said, and not a ProcessError's standard error.
func publicMessage(err error) string {
	for _, s := range []error{ErrEmptyAudio, ErrTooLarge, ErrTooLong, ErrUnsupportedAudio, ErrTimeout, ErrBadOutput, ErrFailed} {
		if errors.Is(err, s) {
			return s.Error()
		}
	}
	return "whisper: transcription failed"
}

func (h *Handler) fail(w http.ResponseWriter, client string, status int, code, message string, size int, start time.Time) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(wireError{Code: code, Message: message})
	h.refused.Add(1)
	h.record(client, status, code, size, 0, start)
}

func (h *Handler) record(client string, status int, code string, size int, audio time.Duration, start time.Time) {
	if h.log == nil {
		return
	}
	attrs := []any{
		"client", client,
		"status", status,
		"bytes", size,
		"audio_ms", audio.Milliseconds(),
		"took_ms", time.Since(start).Milliseconds(),
	}
	if code != "" {
		attrs = append(attrs, "error", code)
	}
	h.log.Info("whisper.transcribe", attrs...)
}
