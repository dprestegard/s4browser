# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

s4browser is a Wails v3 desktop S3 client (Go backend, React/TypeScript frontend) for moving very large media files. [README.md](README.md) covers behaviour, install paths, known limits, and the build, dev-server, bindings-regeneration, notices and release steps. This file adds only what the README does not.

## Commands

`main.go` embeds `frontend/dist`, so build the frontend first (`cd frontend; npm ci; npm run build`) or `go vet`, `go test` and `go build` all fail.

CI (`.github/workflows/build.yml`) runs these, so run them before a PR:

```powershell
go vet ./...; go vet -tags devfake ./...
staticcheck ./...; staticcheck -tags devfake ./...
go test -race -short -count=1 ./...          # needs CGO_ENABLED=1 and a C compiler
cd frontend; npm test                         # vitest
go run ./tools/notices -check
```

- Single Go test: `go test -race -run TestName ./internal/engine`. Without `-short` the 32 GiB / 2 TiB engine runs and the 100,000-object listing are included.
- Single frontend test: `cd frontend; npx vitest run src/dropFlow.test.ts`.

## Architecture

Layers, bottom to top (each depends only downward):

- **`internal/engine`**: transfer contracts and scheduler: job/part state machines (state changes only through `NextJobState`/`NextPartState` in `state.go`), retry policy, memory bound, and the seams `ObjectStore`, `Source`, `Sink`, `Clock`. It must stay free of the AWS SDK and of OS-specific code; `GOOS=darwin go vet` must pass for it and `internal/journal`.
- **`internal/s3x`**: adapts the AWS SDK to `engine.ObjectStore`, plus the credential prober and remote cleanup. Its tests pin SDK behaviour the design relies on, so an SDK upgrade fails here first.
- **`internal/fsx`**: download write strategies, temp-file naming, free space, no-replace finalization, path rules. Windows code lives in `_windows.go` files; the rest builds everywhere.
- **`internal/journal`**: crash-safe record of multipart upload IDs and download temp files, cleaned up by `Recovery` at launch. No secrets: `Entry` is an allowlist, guarded by a test.
- **`internal/creds`**: profile discovery and refreshing credential providers. Credential errors are tagged so the engine goes to `credential-wait` instead of failing the job.
- **`internal/queue`**: the service between the bindings and the engine. It holds no job state; the engine owns jobs, and the service maps snapshots to `JobView` and batches progress events.
- **`internal/browse`, `internal/objects`**: Go owns every listing (the UI asks for sorted windows of rows, never the full list) and the non-transfer commands (delete, folders, presigned links, tags).
- **`internal/app`**: the Wails service bound to the frontend. File contents never reach the frontend; dropped paths are classified in Go.
- **`frontend/`**: React UI. It calls Go through the committed bindings in `frontend/bindings` and receives events through `src/wails.ts`.

Test-only: `internal/fakes3` (in-process S3 and HTTPS server that stores only part length, CRC32 and ETag, with data from a deterministic generator), `internal/faultsuite` (end-to-end fault tests, including kill and relaunch), `internal/buildcheck`. `cmd/s4engine` is a CLI over the same engine and `cmd/s4credtest` a `credential_process` helper.

[docs/engine-design.md](docs/engine-design.md) is normative for the engine: a contract change updates it in the same commit. Stage numbers in code comments ("stage 4.2, F7") refer to the old implementation plan, which was removed and is in git history.

## Things that bite

- **Build tags carry behaviour:** `devfake` (vet and staticcheck run with and without it), `production` (release builds), and `race`/`!race` (the memory-bound tests run only without the race detector).
- **`S4_WRITE_PREFIX`:** an invalid value refuses every write rather than disabling the guard. Test tooling must set `Config.Guard` and write only under `s4browser-test/<run-id>/`.
- **Two web engines:** the page runs in WebView2 and WKWebView. Platform-specific wording and layout key off `src/ui/platform.ts` and `html[data-platform=mac]`.
- Files are LF (`.gitattributes`). Sample media in the repo root is git-ignored; do not add it.
