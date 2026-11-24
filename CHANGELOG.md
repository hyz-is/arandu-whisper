# Changelog

Everything worth knowing about a release of Whisper is recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
the versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

A published module version is immutable: Go serves it from the proxy forever, so
a release is corrected by another release and never by moving a tag.

## [Unreleased]

## [0.2.0] - 2026-11-24

### Added

- `Handler` (`NewHandler(HandlerConfig)`): serves any `Transcriber` over HTTP. `POST` with the audio as the body and its MIME type as `Content-Type`, answered with the transcript as JSON (`text`, `language`, `duration_ms`, `segments`). It checks a bearer token per client (compared in constant time, at least 32 characters, never logged), limits the body before and while reading it, limits the concurrency and answers `429` with `Retry-After` when no slot frees within `QueueWait`, and logs one line per call with the client, status, size and time, never the audio or the text. Errors are `{"error": code, "message": text}` with a stable code per sentinel error.
- `Remote` (`NewRemote(RemoteConfig)`): a `Transcriber` that calls nodes running `Handler`, so an application switches between the local `*Client` and a remote service by configuration. It spreads calls over the nodes in turn, tries the next node at once when one is busy, unreachable, timed out or failing, takes a node out of the rotation after `FailureThreshold` failures in a row for `Cooldown`, tries the nodes that are out as a last resort when all are, and returns a refusal another node would repeat (empty, too large, too long or unreadable audio) as the same sentinel error the local client gives. `Remote.Health` asks every node's `/_arandu/health` and reports the place of each in the rotation.
- `ParseNodes` reads the nodes from one configuration value: `one=https://one.example,two=https://two.example`, bare URLs, or a single URL.
- `Config.ServerURL`: sends the converted audio to a resident whisper.cpp `whisper-server` (`/inference`) instead of starting `whisper-cli` on every call, so the model is loaded once. With it, `BinaryPath` and `ModelPath` are not required. The conversion, the limits and the cleanup are unchanged.

### Changed

- `arandu.mod.toml` declares `network = true`: the module can now use the network, and only when `ServerURL`, `Handler` or `Remote` is configured. Nothing changes for an application that only uses `New` with `BinaryPath` and `ModelPath`.

## [0.1.0] - 2026-11-15

### Added

- `New(Config)` builds a `*Client`, refusing a missing or non-executable `whisper-cli`, `ffmpeg` or `ffprobe`, a missing or empty model, an unrecognized language, a negative limit and a `TempDir` that is not a directory.
- `Transcribe(ctx, Audio)` converts the audio to a 16 kHz mono WAV with ffmpeg inside a private temporary directory, runs whisper.cpp's `whisper-cli` on it and returns a `Transcript` with the text, the language, the duration and the segments. The directory is removed on every path.
- Limits checked before the expensive step: `MaxBytes` (default 32 MiB), `MaxDuration` (default 5 minutes, read with ffprobe and again from the converted WAV) and `Timeout` (default 3 minutes, covering the whole call and killing the running program).
- Errors to test with `errors.Is`: `ErrEmptyAudio`, `ErrTooLarge`, `ErrTooLong`, `ErrUnsupportedAudio`, `ErrTimeout`, `ErrFailed`, `ErrBadOutput`. A failing program is a `*ProcessError` with its exit status and a short stderr summary, never the transcript.
- Defaults: language `pt`, whisper's own thread count, no prompt. `Config.Prompt` biases spelling toward a vocabulary.
- `Transcriber` and `Fake`, a scripted `Transcriber` that records the audio it received, for an application's tests.
