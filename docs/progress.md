# Progress

## Stage 0.1: toolchain, git and Wails scaffold

**Status:** done. `wails build` produces `build/bin/s4browser.exe`. `go test -race ./...`, `go vet ./...` and `staticcheck ./...` all pass with `CGO_ENABLED=1`.

### Installed versions

| Tool | Version | How |
|---|---|---|
| Go | go1.27.4 windows/amd64 | `winget install --id GoLang.Go --exact` |
| Node.js (LTS) | v26.13.1 | `winget install --id OpenJS.NodeJS.LTS --exact` |
| npm | 11.20.0 | bundled with Node |
| C toolchain (tests only) | gcc 16.2.0, MinGW-W64 UCRT posix seh (WinLibs r3) | `winget install --id BrechtSanders.WinLibs.POSIX.UCRT --exact` |
| Wails CLI | v2.17.1 | `go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.1` |
| staticcheck | 2026.2.2 (0.8.2) | `go install honnef.co/go/tools/cmd/staticcheck@latest` |
| Git | 2.51.0.windows.2 | already installed |

Scaffold dependencies, pinned by `frontend/package-lock.json`:

- React 19.3.1, `@vitejs/plugin-react` 5.2.0
- Vite 7.3.8, TypeScript 5.9.3

These are the versions the `react-ts` template resolves to. They were not forced to newer majors.

### `wails doctor`

Result: `SUCCESS  Your system is ready for Wails development!`

- Wails v2.17.1, Go go1.27.4, platform windows/amd64
- OS: Windows 11 Pro, build 26300
- WebView2 157.0.4341.46, Node 26.13.1, npm 11.20.0
- Optional and not installed: UPX, NSIS. Not needed per the plan.

### Deviations from the plan

- **Node install:** winget warned that the installer would request administrator rights. It completed without an error, and `node --version` returns v26.13.1.
- **PATH:** Go, Node and gcc were not on the PATH of an already-open shell after install. Every command below sets PATH explicitly.
- **`.gitignore`:** extended beyond the template's three entries. It now also covers env and credential files and the root-level `/*.mkv`. The repo root holds a 2 GB sample video, `Elemental_Foreman_2160p24_10Bit_422_H264Lossless.mkv`. It is ignored and must not be committed.
- **`internal/buildcheck/`:** this is the placeholder test the plan asked for. It runs a goroutine and mutex test so the race detector has something to instrument. It is not engine code.
- **Git identity:** `user.name` and `user.email` were already set in the global config, so nothing was changed.
- **`PLAN.md` and `docs/implementation-plan.md`:** copied from the scaffold step unchanged. Verified by SHA-256 before and after.
- **No AWS calls were made and `~/.aws` was not read.**

### Commands

Run these from `F:\s4browser` in PowerShell. Each command sets PATH itself, so it works in a fresh shell.

```powershell
$env:Path = "C:\Program Files\Go\bin;C:\Program Files\nodejs;<WinLibs install folder>\mingw64\bin;$env:USERPROFILE\go\bin;" + $env:Path
```

Build the app:

```powershell
wails build
```

Run the Go checks. `CGO_ENABLED=1` is required for `-race`:

```powershell
$env:CGO_ENABLED = "1"
go test -race -count=1 ./...
go vet ./...
staticcheck ./...
```

Dev run, for later stages:

```powershell
wails dev
```

### Next

Stage 0.2 (engine design, Opus) starts from this commit. It is not started here.

## Stage 0.2: engine design and contracts

**Status:** done. `docs/engine-design.md` is the normative design. The compiling contracts are in `internal/engine`, `internal/journal`, `internal/fsx` and `internal/s3x`. `go build`, `go test -race -count=1`, `go vet` and `staticcheck` all pass on `./...`. `GOOS=darwin go vet` passes for the engine, journal, fsx and s3x packages.

No AWS calls were made and `~/.aws` was not read. SDK behaviour was checked by reading the source and by tests against a local TLS server with fake keys.

### What later stages must know

- **The fake S3 must serve HTTPS.** Over plain HTTP the SDK reads every part twice and computes SHA-256, so throughput and metering would not match production.
- **Pass `s3x.SingleAttemptOptions` on every S3 call.** It is the per-request way to disable SDK retries.
- **Build credentials as `aws.NewCredentialsCache(s3x.MarkCredentialErrors(base))`.** Without the marker, provider failures look like network errors.
- **Change job and part state only through `NextJobState` and `NextPartState`.**
- **Memory tests must be tagged `//go:build !race`.**

### Plan changes

- **Dependencies added now:** `aws-sdk-go-v2` v1.48.0, `service/s3` v1.115.0, `credentials` v1.21.1 and `smithy-go` v1.28.9. `config` and `golang.org/x/time/rate` are added by the stages that use them.
- **Stage 0.4 also implements the file-backed `journal.Store`,** because uploads cannot start without it.
- **Failure budget:** a job fails from retryable errors only after 10 attempts on a part **and** 30 minutes without a finished part. This keeps a network outage from aborting a long transfer.
- **Part sizes are rounded up to 1 MiB.** This refines the rule in `PLAN.md`.
- **Small download temp files are not journaled.** Files of 64 MiB or less skip the journal, so a crash can leave a small `~s4-*.s4tmp` file behind.

### Open risks

- **Memory budget constants are estimates.** `MemPerConnection` and `MemBase` have not been measured. Stage 0.4 must measure them and update the constants and design together.
- **Real S3 is unverified until stage 0.7.** The trailing checksum, composite CRC32 response format, `MpuObjectSize` with composite checksums, and the error codes returned for expired credentials were checked only against AWS documentation and local servers.
- **Downloads have no per-part checksum.** Arbitrary ranged GETs return none, so integrity rests on TLS, length checks and `If-Match`.
- **Read size may limit upload speed.** `net/http` reads request bodies in 32 KiB pieces, about 20,000 `ReadAt` calls per second at 5 Gbps. Stage 0.7 should check this if the throughput target is missed.
- **Parked download attempts can idle long enough for S3 to close the connection.** This is handled as a normal retry, but stage 0.5 should choose a `MaxLead` that keeps it rare.
- **The `journal.lock` mechanism is not yet implemented.** Stage 0.4 must make sure a second app instance cannot abort the first instance's uploads.

### Next

Stage 0.3 (fake S3, Sonnet) starts from this commit. It is not started here.
