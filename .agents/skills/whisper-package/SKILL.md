---
name: whisper-package
description: Install, wire and use the Whisper package (github.com/hyz-is/arandu-whisper) in an Arandu application. Use when the request is to "transcribe audio", "transcribe a voice note", "speech to text", "WhatsApp audio to text", "use whisper.cpp", "add whisper to the app", "go get github.com/hyz-is/arandu-whisper", "wire whisper into bootstrap/app.go", "test code that transcribes", "whisper-cli not found", "ErrTimeout", "ErrTooLong", "ErrUnsupportedAudio", or when a project's go.mod already requires github.com/hyz-is/arandu-whisper. Covers the paths in the application's config, the one construction in bootstrap, Transcribe, the errors, the Fake for tests, what must be installed on the server, and why no audio or transcript goes into a log.
license: MIT
metadata:
  audience: app
---

# Using Whisper

A library: `whisper.New(Config)` builds a `*whisper.Client`, which implements
`whisper.Transcriber`. It has no table, no route and no policy, so there is no
`k.Register`, no migration and no permission to open. It runs three programs on
the server, so the deployment has to carry them.

## Install

```bash
go get github.com/hyz-is/arandu-whisper@<latest tag>   # gh release list -R hyz-is/arandu-whisper
```

On the server (the README has a Dockerfile that builds all of it):

- `whisper-cli`, compiled from a whisper.cpp release;
- a ggml model, such as `ggml-small.bin`, from the official Hugging Face
  repository. `small` is the recommended start for Brazilian Portuguese on a
  CPU, `base` when memory is tight;
- `ffmpeg` and `ffprobe`.

## Wire it, in bootstrap/app.go

1. Put the two paths in the application's config, read from `WHISPER_CLI` and
   `WHISPER_MODEL`, and add the empty variables to `.env.example`.
2. Build the client once, only when both are configured:

   ```go
   var stt whisper.Transcriber
   if cfg.Whisper.Configured() {
       c, err := whisper.New(whisper.Config{
           BinaryPath: cfg.Whisper.Binary,
           ModelPath:  cfg.Whisper.Model,
           Threads:    4,
       })
       if err != nil {
           return App{}, err
       }
       stt = c
   }
   ```

3. Pass it to the services that need it, typed as `whisper.Transcriber`. A
   service with a nil Transcriber answers "not configured" instead of calling.

`New` fails when a program or the model is missing, so a bad deployment stops at
the first restart.

## Use it

- `Transcribe(ctx, whisper.Audio{Data, MIMEType})` returns a `Transcript` with
  `Text`, `Language`, `Duration` and `Segments`. Silence gives an empty `Text`,
  not an error.
- The MIME type is a hint; ffmpeg recognizes the format from the bytes. WhatsApp
  voice notes are `audio/ogg; codecs=opus`.
- Errors: `ErrEmptyAudio`, `ErrTooLarge`, `ErrTooLong`, `ErrUnsupportedAudio`,
  `ErrTimeout`, `ErrFailed` (with `errors.As` to a `*whisper.ProcessError`),
  `ErrBadOutput`, and `context.Canceled` when the caller cancels. Tell the
  person plainly for the first four; do not retry `ErrTooLong` or
  `ErrUnsupportedAudio`.
- It is CPU bound: call it from a job and bound how many run at once. Set
  `Timeout` from a measured run of a real note on the server.
- `Config.Prompt` takes a short vocabulary of the domain (names, technical
  terms) and improves spelling of those words.
- Never log the audio, `Transcript.Text` or a `ProcessError`'s fields beyond the
  exit status. They can carry personal data. The package itself logs nothing.

## Test with it

```go
fake := whisper.NewFake(whisper.Answer{Text: "preciso de adubo"}, whisper.Answer{Err: whisper.ErrTooLong})
// pass fake where production passes the client
fake.Requests() // the audio received, in order
```
