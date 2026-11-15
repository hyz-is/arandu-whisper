# Contributing

Thank you for considering a contribution.

## Before you open a pull request

Run the four gates. They are what CI runs, and a pull request that fails one of
them fails for everybody who pulls after it.

```bash
gofmt -l $(find . -name '*.go' -not -path '*/testdata/*' -not -name '*.kyse.go')
go build ./...
go vet ./...
go test -race ./...
```

The two filters on `gofmt` are not preference. `gofmt` is the only tool in the
chain that ignores build tags, so it reads files the compiler deliberately
excludes; `testdata/` holds fixtures that are invalid on purpose.

The suite does not need whisper.cpp, a model or ffmpeg: it runs stand-in
programs it writes itself. If you have the real ones, also run

```bash
WHISPER_CLI=/path/to/whisper-cli WHISPER_MODEL=/path/to/ggml-small.bin \
  go test -run TestRealWhisper -v ./tests/Unit
```

## What a change has to keep true

A change that breaks one of these will not be merged, whatever else it improves.

1. **No shell.** Programs are started with `exec.CommandContext` and fixed
   arguments; nothing a caller supplies is ever joined into a command line.
2. **Nothing said leaks.** The transcript goes to standard output, which is
   discarded; errors carry only an exit status and a short, truncated summary of
   standard error. No log line, no error and no file outlives the call with
   content from the audio.
3. **The call cleans up.** Its private directory is removed on every path,
   timeout and cancellation included.
4. **`arandu.mod.toml` matches the code.** Adding a network call, a dependency
   on the framework or another program means declaring it there in the same
   commit; `tests/Unit/manifest_test.go` says when it does not.
5. **A test never needs the real program.** Use the stand-in scripts or the
   `Fake`.

## Style

Everything in the source is in English: identifiers, doc comments, internal
comments, error messages, log messages, and the names and messages of tests.
`pkg.go.dev` publishes the doc comments, and its readers are users of this
package.

Every exported symbol carries a doc comment saying what it does. The comment
documents the symbol and nothing else: no dates, no issue numbers, no
references to other repositories. Why a signature is what it is belongs there
when it is a fact about the code; anything else belongs in the pull request.

## Commits

One commit per logical change. Formatting is not mixed with behaviour, because
a diff that does both is a diff nobody reviews.

## Reporting a vulnerability

Do not open an issue. See [SECURITY.md](SECURITY.md).

## Licence

By contributing you agree that your contribution is licensed under the MIT
licence, the same as the rest of github.com/hyz-is/arandu-whisper.
