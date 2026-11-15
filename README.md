<p align="center">
  <img src=".github/logo.svg" alt="Arandu" width="180">
</p>

# Whisper for Arandu

Speech to text on your own server. A voice note goes in, text comes out, and
nothing leaves the machine: it runs [whisper.cpp](https://github.com/ggml-org/whisper.cpp),
the open source port of OpenAI's Whisper, so there is no per-minute cost and no
audio sent to a third party.

It is a thin layer over three programs you install. `ffprobe` and `ffmpeg` read
whatever audio arrived (a WhatsApp voice note is Ogg Opus) and make a 16 kHz
mono WAV; `whisper-cli` from whisper.cpp reads that WAV. This package adds what
an application otherwise writes every time:

- a configuration refused at boot, so a missing binary or model fails at the
  first restart and not on the first voice note;
- limits on size, duration and time, with a timeout that kills the running
  process;
- a private temporary directory per call, removed on every path;
- errors that carry the exit status and a short summary, never what was said;
- the `Transcriber` interface, with a `Fake`, so the code that transcribes is
  tested without the programs or a model.

It owns no table, no route and no policy, and it does not import the framework:
an application on any framework version installs it. It has no third-party
dependency.

## Requirements

| what | notes |
| --- | --- |
| whisper.cpp | the `whisper-cli` program, built from a release |
| a ggml model | for example `ggml-small.bin`, see below |
| ffmpeg and ffprobe | any recent build; the two programs ship together |

## Install

```bash
go get github.com/hyz-is/arandu-whisper@<latest tag>   # gh release list -R hyz-is/arandu-whisper
```

### Get whisper.cpp

Build `whisper-cli` from a release of <https://github.com/ggml-org/whisper.cpp>:

```bash
git clone --depth 1 --branch v1.9.5 https://github.com/ggml-org/whisper.cpp
cd whisper.cpp
cmake -B build -DBUILD_SHARED_LIBS=OFF
cmake --build build -j --config Release
# the program: build/bin/whisper-cli
```

`-DBUILD_SHARED_LIBS=OFF` links the libraries into the program, so
`whisper-cli` is one file you can copy. The flags `-m`, `-f`, `-l`, `-oj`,
`-otxt`, `-of`, `-np`, `-t` and `--prompt`, which this package uses, are the
ones documented in whisper.cpp's command-line example when this package was
written; run `whisper-cli --help` and `go test -run TestRealWhisper` after
upgrading whisper.cpp.

### Get a model

Models are single files from the official repository on Hugging Face:

```bash
curl -L -o ggml-small.bin \
  https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-small.bin
```

Use the multilingual models (no `.en` in the name): the `.en` ones know only
English. For Brazilian Portuguese on a CPU:

| model | file | disk | memory | when |
| --- | --- | --- | --- | --- |
| `base` | `ggml-base.bin` | 142 MiB | about 390 MB | fastest usable; misses technical words and noisy rooms |
| `small` | `ggml-small.bin` | 466 MiB | about 850 MB | **the recommended start**: a good balance for voice notes |
| `small` quantized | `ggml-small-q5_1.bin` | about 180 MiB | less than `small` | nearly the quality of `small` in less memory |
| `medium` | `ggml-medium.bin` | 1.5 GiB | about 2.1 GB | clearly better on accents and noise, several times slower |

Disk and memory are the figures whisper.cpp publishes for each model. Speed
depends on the CPU, so measure it: transcribe a real two-minute note on the
server with `whisper-cli -m ggml-small.bin -f note.wav -l pt` and set `Timeout`
to a few times the time you see. Transcription is CPU bound and uses `Threads`
cores for its whole duration, so decide how many calls may run at once (a
buffered channel is enough) instead of letting every incoming message start one.

## Wire it

The paths come from the application's own configuration; this package never
reads the environment.

```go
// bootstrap/app.go
stt, err := whisper.New(whisper.Config{
    BinaryPath: cfg.Whisper.Binary, // WHISPER_CLI,   e.g. /usr/local/bin/whisper-cli
    ModelPath:  cfg.Whisper.Model,  // WHISPER_MODEL, e.g. /var/lib/whisper/ggml-small.bin
    // FFmpegPath: "ffmpeg",        // the default: looked up in PATH
    // Language:   "pt",            // the default
    Threads: 4,
    Prompt:  "cafeicultura, ferrugem, broca-do-café, adubação, talhão", // optional vocabulary
})
if err != nil {
    return App{}, err
}
assistant := services.NewAssistant(db, stt) // takes a whisper.Transcriber
```

Add `WHISPER_CLI` and `WHISPER_MODEL` to `.env.example`. There is no migration,
no route and no `k.Register` line.

## Use it

```go
tr, err := stt.Transcribe(ctx, whisper.Audio{Data: note, MIMEType: "audio/ogg; codecs=opus"})
switch {
case errors.Is(err, whisper.ErrEmptyAudio):       // no bytes, or no samples
case errors.Is(err, whisper.ErrTooLarge):         // over Config.MaxBytes
case errors.Is(err, whisper.ErrTooLong):          // over Config.MaxDuration
case errors.Is(err, whisper.ErrUnsupportedAudio): // ffmpeg could not read it
case errors.Is(err, whisper.ErrTimeout):          // Config.Timeout elapsed; the process was killed
case errors.Is(err, whisper.ErrFailed):           // whisper-cli failed: errors.As(err, &perr)
case err != nil:                                  // context cancelled, disk, bad output
}
log.Info("transcribed", "language", tr.Language, "seconds", tr.Duration.Seconds())
// tr.Text is what was said; tr.Segments has each stretch with its start and end.
```

Silence is not an error: it gives an empty `tr.Text`. Do not log `tr.Text`, the
audio or a `ProcessError`'s fields beyond what you need: they can carry personal
data. The package itself never logs.

Do it from a job and not from a request handler: a two-minute note can take a
while on a CPU.

## Test with it

```go
fake := whisper.NewFake(
    whisper.Answer{Text: "apareceu ferrugem no talhão três", Duration: 4 * time.Second},
    whisper.Answer{Err: whisper.ErrTooLong},
)
svc := services.NewAssistant(db, fake)
// ... exercise svc ...
fake.Requests()[0].MIMEType // what the service sent
```

## Config

| field | default | |
| --- | --- | --- |
| `BinaryPath` | required | `whisper-cli`: a path, or a bare name looked up in PATH |
| `ModelPath` | required | a ggml model file |
| `FFmpegPath` | `ffmpeg` in PATH | |
| `FFprobePath` | beside ffmpeg, then PATH | |
| `Language` | `pt` | a two or three letter code, or `auto` |
| `Prompt` | none | text that biases spelling toward a vocabulary |
| `Threads` | whisper's own (the smaller of 4 and the cores) | |
| `Timeout` | 3 minutes | probe, conversion and whisper together |
| `MaxDuration` | 5 minutes | longer audio is `ErrTooLong` |
| `MaxBytes` | 32 MiB | larger input is `ErrTooLarge` |
| `TempDir` | the system's | where each call makes its private directory |

## Docker

Compile whisper.cpp in a build stage and copy only the program. `GGML_NATIVE=OFF`
keeps the build from using the instruction set of the machine that built it, so
the image also runs on a different CPU. The model is not baked in: mount it as a
volume or download it at deploy time.

```dockerfile
FROM debian:bookworm-slim AS whisper
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential cmake git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
ARG WHISPER_CPP_REF=v1.9.5
RUN git clone --depth 1 --branch "$WHISPER_CPP_REF" https://github.com/ggml-org/whisper.cpp /src
WORKDIR /src
RUN cmake -B build -DBUILD_SHARED_LIBS=OFF -DGGML_NATIVE=OFF -DWHISPER_BUILD_TESTS=OFF \
    && cmake --build build -j --config Release --target whisper-cli

FROM golang:1.26 AS app
WORKDIR /app
COPY . .
RUN CGO_ENABLED=0 go build -o /out/app .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ffmpeg libgomp1 libstdc++6 ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=whisper /src/build/bin/whisper-cli /usr/local/bin/whisper-cli
COPY --from=app /out/app /usr/local/bin/app
ENV WHISPER_CLI=/usr/local/bin/whisper-cli \
    WHISPER_MODEL=/models/ggml-small.bin
# docker run -v /srv/models:/models:ro ...
CMD ["app"]
```

The `ffmpeg` package brings `ffprobe` with it.

## Run it against the real thing

The tests never need whisper.cpp. One of them runs the real program when you ask:

```bash
WHISPER_CLI=/usr/local/bin/whisper-cli WHISPER_MODEL=/models/ggml-small.bin \
  go test -run TestRealWhisper -v ./tests/Unit
# optionally: WHISPER_AUDIO=/path/to/a-recording-with-speech.ogg
```

Without those variables the test skips and says why.

## License

MIT. See [LICENSE.md](LICENSE.md).
