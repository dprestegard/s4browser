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
