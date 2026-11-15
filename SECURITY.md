# Security policy

## Supported versions

The latest minor release of Whisper receives security fixes. Older minor
releases do not: a published Go module version is immutable, so a fix is a new
release rather than a moved tag.

## Reporting a vulnerability

Report it privately, through GitHub's advisory form:

<https://github.com/hyz-is/arandu-whisper/security/advisories/new>

Do not open a public issue and do not describe the problem in a pull request.
A report that arrives in public is a report every reader of this repository can
act on before there is a release to upgrade to.

Include what you need to reproduce it: the version, the wiring, and the request
or call that triggers it.

Expect an acknowledgement within a few days. If the report is confirmed, the
fix, the release and the advisory are published together, and you are credited
unless you ask not to be.

## What is in scope

Anything that lets a caller do more than transcribe the audio it handed over.
In particular:

- a way to make the package run a program, or pass an option, other than the
  ones `Config` names, for example through the language, the prompt or the MIME
  type;
- a path from the audio, or from the model's output, to a file outside the
  call's private temporary directory;
- a working directory, an audio file or a transcript that survives the call, or
  appears in an error;
- a call that outlives its context or timeout, or leaves a running process
  behind.

## What is not

- A vulnerability in whisper.cpp, ffmpeg or a model. Report that to the project
  that owns it.
- A recording that is simply slow or large for the hardware it runs on, within
  the limits the application configured. Choosing `MaxBytes`, `MaxDuration`,
  `Timeout` and how many calls run at once is the application's.
