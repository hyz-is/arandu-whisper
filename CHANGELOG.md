# Changelog

Everything worth knowing about a release of Whisper is recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
the versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

A published module version is immutable: Go serves it from the proxy forever, so
a release is corrected by another release and never by moving a tag.

## [Unreleased]

## [0.1.0] - 2026-11-15

### Added

- `New(Config)` builds a `*Client`, refusing a missing or non-executable `whisper-cli`, `ffmpeg` or `ffprobe`, a missing or empty model, an unrecognized language, a negative limit and a `TempDir` that is not a directory.
- `Transcribe(ctx, Audio)` converts the audio to a 16 kHz mono WAV with ffmpeg inside a private temporary directory, runs whisper.cpp's `whisper-cli` on it and returns a `Transcript` with the text, the language, the duration and the segments. The directory is removed on every path.
- Limits checked before the expensive step: `MaxBytes` (default 32 MiB), `MaxDuration` (default 5 minutes, read with ffprobe and again from the converted WAV) and `Timeout` (default 3 minutes, covering the whole call and killing the running program).
- Errors to test with `errors.Is`: `ErrEmptyAudio`, `ErrTooLarge`, `ErrTooLong`, `ErrUnsupportedAudio`, `ErrTimeout`, `ErrFailed`, `ErrBadOutput`. A failing program is a `*ProcessError` with its exit status and a short stderr summary, never the transcript.
- Defaults: language `pt`, whisper's own thread count, no prompt. `Config.Prompt` biases spelling toward a vocabulary.
- `Transcriber` and `Fake`, a scripted `Transcriber` that records the audio it received, for an application's tests.
