// Package whisper turns speech into text by running whisper.cpp on the
// application's own server: no audio leaves the machine and there is no
// per-minute cost.
//
// It is a thin layer over three programs the application installs: ffmpeg and
// ffprobe, which read whatever audio arrived (a WhatsApp voice note is Ogg
// Opus) and produce a 16 kHz mono WAV, and whisper.cpp's whisper-cli, which
// reads that WAV. Each program is started with fixed arguments through
// os/exec, never through a shell, inside a private temporary directory that is
// removed before the call returns. The package adds what an application
// otherwise writes by hand every time: a configuration that is refused at boot
// rather than on the first voice note, limits on size and duration, a timeout
// that also kills the process, errors that never carry what was said, and an
// interface with a Fake so the code that transcribes is tested without the
// programs or a model.
//
// It owns no table, no route and no policy, and it does not import the
// framework, so an application on any framework version installs it.
package whisper

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Audio is one recording: its bytes and the MIME type it arrived with. The
// type is only a hint for the file name given to the converter, which
// recognizes the format from the content, so an empty or wrong type does not
// make a readable recording unreadable.
type Audio struct {
	Data     []byte
	MIMEType string
}

// Segment is one stretch of speech whisper recognized, with its position in
// the audio.
type Segment struct {
	Start time.Duration
	End   time.Duration
	Text  string
}

// Transcript is what was said.
type Transcript struct {
	// Text is the whole transcript, trimmed. It is empty when the audio holds
	// no speech, which is not an error.
	Text string
	// Language is the language code whisper used or detected, such as "pt".
	Language string
	// Duration is the length of the audio.
	Duration time.Duration
	// Segments are the recognized stretches in order. It is empty when the
	// structured output of whisper-cli could not be read and the plain-text
	// output was used instead.
	Segments []Segment
}

// Transcriber is what turns audio into text. *Client implements it with
// whisper.cpp and *Fake in memory, so the code that depends on it is tested
// without the programs, the model or the CPU time.
type Transcriber interface {
	Transcribe(ctx context.Context, audio Audio) (Transcript, error)
}

// The errors Transcribe returns. Test for them with errors.Is.
var (
	// ErrEmptyAudio means there were no bytes to transcribe, or the converter
	// produced no samples.
	ErrEmptyAudio = errors.New("whisper: the audio is empty")
	// ErrTooLarge means the input is larger than Config.MaxBytes.
	ErrTooLarge = errors.New("whisper: the audio file is too large")
	// ErrTooLong means the audio lasts longer than Config.MaxDuration.
	ErrTooLong = errors.New("whisper: the audio is too long")
	// ErrUnsupportedAudio means ffmpeg or ffprobe could not read the bytes as
	// audio. The error is a *ProcessError carrying a short summary.
	ErrUnsupportedAudio = errors.New("whisper: the audio could not be read")
	// ErrTimeout means Config.Timeout, or the caller's deadline, elapsed. The
	// running program was killed. It also satisfies
	// errors.Is(err, context.DeadlineExceeded).
	ErrTimeout = errors.New("whisper: transcription timed out")
	// ErrFailed means whisper-cli could not run or exited with an error. The
	// error is a *ProcessError carrying a short summary.
	ErrFailed = errors.New("whisper: whisper-cli failed")
	// ErrBadOutput means whisper-cli exited successfully but wrote nothing
	// this package can read.
	ErrBadOutput = errors.New("whisper: whisper-cli wrote no readable output")
)

// ProcessError says that one of the programs failed. Its text is built from
// the exit status and a short summary of the program's standard error, with
// the working directory replaced by "<tmp>". The program's standard output,
// which is where whisper-cli prints what was said, is never captured, so a
// ProcessError cannot leak a transcript.
type ProcessError struct {
	// Program is the base name of the program that failed.
	Program string
	// ExitCode is the exit status, or -1 when the program did not start or
	// was ended by a signal.
	ExitCode int
	// Stderr is the summarized standard error.
	Stderr string

	kind error
}

// Error describes the failure.
func (e *ProcessError) Error() string {
	msg := fmt.Sprintf("%v: %s exited with status %d", e.kind, e.Program, e.ExitCode)
	if e.ExitCode < 0 {
		msg = fmt.Sprintf("%v: %s did not finish", e.kind, e.Program)
	}
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

// Unwrap returns ErrUnsupportedAudio for ffmpeg and ffprobe and ErrFailed for
// whisper-cli.
func (e *ProcessError) Unwrap() error { return e.kind }

// Transcribe converts the audio to a 16 kHz mono WAV and runs whisper-cli on
// it. It refuses audio larger than MaxBytes or longer than MaxDuration before
// running whisper, and ends all work, killing the running program, when ctx is
// done or Timeout elapses.
func (c *Client) Transcribe(ctx context.Context, audio Audio) (Transcript, error) {
	if len(audio.Data) == 0 {
		return Transcript{}, ErrEmptyAudio
	}
	if int64(len(audio.Data)) > c.maxBytes {
		return Transcript{}, fmt.Errorf("%w: %d bytes, the limit is %d", ErrTooLarge, len(audio.Data), c.maxBytes)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	dir, err := os.MkdirTemp(c.tempDir, "arandu-whisper-")
	if err != nil {
		return Transcript{}, fmt.Errorf("whisper: making the working directory: %w", err)
	}
	defer os.RemoveAll(dir)

	in := filepath.Join(dir, "input"+extensionFor(audio.MIMEType))
	if err := os.WriteFile(in, audio.Data, 0o600); err != nil {
		return Transcript{}, fmt.Errorf("whisper: writing the audio: %w", err)
	}

	// The early refusal: the duration is read from the container, so a long
	// recording is turned away before ffmpeg spends any time on it.
	if probed, known, err := c.probe(ctx, dir, in); err != nil {
		return Transcript{}, err
	} else if known && probed > c.maxDuration {
		return Transcript{}, tooLong(probed, c.maxDuration)
	}

	wav := filepath.Join(dir, "audio.wav")
	if err := c.convert(ctx, dir, in, wav); err != nil {
		return Transcript{}, err
	}
	duration, err := wavDuration(wav)
	if err != nil {
		return Transcript{}, err
	}
	if duration > c.maxDuration {
		return Transcript{}, tooLong(duration, c.maxDuration)
	}

	var result Transcript
	if c.server != "" {
		result, err = c.recognizeOnServer(ctx, wav)
	} else {
		base := filepath.Join(dir, "out")
		if err = c.recognize(ctx, dir, wav, base); err == nil {
			result, err = readOutput(base, c.language)
		}
	}
	if err != nil {
		return Transcript{}, err
	}
	result.Duration = duration
	return result, nil
}

func tooLong(got, limit time.Duration) error {
	return fmt.Errorf("%w: %s, the limit is %s", ErrTooLong, got.Round(time.Second), limit)
}

// probe reads the duration of the input. known is false when the container
// does not say.
func (c *Client) probe(ctx context.Context, dir, in string) (d time.Duration, known bool, err error) {
	out, runErr := c.run(ctx, dir, c.ffprobe, ErrUnsupportedAudio, true,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		in,
	)
	if runErr != nil {
		return 0, false, runErr
	}
	text := strings.TrimSpace(string(out))
	seconds, parseErr := strconv.ParseFloat(text, 64)
	if parseErr != nil || seconds < 0 {
		return 0, false, nil
	}
	return time.Duration(seconds * float64(time.Second)), true, nil
}

// convert writes a 16 kHz mono 16-bit PCM WAV, the one input whisper-cli
// reads. The output is capped a second past MaxDuration, so a container that
// hides its length cannot make ffmpeg decode hours of audio.
func (c *Client) convert(ctx context.Context, dir, in, out string) error {
	limit := strconv.FormatFloat((c.maxDuration + time.Second).Seconds(), 'f', 3, 64)
	_, err := c.run(ctx, dir, c.ffmpeg, ErrUnsupportedAudio, false,
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		"-y",
		"-i", in,
		"-vn",
		"-ac", "1",
		"-ar", "16000",
		"-c:a", "pcm_s16le",
		"-t", limit,
		"-f", "wav",
		out,
	)
	return err
}

// recognize runs whisper-cli, which writes base+".json" and base+".txt". The
// plain-text file is a fallback for the day the JSON cannot be parsed.
func (c *Client) recognize(ctx context.Context, dir, wav, base string) error {
	args := []string{
		"-m", c.model,
		"-f", wav,
		"-l", c.language,
		"-oj",
		"-otxt",
		"-of", base,
		"-np",
	}
	if c.threads > 0 {
		args = append(args, "-t", strconv.Itoa(c.threads))
	}
	if c.prompt != "" {
		args = append(args, "--prompt", c.prompt)
	}
	_, err := c.run(ctx, dir, c.binary, ErrFailed, false, args...)
	return err
}

// run starts one program without a shell and waits for it. Standard output is
// kept only when keepStdout is set (ffprobe's answer); otherwise it is
// discarded, because whisper-cli prints the transcript there.
func (c *Client) run(ctx context.Context, dir, program string, kind error, keepStdout bool, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.WaitDelay = 2 * time.Second

	var stdout bytes.Buffer
	if keepStdout {
		cmd.Stdout = &limitedWriter{buf: &stdout, max: 4096}
	} else {
		cmd.Stdout = io.Discard
	}
	stderr := &tailBuffer{max: 2048}
	cmd.Stderr = stderr

	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %w", ErrTimeout, ctxErr)
		}
		return nil, ctxErr
	}
	perr := &ProcessError{
		Program:  filepath.Base(program),
		ExitCode: -1,
		Stderr:   summarize(stderr.String(), dir),
		kind:     kind,
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		perr.ExitCode = exit.ExitCode()
	} else {
		perr.Stderr = summarize(err.Error(), dir)
	}
	return nil, perr
}

// whisperJSON is the part of whisper-cli's -oj file this package reads:
// result.language and, per segment, the offsets in milliseconds and the text.
type whisperJSON struct {
	Result struct {
		Language string `json:"language"`
	} `json:"result"`
	Transcription []struct {
		Offsets struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		} `json:"offsets"`
		Text string `json:"text"`
	} `json:"transcription"`
}

// readOutput reads what whisper-cli wrote: the JSON when it parses, the text
// file otherwise.
func readOutput(base, language string) (Transcript, error) {
	if raw, err := os.ReadFile(base + ".json"); err == nil {
		var doc whisperJSON
		if json.Unmarshal(raw, &doc) == nil {
			return fromJSON(doc, language), nil
		}
	}
	raw, err := os.ReadFile(base + ".txt")
	if err != nil {
		return Transcript{}, ErrBadOutput
	}
	t := Transcript{Text: strings.TrimSpace(string(raw))}
	if language != "auto" {
		t.Language = language
	}
	return t, nil
}

func fromJSON(doc whisperJSON, language string) Transcript {
	t := Transcript{Language: doc.Result.Language}
	if t.Language == "" && language != "auto" {
		t.Language = language
	}
	var parts []string
	for _, s := range doc.Transcription {
		text := strings.TrimSpace(s.Text)
		if text == "" || strings.EqualFold(text, "[BLANK_AUDIO]") {
			continue
		}
		parts = append(parts, text)
		t.Segments = append(t.Segments, Segment{
			Start: time.Duration(s.Offsets.From) * time.Millisecond,
			End:   time.Duration(s.Offsets.To) * time.Millisecond,
			Text:  text,
		})
	}
	t.Text = strings.Join(parts, " ")
	return t
}

// wavDuration reads the length of the WAV ffmpeg wrote: 16 kHz, mono, 16-bit,
// so 32000 bytes per second of the "data" chunk.
func wavDuration(path string) (time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, ErrUnsupportedAudio
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, ErrUnsupportedAudio
	}

	var header [12]byte
	if _, err := io.ReadFull(f, header[:]); err != nil || string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return 0, ErrUnsupportedAudio
	}
	offset := int64(12)
	for {
		var chunk [8]byte
		if _, err := io.ReadFull(f, chunk[:]); err != nil {
			return 0, ErrEmptyAudio
		}
		size := int64(binary.LittleEndian.Uint32(chunk[4:8]))
		offset += 8
		if string(chunk[0:4]) == "data" {
			// A writer that could not seek back leaves the size unset.
			if remaining := info.Size() - offset; size == 0xFFFFFFFF || size > remaining {
				size = remaining
			}
			if size <= 0 {
				return 0, ErrEmptyAudio
			}
			const bytesPerSecond = 16000 * 2
			return time.Duration(size) * time.Second / bytesPerSecond, nil
		}
		offset += size + size%2
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return 0, ErrUnsupportedAudio
		}
	}
}

// extensionFor turns a MIME type into a file extension for the converter's
// benefit. Unknown types get none: ffmpeg looks at the content.
func extensionFor(mimeType string) string {
	base, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return ""
	}
	switch strings.ToLower(base) {
	case "audio/ogg", "audio/opus", "application/ogg":
		return ".ogg"
	case "audio/mpeg", "audio/mp3":
		return ".mp3"
	case "audio/mp4", "audio/m4a", "audio/x-m4a", "audio/aac":
		return ".m4a"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return ".wav"
	case "audio/webm", "video/webm":
		return ".webm"
	case "audio/flac", "audio/x-flac":
		return ".flac"
	case "audio/amr":
		return ".amr"
	}
	return ""
}

// summarize reduces a program's standard error to one short line: the working
// directory is replaced, whitespace is collapsed and the length is capped.
func summarize(text, dir string) string {
	text = strings.ReplaceAll(text, dir, "<tmp>")
	text = strings.Join(strings.Fields(text), " ")
	const max = 300
	if r := []rune(text); len(r) > max {
		text = string(r[:max]) + "..."
	}
	return text
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// limitedWriter keeps at most max bytes and drops the rest.
type limitedWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
