---
name: whisper-release
description: Get this Arandu package green, declared and published. Use when the request is to "run the gates", "check it builds", "the tests fail", "go build says the directory prefix does not contain modules listed in go.work", "aru doctor says this is not an Arandu project", "update arandu.mod.toml", "add a dependency", "bump the framework", "tag a release", "publish it", "go get cannot find the module", "write the changelog", or before opening a pull request. Covers the four gates and why each filter is there, what the manifest declares and who checks it, the immutability of a published module version, and what breaks somebody else's build.
license: MIT
---

# Getting it green and out

## The four gates

Nothing is finished until all four exit zero.

```sh
export GOWORK=off
gofmt -l $(find . -name '*.go' -not -path '*/testdata/*' -not -name '*.kyse.go')
go build ./...
go vet ./...
go test -race ./...
```

`GOWORK=off` first, and here it is not a habit. This checkout may sit beside a
Go workspace that lists the framework repositories and does not list this one.
When it does, `go build ./...` exits 1 before compiling anything:

```
pattern ./...: directory prefix . does not contain modules listed in go.work
or their selected dependencies
```

That is the workspace refusing to answer for a module it was not told about, and
it says nothing about the code. With the workspace off, the module resolves the
versions in `go.mod` — which is what CI compiles against, and what somebody's
`go get` will get. A pass obtained with a workspace active is a pass against
whatever happens to be checked out beside you.

`aru doctor` is not a gate. It exits 1 here with *"this is not an Arandu
project: no go.mod, main.go and arandu.toml together"* — it reads applications,
and this is a library.

The suite needs no whisper.cpp, model or ffmpeg. `TestRealFFmpegConvertsAnOpusVoiceNote`
uses the real ffmpeg and ffprobe when they are in PATH, and `TestRealWhisper`
runs the real program when `WHISPER_CLI` and `WHISPER_MODEL` are set; each skips
with its reason otherwise. Run both before a release on a machine that has them,
and say in the release notes that you did.

CI runs the same four, and so does the release workflow on the tagged archive.

## What the manifest declares, and who reads it

`arandu.mod.toml` is a promise, and the check that gives it weight runs here.
Nowhere else does: `aru doctor` audits the tree of the application it is run
inside and never loads a dependency, so the manifest of an installed package is
opened by nothing but that package's own suite.

```toml
name = "hyz-is/whisper"
framework = ">= 0.50"
profiles = ["conventional", "performance"]

[permissions]
network = false
filesystem = true
exec = true
migrations = false
```

`TestTheManifestMatchesWhatThePackageDoes`, in `tests/Unit/manifest_test.go`,
holds the four permissions and the package name against the declaration and
refuses an import of `net`, `net/http`, `database/sql` or the framework, which
would contradict `network = false`, `migrations = false` and the no-framework
rule. `aru doctor` audits the tree of an application and never loads an
installed package, so this suite is the only place the declaration is checked.

A call added here is a line changed in `arandu.mod.toml` in the same commit.
The `framework` line is only a floor for the installing application, since this
package imports nothing from it.

## Adding a dependency

Do not, without an argument. This module goes into other people's builds: a
second `require` is a download and an audit surface for every project that
installs the package, and the framework's own rule is a core with no third-party
dependency beyond `golang.org/x/crypto`.

`go.mod` has no `require` at all, and that is a property worth keeping:

```sh
export GOWORK=off
go list -m all   # prints only this module
```

If something genuinely has to come in, it goes with a note in the pull request
saying what it does that the framework does not, and the manifest changes in the
same commit if it can reach the network, the filesystem or another process.

## Publishing

A published Go module version is **immutable**. The proxy serves it forever, and
a mistake is corrected by another release, never by moving a tag. That is the
reason `CHANGELOG.md` says so and the reason an exported name is fixed once it has
shipped.

Before tagging:

- the four gates pass with `GOWORK=off`;
- `arandu.mod.toml` matches the code;
- `CHANGELOG.md` has the entry, under the version, in the Keep a Changelog
  sections — `Added`, `Changed`, `Fixed`, `Removed`.

While the version starts with `v0.`, breaking is allowed — freezing a shape this
early is worse. Breaking *quietly* is not. A field that stops existing is a
build that stops in somebody else's repository, weeks later, when they upgrade,
with an error about a struct literal they did not write. Say what it was, what
it is, and what they have to write instead.

Run `apidiff` against the release baseline and enumerate every removal; the CI
step named `api diff against the last release` fails unless `UPGRADE.md` names
each one.

Changes that break an installer without touching a signature, each worth a line
in the changelog:

- **A default.** `DefaultLanguage`, `DefaultTimeout`, `DefaultMaxDuration` and
  `DefaultMaxBytes` are what an application gets by leaving the field empty, so
  moving one moves behaviour in every project that never mentioned it.
- **The arguments passed to whisper-cli or ffmpeg**, or the file names whisper
  is expected to write. They are the contract with whisper.cpp; a change that
  needs a newer whisper.cpp raises the minimum release in the README.
- **An error that stops being, or starts being, a sentinel.** Applications branch
  on `errors.Is`.

## Reporting a vulnerability

Not in an issue and not in a pull request. `SECURITY.md` has the private
advisory address. Anything that lets a caller run another program or option,
reach a file outside the call's directory, leave audio or a transcript behind,
or put what was said in an error is in scope.
