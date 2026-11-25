# Upgrade Guide

## Unreleased

## 0.2.1

Nothing to change. `Remote` now waits up to `BusyWait` (5 seconds) when every node is busy, where it used to fail at once; set `BusyWait: -1` to keep the old behaviour.

## 0.2.0

Nothing to change: every addition is optional and the API of 0.1.0 is intact.

- To run transcription as a service, build a `*Client` with `Config.ServerURL` (a resident `whisper-server`, which loads the model once) and mount `whisper.NewHandler` where the service listens.
- To use a service from an application, build `whisper.NewRemote` and pass it where the application passed the `*Client`: both are a `whisper.Transcriber`.
- The manifest now declares `network = true`. An application whose own manifest or policy forbids network use by its dependencies should be told.

## 0.1.0

The first release: nothing to upgrade from. Install it as the README says.
