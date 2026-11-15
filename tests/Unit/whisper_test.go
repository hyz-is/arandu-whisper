package unit_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hyz-is/arandu-whisper"
)

// rig is a set of stand-in programs: shell scripts that log their arguments
// and write canned files. They exercise the real os/exec path without ffmpeg,
// whisper.cpp or a model.
type rig struct {
	dir     string // holds the scripts and the logs
	tempDir string // the client's TempDir, which must be empty after a call
	cfg     whisper.Config
}

const sampleJSON = `{
  "systeminfo": "x",
  "result": {"language": "pt"},
  "transcription": [
    {"timestamps": {"from": "00:00:00,000", "to": "00:00:02,500"}, "offsets": {"from": 0, "to": 2500}, "text": " A ferrugem apareceu"},
    {"timestamps": {"from": "00:00:02,500", "to": "00:00:04,000"}, "offsets": {"from": 2500, "to": 4000}, "text": " no talhão três."},
    {"timestamps": {"from": "00:00:04,000", "to": "00:00:05,000"}, "offsets": {"from": 4000, "to": 5000}, "text": " [BLANK_AUDIO]"}
  ]
}`

// newRig writes the stand-ins. probeSeconds is what the fake ffprobe prints;
// whisperBody is the shell that runs inside the fake whisper-cli after it has
// logged its arguments and found the -of value in $out.
func newRig(t *testing.T, probeSeconds string, whisperBody string) *rig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in programs are POSIX shell scripts")
	}
	dir := t.TempDir()
	r := &rig{dir: dir, tempDir: filepath.Join(dir, "tmp")}
	if err := os.Mkdir(r.tempDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Five seconds of 16 kHz mono 16-bit silence, as ffmpeg would write it.
	if err := os.WriteFile(filepath.Join(dir, "canned.wav"), wav(5*time.Second), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "canned.json"), []byte(sampleJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	script(t, dir, "ffprobe", fmt.Sprintf(`
echo "ffprobe $*" >> %[1]s/calls.log
echo %[2]s
`, dir, probeSeconds))
	script(t, dir, "ffmpeg", fmt.Sprintf(`
echo "ffmpeg $*" >> %[1]s/calls.log
for a; do out=$a; done
cp %[1]s/canned.wav "$out"
`, dir))
	script(t, dir, "whisper-cli", fmt.Sprintf(`
echo "whisper-cli $*" >> %[1]s/calls.log
while [ $# -gt 0 ]; do
  if [ "$1" = "-of" ]; then out=$2; fi
  shift
done
%[2]s
`, dir, whisperBody))

	model := filepath.Join(dir, "ggml-small.bin")
	if err := os.WriteFile(model, []byte("not a real model"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.cfg = whisper.Config{
		BinaryPath:  filepath.Join(dir, "whisper-cli"),
		ModelPath:   model,
		FFmpegPath:  filepath.Join(dir, "ffmpeg"),
		FFprobePath: filepath.Join(dir, "ffprobe"),
		TempDir:     r.tempDir,
	}
	return r
}

// okWhisper writes the canned JSON and a text file, as whisper-cli does.
func (r *rig) okWhisper() string {
	return fmt.Sprintf(`cp %[1]s/canned.json "$out.json"; echo " A ferrugem apareceu no talhao tres." > "$out.txt"`, r.dir)
}

func script(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// wav is a 16 kHz mono 16-bit PCM WAV of the given length.
func wav(d time.Duration) []byte {
	n := int(d.Seconds()) * 16000 * 2
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+n))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(16000), uint32(32000), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(n))
	b.Write(make([]byte, n))
	return b.Bytes()
}

func (r *rig) client(t *testing.T) *whisper.Client {
	t.Helper()
	c, err := whisper.New(r.cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func (r *rig) calls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.dir, "calls.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func (r *rig) call(t *testing.T, program string) string {
	t.Helper()
	for _, line := range r.calls(t) {
		if strings.HasPrefix(line, program+" ") {
			return line
		}
	}
	t.Fatalf("%s was never called; calls: %q", program, r.calls(t))
	return ""
}

func (r *rig) assertTempDirEmpty(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(r.tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the working directory was not removed: %v", entries)
	}
}

var voiceNote = whisper.Audio{Data: []byte("OggS fake opus bytes"), MIMEType: "audio/ogg; codecs=opus"}

func TestTranscribeReadsTheJSONAndCleansUp(t *testing.T) {
	r := newRig(t, "5.020000", "")
	r.cfg.Threads = 2
	r.cfg.Prompt = "cafeicultura, ferrugem, broca"
	r.cfg.Language = " PT "
	script(t, r.dir, "whisper-cli", fmt.Sprintf(`
echo "whisper-cli $*" >> %[1]s/calls.log
while [ $# -gt 0 ]; do
  if [ "$1" = "-of" ]; then out=$2; fi
  shift
done
echo "SECRET TRANSCRIPT ON STDOUT"
%[2]s
`, r.dir, r.okWhisper()))

	got, err := r.client(t).Transcribe(context.Background(), voiceNote)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	if got.Text != "A ferrugem apareceu no talhão três." {
		t.Errorf("Text = %q", got.Text)
	}
	if got.Language != "pt" {
		t.Errorf("Language = %q", got.Language)
	}
	if got.Duration != 5*time.Second {
		t.Errorf("Duration = %v", got.Duration)
	}
	want := []whisper.Segment{
		{Start: 0, End: 2500 * time.Millisecond, Text: "A ferrugem apareceu"},
		{Start: 2500 * time.Millisecond, End: 4 * time.Second, Text: "no talhão três."},
	}
	if len(got.Segments) != len(want) || got.Segments[0] != want[0] || got.Segments[1] != want[1] {
		t.Errorf("Segments = %+v, want %+v", got.Segments, want)
	}

	probe := r.call(t, "ffprobe")
	if !strings.Contains(probe, "-show_entries format=duration") || !strings.Contains(probe, "/input.ogg") {
		t.Errorf("ffprobe call = %q", probe)
	}
	conv := r.call(t, "ffmpeg")
	for _, arg := range []string{"-ac 1", "-ar 16000", "-c:a pcm_s16le", "-nostdin", "-loglevel error", "-t 301.000", "-f wav", "/input.ogg", "/audio.wav"} {
		if !strings.Contains(conv, arg) {
			t.Errorf("ffmpeg call lacks %q: %q", arg, conv)
		}
	}
	cli := r.call(t, "whisper-cli")
	for _, arg := range []string{"-m " + r.cfg.ModelPath, "-f ", "/audio.wav", "-l pt", "-oj", "-otxt", "-np", "-t 2", "--prompt cafeicultura, ferrugem, broca"} {
		if !strings.Contains(cli, arg) {
			t.Errorf("whisper-cli call lacks %q: %q", arg, cli)
		}
	}
	r.assertTempDirEmpty(t)
}

func TestTranscribeWithoutThreadsOrPromptLeavesWhisperDefaults(t *testing.T) {
	r := newRig(t, "5", "")
	script(t, r.dir, "whisper-cli", fmt.Sprintf("echo \"whisper-cli $*\" >> %s/calls.log\nwhile [ $# -gt 0 ]; do [ \"$1\" = \"-of\" ] && out=$2; shift; done\n%s\n", r.dir, r.okWhisper()))
	if _, err := r.client(t).Transcribe(context.Background(), voiceNote); err != nil {
		t.Fatal(err)
	}
	cli := r.call(t, "whisper-cli")
	if strings.Contains(cli, " -t ") || strings.Contains(cli, "--prompt") {
		t.Errorf("unexpected optional arguments: %q", cli)
	}
}

func TestTranscribeFallsBackToTheTextFileWhenTheJSONIsBroken(t *testing.T) {
	r := newRig(t, "5", `printf '{"result": {"language": "pt"}, "transcription": [{"text": "bad\nnewline"}' > "$out.json"; echo " texto simples " > "$out.txt"`)
	got, err := r.client(t).Transcribe(context.Background(), voiceNote)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "texto simples" || got.Language != "pt" || len(got.Segments) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestTranscribeReportsUnreadableOutput(t *testing.T) {
	r := newRig(t, "5", `true`)
	_, err := r.client(t).Transcribe(context.Background(), voiceNote)
	if !errors.Is(err, whisper.ErrBadOutput) {
		t.Fatalf("err = %v, want ErrBadOutput", err)
	}
	r.assertTempDirEmpty(t)
}

func TestSilenceIsAnEmptyTranscriptNotAnError(t *testing.T) {
	r := newRig(t, "5", `echo '{"result": {"language": "pt"}, "transcription": [{"offsets": {"from": 0, "to": 100}, "text": " [BLANK_AUDIO]"}]}' > "$out.json"`)
	got, err := r.client(t).Transcribe(context.Background(), voiceNote)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "" || len(got.Segments) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestEmptyAudioStartsNothing(t *testing.T) {
	r := newRig(t, "5", neverRuns)
	_, err := r.client(t).Transcribe(context.Background(), whisper.Audio{MIMEType: "audio/ogg"})
	if !errors.Is(err, whisper.ErrEmptyAudio) {
		t.Fatalf("err = %v", err)
	}
	if calls := r.calls(t); len(calls) != 0 {
		t.Errorf("programs were started: %q", calls)
	}
}

// neverRuns is the body of a whisper-cli that a test expects not to be started.
const neverRuns = "exit 99"

func TestTooLargeInputIsRefusedBeforeAnythingRuns(t *testing.T) {
	r := newRig(t, "5", neverRuns)
	r.cfg.MaxBytes = 10
	_, err := r.client(t).Transcribe(context.Background(), voiceNote)
	if !errors.Is(err, whisper.ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if calls := r.calls(t); len(calls) != 0 {
		t.Errorf("programs were started: %q", calls)
	}
	r.assertTempDirEmpty(t)
}

func TestTooLongAudioIsRefusedByTheProbeBeforeConverting(t *testing.T) {
	r := newRig(t, "400.5", neverRuns)
	_, err := r.client(t).Transcribe(context.Background(), voiceNote)
	if !errors.Is(err, whisper.ErrTooLong) {
		t.Fatalf("err = %v", err)
	}
	for _, c := range r.calls(t) {
		if !strings.HasPrefix(c, "ffprobe ") {
			t.Errorf("only ffprobe should have run, got %q", c)
		}
	}
	r.assertTempDirEmpty(t)
}

func TestTooLongAudioIsRefusedByTheConvertedLengthWhenTheProbeCannotTell(t *testing.T) {
	r := newRig(t, "N/A", neverRuns)
	r.cfg.MaxDuration = 3 * time.Second // the canned WAV lasts 5 s
	_, err := r.client(t).Transcribe(context.Background(), voiceNote)
	if !errors.Is(err, whisper.ErrTooLong) {
		t.Fatalf("err = %v", err)
	}
	cli := false
	for _, c := range r.calls(t) {
		cli = cli || strings.HasPrefix(c, "whisper-cli ")
	}
	if cli {
		t.Error("whisper-cli ran on audio over the limit")
	}
	if conv := r.call(t, "ffmpeg"); !strings.Contains(conv, "-t 4.000") {
		t.Errorf("ffmpeg was not capped a second past the limit: %q", conv)
	}
}

func TestTimeoutKillsWhisperAndNamesTheCause(t *testing.T) {
	r := newRig(t, "5", `exec sleep 30`)
	r.cfg.Timeout = 400 * time.Millisecond
	start := time.Now()
	_, err := r.client(t).Transcribe(context.Background(), voiceNote)
	if !errors.Is(err, whisper.ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ErrTimeout and DeadlineExceeded", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the process was not killed: took %v", took)
	}
	r.assertTempDirEmpty(t)
}

func TestACancelledContextIsReportedAsCancelled(t *testing.T) {
	r := newRig(t, "5", `exec sleep 30`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	_, err := r.client(t).Transcribe(ctx, voiceNote)
	if !errors.Is(err, context.Canceled) || errors.Is(err, whisper.ErrTimeout) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	r.assertTempDirEmpty(t)
}

func TestWhisperFailureCarriesAShortStderrAndNeverTheTranscript(t *testing.T) {
	r := newRig(t, "5", `echo "SPOKEN WORDS: a safra de 2026 foi ruim" ; echo "error: failed to load model $out" >&2; exit 3`)
	_, err := r.client(t).Transcribe(context.Background(), voiceNote)
	if !errors.Is(err, whisper.ErrFailed) {
		t.Fatalf("err = %v, want ErrFailed", err)
	}
	var perr *whisper.ProcessError
	if !errors.As(err, &perr) {
		t.Fatalf("err is not a *ProcessError: %T", err)
	}
	if perr.ExitCode != 3 || perr.Program != "whisper-cli" {
		t.Errorf("ProcessError = %+v", perr)
	}
	msg := err.Error()
	if !strings.Contains(msg, "failed to load model") || !strings.Contains(msg, "<tmp>/out") {
		t.Errorf("message lacks the summary: %q", msg)
	}
	if strings.Contains(msg, "SPOKEN WORDS") || strings.Contains(msg, r.tempDir) {
		t.Errorf("message leaks stdout or the working directory: %q", msg)
	}
	r.assertTempDirEmpty(t)
}

func TestHugeStderrIsTruncated(t *testing.T) {
	r := newRig(t, "5", `i=0; while [ $i -lt 2000 ]; do echo "line $i of noise noise noise" >&2; i=$((i+1)); done; exit 1`)
	_, err := r.client(t).Transcribe(context.Background(), voiceNote)
	if !errors.Is(err, whisper.ErrFailed) {
		t.Fatalf("err = %v", err)
	}
	if len(err.Error()) > 500 {
		t.Errorf("message is %d bytes", len(err.Error()))
	}
}

func TestUnreadableAudioIsReportedAsUnsupported(t *testing.T) {
	r := newRig(t, "5", neverRuns)
	script(t, r.dir, "ffmpeg", fmt.Sprintf("echo \"ffmpeg $*\" >> %s/calls.log\necho \"input: Invalid data found when processing input\" >&2\nexit 1\n", r.dir))
	_, err := r.client(t).Transcribe(context.Background(), whisper.Audio{Data: []byte("not audio")})
	if !errors.Is(err, whisper.ErrUnsupportedAudio) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "Invalid data found") {
		t.Errorf("message lacks the reason: %q", err)
	}
	if errors.Is(err, whisper.ErrFailed) {
		t.Error("an ffmpeg failure must not look like a whisper failure")
	}
	r.assertTempDirEmpty(t)
}

func TestAnUnreadableProbeIsReportedAsUnsupported(t *testing.T) {
	r := newRig(t, "5", neverRuns)
	script(t, r.dir, "ffprobe", "echo bad >&2\nexit 1\n")
	_, err := r.client(t).Transcribe(context.Background(), voiceNote)
	if !errors.Is(err, whisper.ErrUnsupportedAudio) {
		t.Fatalf("err = %v", err)
	}
}

func TestConcurrentCallsDoNotShareAWorkingDirectory(t *testing.T) {
	r := newRig(t, "5", neverRuns)
	script(t, r.dir, "whisper-cli", fmt.Sprintf("while [ $# -gt 0 ]; do [ \"$1\" = \"-of\" ] && out=$2; shift; done\n%s\n", r.okWhisper()))
	c := r.client(t)
	errs := make(chan error, 8)
	for i := 0; i < cap(errs); i++ {
		go func() {
			got, err := c.Transcribe(context.Background(), voiceNote)
			if err == nil && got.Text == "" {
				err = errors.New("empty transcript")
			}
			errs <- err
		}()
	}
	for i := 0; i < cap(errs); i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	r.assertTempDirEmpty(t)
}

func TestNewValidatesTheConfig(t *testing.T) {
	good := func(t *testing.T) (*rig, string) {
		r := newRig(t, "5", "")
		notExec := filepath.Join(r.dir, "plain.txt")
		if err := os.WriteFile(notExec, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return r, notExec
	}

	cases := []struct {
		name   string
		mutate func(r *rig, notExec string)
		want   string
	}{
		{"binary missing", func(r *rig, _ string) { r.cfg.BinaryPath = "" }, "BinaryPath: is required"},
		{"binary absent", func(r *rig, _ string) { r.cfg.BinaryPath = filepath.Join(r.dir, "nope") }, "BinaryPath"},
		{"binary is a directory", func(r *rig, _ string) { r.cfg.BinaryPath = r.dir }, "BinaryPath"},
		{"binary not executable", func(r *rig, notExec string) { r.cfg.BinaryPath = notExec }, "not executable"},
		{"binary not in PATH", func(r *rig, _ string) { r.cfg.BinaryPath = "whisper-cli-that-does-not-exist" }, "not found in PATH"},
		{"model missing", func(r *rig, _ string) { r.cfg.ModelPath = "" }, "ModelPath: is required"},
		{"model absent", func(r *rig, _ string) { r.cfg.ModelPath = filepath.Join(r.dir, "nope.bin") }, "ModelPath"},
		{"model is a directory", func(r *rig, _ string) { r.cfg.ModelPath = r.dir }, "not a regular file"},
		{"model is empty", func(r *rig, _ string) {
			p := filepath.Join(r.dir, "empty.bin")
			_ = os.WriteFile(p, nil, 0o600)
			r.cfg.ModelPath = p
		}, "is empty"},
		{"ffmpeg absent", func(r *rig, _ string) { r.cfg.FFmpegPath = filepath.Join(r.dir, "nope") }, "FFmpegPath"},
		{"ffprobe absent", func(r *rig, _ string) { r.cfg.FFprobePath = filepath.Join(r.dir, "nope") }, "FFprobePath"},
		{"language garbage", func(r *rig, _ string) { r.cfg.Language = "portugues-br" }, "Language"},
		{"language flag-like", func(r *rig, _ string) { r.cfg.Language = "--help" }, "Language"},
		{"negative threads", func(r *rig, _ string) { r.cfg.Threads = -1 }, "Threads"},
		{"negative timeout", func(r *rig, _ string) { r.cfg.Timeout = -time.Second }, "Timeout"},
		{"negative max duration", func(r *rig, _ string) { r.cfg.MaxDuration = -time.Second }, "MaxDuration"},
		{"negative max bytes", func(r *rig, _ string) { r.cfg.MaxBytes = -1 }, "MaxBytes"},
		{"temp dir absent", func(r *rig, _ string) { r.cfg.TempDir = filepath.Join(r.dir, "nope") }, "TempDir"},
		{"temp dir is a file", func(r *rig, notExec string) { r.cfg.TempDir = notExec }, "not a directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, notExec := good(t)
			tc.mutate(r, notExec)
			c, err := whisper.New(r.cfg)
			if err == nil {
				t.Fatalf("New accepted the config: %+v", c)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "whisper: ") {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}

	t.Run("a complete config works and fills the defaults", func(t *testing.T) {
		r, _ := good(t)
		if _, err := whisper.New(r.cfg); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("ffprobe is found beside ffmpeg", func(t *testing.T) {
		r, _ := good(t)
		r.cfg.FFprobePath = ""
		if _, err := whisper.New(r.cfg); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("auto is a language", func(t *testing.T) {
		r, _ := good(t)
		r.cfg.Language = "auto"
		if _, err := whisper.New(r.cfg); err != nil {
			t.Fatal(err)
		}
	})
}

func TestNewDoesNotStartAnything(t *testing.T) {
	r := newRig(t, "5", "")
	if _, err := whisper.New(r.cfg); err != nil {
		t.Fatal(err)
	}
	if calls := r.calls(t); len(calls) != 0 {
		t.Errorf("New started programs: %q", calls)
	}
}

func TestFakeAnswersInOrderAndRecords(t *testing.T) {
	boom := errors.New("boom")
	f := whisper.NewFake(
		whisper.Answer{Text: "primeiro", Duration: 3 * time.Second},
		whisper.Answer{Text: "second", Language: "en"},
		whisper.Answer{Err: boom},
	)
	ctx := context.Background()

	a, err := f.Transcribe(ctx, whisper.Audio{Data: []byte("1"), MIMEType: "audio/ogg"})
	if err != nil || a.Text != "primeiro" || a.Language != "pt" || a.Duration != 3*time.Second {
		t.Fatalf("first = %+v, %v", a, err)
	}
	b, err := f.Transcribe(ctx, whisper.Audio{Data: []byte("2")})
	if err != nil || b.Language != "en" {
		t.Fatalf("second = %+v, %v", b, err)
	}
	if _, err := f.Transcribe(ctx, whisper.Audio{Data: []byte("3")}); !errors.Is(err, boom) {
		t.Fatalf("third err = %v", err)
	}
	if _, err := f.Transcribe(ctx, whisper.Audio{Data: []byte("4")}); !errors.Is(err, whisper.ErrScriptExhausted) {
		t.Fatalf("exhausted err = %v", err)
	}
	f.Push(whisper.Answer{Text: "pushed"})
	if got, err := f.Transcribe(ctx, whisper.Audio{Data: []byte("5")}); err != nil || got.Text != "pushed" {
		t.Fatalf("pushed = %+v, %v", got, err)
	}

	reqs := f.Requests()
	if len(reqs) != 5 || reqs[0].MIMEType != "audio/ogg" || string(reqs[4].Data) != "5" {
		t.Errorf("requests = %+v", reqs)
	}
}

func TestFakeRefusesEmptyAudioWithoutUsingAnAnswer(t *testing.T) {
	f := whisper.NewFake(whisper.Answer{Text: "ok"})
	if _, err := f.Transcribe(context.Background(), whisper.Audio{}); !errors.Is(err, whisper.ErrEmptyAudio) {
		t.Fatalf("err = %v", err)
	}
	if got, err := f.Transcribe(context.Background(), whisper.Audio{Data: []byte("x")}); err != nil || got.Text != "ok" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// TestRealFFmpegConvertsAnOpusVoiceNote runs the real ffmpeg and ffprobe on an
// Ogg Opus note, as WhatsApp sends it, with a stand-in whisper-cli. It proves
// the conversion arguments and the duration read, which the stand-in ffmpeg
// cannot. It is skipped when ffmpeg or ffprobe is missing.
func TestRealFFmpegConvertsAnOpusVoiceNote(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg is not in PATH: %v", err)
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skipf("ffprobe is not in PATH: %v", err)
	}
	note, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi",
		"-i", "sine=frequency=440:duration=2", "-c:a", "libopus", "-f", "ogg", "pipe:1").Output()
	if err != nil {
		t.Skipf("ffmpeg cannot generate a test tone: %v", err)
	}

	r := newRig(t, "0", "")
	// The stand-in whisper-cli checks that it was handed a real 16 kHz mono WAV.
	script(t, r.dir, "whisper-cli", fmt.Sprintf(`
while [ $# -gt 0 ]; do
  [ "$1" = "-f" ] && wav=$2
  [ "$1" = "-of" ] && out=$2
  shift
done
head -c 4 "$wav" | grep -q RIFF || exit 4
%s
`, r.okWhisper()))
	r.cfg.FFmpegPath, r.cfg.FFprobePath = ffmpeg, ffprobe

	got, err := r.client(t).Transcribe(context.Background(), whisper.Audio{Data: note, MIMEType: "audio/ogg; codecs=opus"})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if d := got.Duration - 2*time.Second; d < -100*time.Millisecond || d > 100*time.Millisecond {
		t.Errorf("Duration = %v, want about 2s", got.Duration)
	}

	r.cfg.MaxDuration = time.Second
	_, err = r.client(t).Transcribe(context.Background(), whisper.Audio{Data: note})
	if !errors.Is(err, whisper.ErrTooLong) {
		t.Errorf("a 2 s note under a 1 s limit: err = %v, want ErrTooLong", err)
	}

	_, err = r.client(t).Transcribe(context.Background(), whisper.Audio{Data: []byte("this is not audio at all")})
	if !errors.Is(err, whisper.ErrUnsupportedAudio) {
		t.Errorf("garbage bytes: err = %v, want ErrUnsupportedAudio", err)
	}
	r.assertTempDirEmpty(t)
}

// TestRealWhisper runs the real programs. It needs whisper.cpp and a model, so
// it only runs when WHISPER_CLI and WHISPER_MODEL name them. WHISPER_AUDIO may
// name a recording with speech, in which case the transcript must not be
// empty; without it the test transcribes a generated tone and only checks that
// the whole chain runs and measures the duration.
func TestRealWhisper(t *testing.T) {
	cli, model := os.Getenv("WHISPER_CLI"), os.Getenv("WHISPER_MODEL")
	if cli == "" || model == "" {
		t.Skip("set WHISPER_CLI (path of whisper-cli) and WHISPER_MODEL (path of a ggml model) to run against the real whisper.cpp")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skipf("ffmpeg is not in PATH: %v", err)
	}

	cfg := whisper.Config{BinaryPath: cli, ModelPath: model, FFmpegPath: ffmpeg, Language: "pt"}
	c, err := whisper.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var audio whisper.Audio
	want := time.Second
	if path := os.Getenv("WHISPER_AUDIO"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		audio = whisper.Audio{Data: data}
	} else {
		out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi",
			"-i", "sine=frequency=440:duration=1", "-c:a", "libopus", "-f", "ogg", "pipe:1").Output()
		if err != nil {
			t.Skipf("ffmpeg cannot generate a test tone: %v", err)
		}
		audio = whisper.Audio{Data: out, MIMEType: "audio/ogg"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	got, err := c.Transcribe(ctx, audio)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	t.Logf("language=%q duration=%v segments=%d text=%q", got.Language, got.Duration, len(got.Segments), got.Text)
	if got.Language == "" {
		t.Error("no language")
	}
	if os.Getenv("WHISPER_AUDIO") != "" {
		if strings.TrimSpace(got.Text) == "" {
			t.Error("the recording has speech and the transcript is empty")
		}
	} else if d := got.Duration - want; d < -100*time.Millisecond || d > 100*time.Millisecond {
		t.Errorf("Duration = %v, want about %v", got.Duration, want)
	}
}
