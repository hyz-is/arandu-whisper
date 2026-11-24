# Working on Whisper

This is an Arandu package that is a library: it puts whisper.cpp behind one
call, `Transcribe`, and an interface, `Transcriber`, with a `Fake`. It owns no
table, no route and no policy, and it does not import the framework, so an
application on any framework version can install it. It is a Go module
somebody `go get`s and builds by hand in their own `bootstrap/app.go`.

Read `.agents/skills/` before writing code.

## The gates

Nothing is finished until all four exit zero.

```sh
export GOWORK=off
gofmt -l $(find . -name '*.go' -not -path '*/testdata/*' -not -name '*.kyse.go')
go build ./...
go vet ./...
go test -race ./...
```

`GOWORK=off` is required: this checkout may sit beside a Go workspace that does
not list it.

## What this repository holds

| file | role |
| --- | --- |
| `whisper.go` | `Audio`, `Transcript`, `Segment`, `Transcriber`, `Transcribe`, the errors and `ProcessError` |
| `config.go` | `Config`, its defaults, `New` and the validation of every path |
| `resident.go` | `Config.ServerURL`: the recognition goes to a resident whisper.cpp `whisper-server` (`/inference`, `verbose_json`) |
| `handler.go` | `Handler`: `POST` audio, JSON transcript, bearer tokens per client, body and concurrency limits, the wire error codes |
| `remote.go` | `Remote`, `ParseNodes`, `Health`: a `Transcriber` over several nodes with failover and a cooldown |
| `fake.go` | `Fake`, the scripted `Transcriber` for an application's tests |
| `tests/Unit/service_test.go` | the handler, the remote (rotation, failover, cooldown, health), the resident server against stand-ins |
| `tests/Unit/whisper_test.go` | the client against stand-in `ffprobe`, `ffmpeg` and `whisper-cli` scripts, against the real ffmpeg when present, against the real whisper.cpp when `WHISPER_CLI` and `WHISPER_MODEL` are set, and the fake |
| `tests/Unit/manifest_test.go` | `arandu.mod.toml` held against the imports |

## Rules

- **Programs are started with `exec.CommandContext`, never through a shell,
  with fixed arguments.** Anything that varies (the model, the language, the
  prompt, the paths) is one argument of its own, and `Config.Language` is
  validated as a code so it can never look like a flag.
- **Nothing the person said goes into an error, a log or a file that outlives
  the call.** whisper-cli prints the transcript on standard output: it is
  discarded, never captured. An error carries the exit status and a summarized,
  truncated standard error with the working directory replaced.
- **One private working directory per call**, under `Config.TempDir`, removed
  with `defer` on every path, including timeout and cancellation.
- **Limits are checked before the expensive step.** Size before writing,
  duration (ffprobe) before converting, converted length before whisper, and
  ffmpeg's own `-t` as the last cap.
- **The timeout kills the process.** `Config.Timeout` covers probe, conversion
  and whisper together; an elapsed deadline is `ErrTimeout`, a cancelled caller
  is `context.Canceled`.
- **The key facts about whisper-cli's contract live in `recognize` and
  `readOutput`**: `-oj -otxt -of <base> -np`, the JSON at `<base>.json` and the
  text at `<base>.txt`. Check them against the whisper.cpp release in use before
  changing either.
- **No framework import and no third-party dependency.** `go.mod` has no
  `require`. If one is ever needed, the `framework` floor in `arandu.mod.toml`
  moves with it and every installer is told in `UPGRADE.md`.
- **A test never needs whisper.cpp or a model.** It uses the stand-in scripts or
  the `Fake`; the one test that runs the real program skips itself with the
  reason.

## Commits

The identity is set in this checkout: `HYZIS <admin@hyz.is>`. No AI attribution
of any kind. Work goes to `develop`, and releases are tags on `main`.
