# Progress

An append-only log of what each stage did and found. Entries are history: they describe the code and docs as they were at that commit. `PLAN.md` and `docs/mac-checklist.md`, named in older entries, were later merged into [implementation-plan.md](implementation-plan.md) and [roadmap.md](roadmap.md). For current behaviour see the [README](../README.md); for planned work see [roadmap.md](roadmap.md).

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

## Stage 0.3: fake S3 (`internal/fakes3`)

**Status:** done. `go build ./...`, `go test -race -count=1 ./...`, `go test -count=1 ./...`, `go vet ./...` and `staticcheck ./...` pass. `GOOS=darwin go vet` passes for engine, journal, fsx, s3x and fakes3. The real `aws-sdk-go-v2` client drives the tests, with `s3x.SingleAttemptOptions` on every call. No AWS calls were made and `~/.aws` was not read.

### What exists

| File | Contents |
|---|---|
| `server.go`, `xml.go` | HTTPS server on 127.0.0.1 with a generated ECDSA certificate, HTTP/1.1 only. All S3 operations and XML. |
| `core.go` | Metadata-only object model and the shared operations. Used by both the HTTPS handler and the in-process store. |
| `fault.go` | `Rule`, `Fault` constructors, request log types, attempt counters. |
| `store.go` | `Server.Store()` (in-process `engine.ObjectStore`), `GuardStore`, `Server.Cleaner()` (`journal.RemoteCleaner`), `GuardCleaner`. |
| `api.go` | `AddRule`, `Attempts`, `Requests`, `PutSynthetic`, `Object`, `ActiveUploads`, `VerifyObject`, `RefusedWrites`. |
| `pattern.go` | `Pattern` (SplitMix64, offset-addressable), `PatternReader`. |
| `source_sink.go` | Synthetic `engine.Source` and `engine.Sink`. |
| `clock.go` | `FakeClock` implementing `engine.Clock`. |
| `client.go` | `Server.S3Client()`: a real SDK client with the cert trusted, path-style, fake keys. |

### Usage

```go
s, _ := fakes3.New(fakes3.Options{Guard: guard})   // Buckets default to fakes3.DefaultBucket; Clock defaults to real time
defer s.Close()
c := s.S3Client()                                   // real SDK client over HTTPS
store := fakes3.GuardStore(s.Store(), guard)        // in-process ObjectStore, no sockets
s.AddRule(fakes3.Rule{Op: fakes3.OpUploadPart, Part: 2, Attempt: 1, Fault: fakes3.ResetAt(1 << 20)})
s.Attempts(fakes3.OpUploadPart, bucket, key, 2)     // proves a finished part was not re-sent
s.VerifyObject(bucket, key, seed, 64)               // uploaded parts match Pattern{seed}; checks every 64th part and the last
```

- **Synthetic data:** the word at byte `8*i` is `SplitMix64(seed+i)` little endian, where `SplitMix64(x)` finalizes `x + 0x9E3779B97F4A7C15` (`SplitMix64(0) = 0xe220a8397b1dcdaf`). `PutSynthetic` creates a download object of any size. `Source.Set` and `NewSink` do the same for the engine's source and sink.
- **Uploads store only `(length, CRC32, ETag)` per part.** The server computes the CRC32 of every body it receives and compares it with the client's header or trailer (`BadDigest` on mismatch). `VerifyObject` recomputes the expected CRC from the pattern. A successful verify binds the object to the seed, which is what makes ranged GET of an *uploaded* object possible. An unverified upload answers GET with 500 `InternalError` and a clear message.
- **Rules** match by `Op`, `Bucket`, `Key`, `KeyPrefix`, `Part`, `Attempt` (nth), `FromAttempt`, `RangeStart` and `Times`. The attempt counter is per (op, bucket, key, part), so results do not depend on how requests for other keys interleave. The first matching rule fires.
- **Faults:** `InternalError`, `ServiceUnavailable`, `SlowDown`, `ExpiredToken`, `InvalidAccessKeyID`, `ResetAt(n)` (real TCP RST via hijack and `SetLinger(0)`), `LostResponse()` (do the work, then reset), `StallAt(n, d)` (`d` = 0 stalls until the request ends), `SlowResponse(d)`, `SlowBody(bytesPerSec)`, `TruncateAt(n)`, `ChangeObject()` (new ETag, so `If-Match` gets 412), `.ErrorIn200()`, `.WithRetryAfter(d)`, `.Afterwards()`. Delays use `Options.Clock`, so a `FakeClock` controls them. `FakeClock.WaitPending` lets a test advance time only once the server is waiting.
- **Guard:** `Options.Guard` makes the server answer 403 `AccessDenied` for refused writes before any state or rule counter is touched, and counts them in `RefusedWrites`. `GuardStore` and `GuardCleaner` are the engine-side decorators from design section 12. Tests prove a refused write leaves no object, upload or part, and that the wrapped store saw zero requests.
- **Stage 0.4 simulated run:** use `s.Store()` with `Source` and `Sink`. The 2 TiB test in `bigsim_test.go` (tagged `!race`, skipped by `-short`) uploads 9,987 parts of 210 MiB with 32 concurrent workers in about 20 s on a 16-core machine. Heap growth was 29 MiB (limit asserted: 48 MiB). Every part was sent exactly once. It then HEADs and range-reads the end of the object over HTTPS with the SDK.

### Known differences from real S3

- **No signature verification.** Any `Authorization` header is accepted. The access key ID is parsed and logged. Credential failures exist only as rules.
- **ETags are not MD5 of the data.** A part ETag is the MD5 of its CRC32 and length. The multipart ETag follows S3's formula (MD5 of the part digests, `-N`) over those values. Nothing may compare ETags with a local MD5.
- **CRC32 only.** Other checksum algorithms are rejected on create, and ignored on parts. Part checksums are `FULL_OBJECT`-style on responses, composite on the completed object.
- **Path-style addressing only, HTTP/1.1 only, no CopyObject, no versioning, no encryption headers, no object metadata or tagging.** Buckets are fixed at construction (`AddBucket`).
- **Synthetic objects have no HEAD checksum,** because computing one would read all of it.
- **Timing is not S3's.** Real S3 may hold a Complete for minutes and send keep-alive whitespace. The fake answers at once unless a rule delays it.
- **ListMultipartUploads ignores `delimiter`.** The listing has no `NextUploadIdMarker` paging subtleties beyond key and ID order.
- **Request log is capped at 1,048,576 entries.** Attempt counters are not capped.

### Open risks

- **Error codes unverified against real S3.** These were taken from memory of AWS behavior and documentation, not from a live account: `InvalidPart` for a part-checksum mismatch on Complete, `InvalidRequest` for an `x-amz-mp-object-size` mismatch and for a missing part checksum on Complete (message copied from S3's wording), `BadDigest` for a CRC32 mismatch on a part. Stage 0.7 should confirm them with a 10 GB real run.
- **HEAD errors carry no code.** Like S3, the fake sends no body on a HEAD error, so the SDK reports an expired token as `BadRequest` with status 400, which `ClassifyResponse` treats as permanent. This is the real behavior. The stage 0.4 `s3x` adapter should map HEAD 400/403 to a credential class, or the lost-Complete recovery path and download planning will fail a job instead of waiting for credentials. **This is a contract question for the design owner.**
- **The 2 TiB test depends on core count.** It takes about 20 s on 16 cores (7 GB/s per core for generate plus CRC). A 4-core machine needs about 80 s. Use `-short` to skip it.
- **The heap measurement includes uncollected garbage.** The assertion is deliberately loose (48 MiB for a 2 TiB object). Stage 0.4 should assert its own bound as the design describes.
- **A stalled request with unread body is only released when the server closes,** because Go's HTTP server does not notice a client disconnect until the body is read. This matters only for goroutine-leak checks in tests that stall uploads: call `Server.Close` before the leak check.

### Next

Stage 0.4 starts from this commit. It is not started here.

## Stage 0.4: scheduler, limiter and upload path

**Status:** done. `go build ./...`, `go test -count=1 ./...`, `go test -race -count=1 ./...`, `go vet ./...` and `staticcheck ./...` pass. `GOOS=darwin go vet` passes for engine, journal, fsx, s3x and fakes3. No AWS calls were made and `~/.aws` was not read.

### What exists

| Area | Files |
|---|---|
| Scheduler, commands, shutdown | `internal/engine/scheduler.go`, `job.go` |
| Upload path, retries, cleanup | `internal/engine/upload.go` |
| Credential wait and probes | `internal/engine/credentials.go` |
| Metered body, watchdog, limiter, progress | `internal/engine/body.go`, `watchdog.go`, `limiter.go`, `progress.go` |
| Production clock and file source | `internal/engine/clock.go`, `filesource.go` |
| `engine.ObjectStore` adapter and tuned transport | `internal/s3x/store.go`, `transport.go` |
| File journal and process lock | `internal/journal/filestore.go`, `lock_windows.go`, `lock_unix.go` |

### Measured results

Measured on the 16-core, 32-thread development machine with Go 1.27.4.

| Check | Result |
|---|---|
| 2 TiB upload, no race detector | Passed. 9,987 parts of 210 MiB in 30 s, plus 19 s to verify |
| 2 TiB upload, race detector | **Passed.** Upload 16 min 52 s, verify 11 min 58 s, 1,734 s (28 min 54 s) wall clock |
| Parts sent on the clean 2 TiB runs | Every part exactly once. The CRC32 of all 9,987 parts was recomputed from the pattern |
| Peak heap growth during the 2 TiB run | 10.2 MiB, limit 130.3 MiB (2 x `MemoryBound`) |
| Peak heap, 128 GiB object | 2.9 MiB with 64 MiB parts, 0.3 MiB with 1 GiB parts |
| Deliberate bugs injected to test the tests | 15 of 15 caught |

The 2 TiB memory figure uses the in-process store, so it has no TLS connections in it. Connection memory was measured separately, as the table in design section 4 shows.

An ordinary `go test -race` uploads 32 GiB in this test. To repeat the full run:

```powershell
$env:S4_RACE_FULL_2TIB = "1"
go test -race -count=1 -timeout 0 -v -run '^TestSimulated2TiBUpload$' ./internal/engine
```

### Contract changes

- **HEAD errors.** A bare HEAD 400, 401 or 403 is now resolved by a one-byte ranged GET, whose error body carries the real code. Expired credentials wait, `AccessDenied` fails the job, and an unresolved case is retryable, so the retry budget ends it. See design section 6. New: `engine.IsCodelessAuthFailure`, `engine.CodeAmbiguousAuth`.
- **Memory constants.** `MemBase` went from 16 MiB to 8 MiB and `MemPerConnection` from 384 KiB to 256 KiB. The default bound is now about 55 MiB.
- **Broken responses.** A 2xx status with no error code means the connection failed while the response was read. It is retryable. The HTTPS tests found this bug.
- **Create runs detached from a user cancel,** so the new upload's ID is known and it can be aborted.
- **A Create with no response** leaves a separate `creating` journal entry, `<job>-<run>-c<attempt>`, for launch cleanup.
- **Single PUT verifies the source afterwards.**
- **New sentinels:** `ErrInvalidConfig`, and `ErrDownloadUnsupported`, which stage 0.5 removes. New constructors: `engine.New`, `engine.NewBandwidthLimiter`, `journal.OpenFile`, `s3x.NewStore`.
- **Dependencies:** `golang.org/x/time` v0.16.0 and `golang.org/x/sys` are now direct.

### Open risks

- **Real S3 is still unverified until stage 0.7.** That includes the diagnostic GET, the error codes and the composite checksum format.
- **Orphaned uploads wait for stage 2.1.** A lost Create response leaves an upload on S3 that only `journal.Recoverer` can remove, and that does not exist yet.
- **Stage 2.1 must guard intent matching.** `MatchesIntent` matches by key and time, so launch cleanup could abort a new job's live upload to the same key within the one-hour window.
- **No production `CredentialProber` exists yet.** Stage 2.1 owns it. Pause, resume and credential recovery work and are tested here, but kill-and-relaunch is not covered.
- **Part of `MemBase` is unmeasured.** The idle figure is 0.21 MiB. The rest is headroom for the request built by Complete at 10,000 parts.
- **`GetRangeRequest` has no size field,** so the adapter cannot compare the `Content-Range` total. Stage 0.5 should add it.
- **Shutdown tries each abort once.** A failed abort stays in the journal for next launch.
- **A store that ignores cancellation blocks `Close`.** `Close` then returns the caller's context error and that goroutine stays alive.
- **`TestLiveBandwidthChange` uses real time.** Its margins are wide, but a heavily loaded machine could still fail it.

### Next

Stage 0.5 (download path and NTFS write strategies) starts from this commit. It is not started here.

## Stage 0.5: download path and NTFS write strategies

**Status:** done. `go build ./...`, `go test -race -count=1 ./...`, `go test -count=1 ./...`, `go vet ./...` and `staticcheck ./...` pass. `GOOS=darwin go vet` passes for engine, journal, fsx, s3x and fakes3. No AWS calls were made and `~/.aws` was not read. The 300 GiB comparison ran on this machine's F: drive. `F:\s4browser-bench\` is left empty, because the tools refused to delete the empty directory itself (see "Left for you").

### What exists

| Area | Files |
|---|---|
| Download path: prepare, journal, sink, parts, lead gate, commit, cleanup | `internal/engine/download.go`, small hooks in `upload.go`, `job.go`, `scheduler.go` |
| Production `engine.Sink` (free space, collisions, temp file, commit) | `internal/engine/filesink.go` |
| Write strategies, finalizer, free space | `internal/fsx/portable.go`, `strategy_windows.go`, `strategy_other.go`, `rename_link.go` |
| Benchmark | `internal/engine/dlbench_test.go` (+ `_windows`, `_other`) |
| Tests | `download_test.go`, `bigdl_test.go`, `internal/fsx/strategy_test.go`, `strategy_windows_test.go` |

`ErrDownloadUnsupported` is gone. `engine.StatsProvider` (not part of `Engine`) exposes the lead gate counters.

### Contract changes

- **`GetRangeRequest.Size`**, the one change the stage was allowed: the adapter compares it with the `Content-Range` total. A different total is `ErrObjectChanged`. A request without a size is `ErrInvalidSize`. `s3x` and `fakes3` both enforce it. Design section 8 is updated.
- Additive, nothing existing changed: `fsx.Finisher`, `fsx.Leader`, `fsx.NewStrategy`, `fsx.DefaultStrategy`, `fsx.NewFinalizer`, `fsx.FreeSpace`, `fsx.ErrUnsupportedStrategy`, `engine.NewFileSink`, `engine.FreeSpaceReserve`, `engine.FormatBytes`.
- Design section 8 now records the step order, the accounting check before commit, the lead gate details and the cleanup rules.

### Behaviour worth knowing

- **Writes inside the lowest unfinished part are always allowed through the lead gate.** Without that rule, a lead smaller than a part parks the one part that must finish first, and the job hangs. A test pins it, and it hangs when the rule is removed.
- **A parking attempt nudges the scheduler.** Parking is not a scheduler event, so without the nudge a lowest part waiting for a slot would never trigger preemption. Removing preemption makes `TestLeadGateCompletesWithoutDeadlock` hang.
- **A disk write parks the attempt,** so a 10-second drive stall is not taken for a dead connection. All three 300 GiB runs finished with zero retries, although the worst stall was 9.7 s.
- **Before commit, the engine requires `DoneBytes == TotalBytes`.** A mismatch fails the job instead of committing a file that may have a hole.
- **`Sink` and commit errors are `ErrLocalIO`** (permanent), with `ErrInsufficientSpace` and `ErrDestinationExists` still matchable. A disk error is never retried.
- **If the volume has no sparse files** (FAT, exFAT, some shares), the sparse strategy falls back to a preallocated file whose `fsx.Leader` reports `DefaultLead`, so the gate still bounds the zero-fill. Tested by injecting the failure.
- **Clearing the sparse attribute at commit is best effort.** A failure leaves a complete file that keeps the attribute, and does not fail the job.
- **Commit cannot be interrupted** once it is syncing, so `Close` waits for it (2 to 6 seconds at 300 GiB here). A user cancel is deferred during finalizing, as for Complete.
- `FileSink.Prepare` requires an absolute destination. Validation of the path itself is stage 2.3.

### Strategy comparison

**Method.** `TestDownloadWriteBenchmark` runs the real engine (scheduler, lead gate, file sink) against the in-process fake S3. The fake generates data far faster than 5 Gbps, so the disk is the bottleneck, and the writes arrive as the scheduler produces them: 16 parts of 64 MiB of one file in flight, each at its own offset, in 1 MiB writes. It records every write's latency, the longest gap with no write completing anywhere (worst stall), per-second throughput, heap and peak working set every 100 ms, and the time of Create, clearing the sparse attribute, Sync and Commit. It then verifies content: the whole file up to 32 GiB, and 2,000 random MiB plus both ends above that. It refuses any directory outside `F:\s4browser-bench` and removes only the files it created, including after a failure.

```powershell
$env:S4_BENCH_DIR = 'F:\s4browser-bench'; $env:S4_BENCH_SIZE = '16GiB'; $env:S4_BENCH_STRATEGY = 'sparse'
$env:S4_BENCH_LEAD = '2GiB'          # only for sequential-lead and preallocate
$env:S4_BENCH_MINFREE = '350GiB'     # for the full-size runs
$env:S4_BENCH_RESULTS = "$env:TEMP\bench.jsonl"   # one JSON line per run
go test -count=1 -v -run TestDownloadWriteBenchmark ./internal/engine
```

Also `S4_BENCH_RATE` (feed limit in bytes per second), `S4_BENCH_PART`, `S4_BENCH_FILES` and `S4_BENCH_PARTS`. The raw result lines of this stage are in the shared notes folder, in `stage05-full-300GiB.jsonl` and `stage05-small-runs.jsonl`.

**Machine.** Ryzen 9 9950X3D, 93 GB RAM, F: is a T-FORCE TM8FFE512G (512 GB NVMe, NTFS). The process is not elevated, so `valid-data` could not be tried: `SeManageVolumePrivilege` is not in its token, and the code never raises privileges. It is implemented, and tested to fail cleanly.

**Full size: 300 GiB (322 GB), one run each, unpaced feed.** The runs went in this order and took 8 to 16 minutes each, with more than 350 GiB free before each.

| | sparse | sequential-lead (2 GiB) | preallocate (2 GiB) |
|---|---|---|---|
| Sustained throughput, whole run | **618 MiB/s (5.2 Gbps)** | 334 MiB/s (2.8 Gbps) | 333 MiB/s (2.8 Gbps) |
| First third / last third of the run's time | 1,293 / 275 MiB/s | 581 / 209 MiB/s | 584 / 207 MiB/s |
| Slowest 10 s window | 7 MiB/s | 121 MiB/s | 122 MiB/s |
| Worst stall (no write completes anywhere) | 9.7 s | 1.6 s | 1.8 s |
| Longest single write | 9.8 s | 2.6 s | 2.2 s |
| p50 / p99 / p99.9 write latency | 6 / 210 / 1,913 ms | 5 / 540 / 913 ms | 5 / 547 / 1,093 ms |
| Peak Go heap growth | 32 MiB | 27 MiB | 26 MiB |
| Peak working set | 48 MiB | 46 MiB | 45 MiB |
| Time to finalize (commit) | 5.7 s (clear attribute 4.9 s, sync 0.8 s) | 3.6 s (sync) | 2.3 s (sync) |
| Extents of the finished file | 255 | 6 | 1 |
| Retries, parks | 0, 0 | 0, 0 | 0, 0 |

**Smaller runs on the same drive.** The whole file fits in the RAM cache, so the drive is not the limit:

| Run | sparse | sequential-lead | preallocate |
|---|---|---|---|
| 16 GiB unpaced, lead 1 GiB | 2,302 MiB/s, p99 19 ms | 1,304 MiB/s, p99 205 ms | 1,274 MiB/s, p99 247 ms |
| 64 GiB unpaced, lead 2 GiB | 2,537 MiB/s, p99 31 ms | 1,243 MiB/s, p99 123 ms | 1,228 MiB/s, p99 121 ms |
| 24 GiB paced at 5 Gbps (625 MB/s) | **598 MiB/s**, p99 0.6 ms, worst stall 24 ms | 546 MiB/s, p99 145 ms, stall 247 ms | 539 MiB/s, p99 129 ms, stall 206 ms |

At a 5 Gbps feed (596 MiB/s), only sparse kept up. The other two fell about 9% short even with the drive idle.

**Why.** NTFS zero-fills the gap below a write that starts past the valid data length, and that is the first write of every part when parts run side by side. The gap-filling strategies therefore write up to a part's worth of zeros per part. Preallocating the end of file does not help, which matches the rule: it moves the end of file, not the valid data length, and the two strategies measured the same. A sparse file has no clusters in the gap, so nothing is zero-filled. A unit test shows it: 1 MiB written 255 MiB into a sparse file allocates under 8 MiB.

**Choice.** **`sparse` is the default (`fsx.DefaultStrategy`).** It is 1.85 times faster over 300 GiB and 1.8 to 2 times faster at cache speed, it is the only one that held 5 Gbps in the paced run, and it needs no lead, so a download never parks a connection.

**Limits of this measurement. Read these before trusting the numbers.**

1. **This SSD runs out of fast cache.** The first third of the sparse run's time carried about 70% of the data. The last third ran at 275 MiB/s (2.2 Gbps), well under the 5 Gbps goal, and the other two at about 208 MiB/s. Sparse's whole-run 5.2 Gbps is the average of a fast start and a slow end. A 300 GB download to this drive will not sustain 5 Gbps with any write strategy. That is the drive. Stage 0.7 should repeat the comparison on the NVMe drive the media really lives on, which may be a different model.
2. **The worst stall is larger for sparse (9.7 s against 1.6 s).** It happened in the slow phase, when the drive flushed its cache, after sparse had pushed about twice as much data into it. Sparse's p99 write latency is better than the others' (210 ms against 540 ms), but a user could see a ten-second pause. The watchdog does not fire for it, because a write parks the attempt.
3. **One run per strategy, in a fixed order** (sparse first), to limit wear on the SSD. Drive state differs between runs: garbage collection and wear levelling, and the file system after 300 GiB was deleted. I did not repeat runs or randomise the order. The 1.85 times gap is much larger than the spread between near-identical small runs (the three lead settings of one strategy at 16 GiB differ by under 5%), but it is not a confidence interval.
4. **The feed is in-process.** No TLS, no network stack and no S3 timing, so the CPU is free for the disk, and all parts progress at the same speed. Real parts finish more unevenly, which spreads the writes further. That should hurt the gap-filling strategies more, not less.
5. **Process memory only.** The kernel's file cache (the dirty pages of a 300 GiB file) is not counted in the 45 to 48 MiB working set, and this machine has 93 GB of it. A machine with little RAM would flush earlier and stall sooner.
6. **Read-back verification of the 300 GiB files was sampled** (2,000 random MiB and both ends), not complete. Files up to 32 GiB were verified in full, and so was every strategy in the unit tests.

### MaxLead

`sparse` needs none. For the fallback strategies the default is `fsx.DefaultLead` = **2 GiB**, chosen from the park counts (in-process feed, 16 parts of 64 MiB, so a 1 GiB spread):

| Lead | Parks in 16 GiB | Longest park |
|---|---|---|
| 256 MiB | 212 to 215 | 99 to 132 ms |
| 1 GiB | 25 to 28 | 7 to 15 ms |
| 2 GiB and 4 GiB | 0 | none |

Two GiB covers the default spread with room for 128 MiB parts. With larger parts (210 MiB at 2 TiB, so 3.4 GiB for 16 parts) the gate does park. The 2 TiB simulated download with a 3 GiB lead had 12 parks of at most 67 ms. **Not measured:** how long S3 lets a connection sit unread before it closes it, because there is no real S3 until stage 0.7. A parked attempt that loses its connection simply retries and resumes where it stopped, so it costs a request, not data. If stage 0.7 finds long parks, size the lead from `MaxPartsPerFile * PartSize` in the wiring instead of changing the gate.

### Tests

Against `internal/fakes3`. All pass under `-race`, and each ends by asserting that no goroutines leaked.

| Requirement | Test |
|---|---|
| 2 TiB download, verified content, bounded memory, synthetic sink | `TestSimulated2TiBDownload`: 9,987 parts, every byte checked against the pattern and written once, heap growth 42 MiB against a 105 MiB limit, about 2 min on 16 cores. A `-race` run moves 32 GiB. `S4_RACE_FULL_2TIB=1` with `-timeout 0` moves the full 2 TiB |
| Each fault type behaves as designed | `TestDownloadFaults`: 500, 503, SlowDown with Retry-After, reset mid-body, truncated body, stall (watchdog), slow response. `TestDownloadWaitsForCredentialsAndResumes` for an expired token. `TestRetryBudgetEndsDownload` |
| Changed ETag fails clearly, no final file | `TestChangedETagFailsAndLeavesNoFile` (real files, directory empty afterwards), `TestChangedSizeIsObjectChanged`, and the `Content-Range` total case in `s3x` |
| Finished ranges are not fetched again | Every faulted part is requested twice, and the second request starts inside the part and ends at its end. Every byte is written exactly once |
| Cancel leaves no temp file | `TestCancelLeavesNoTempFile` |
| Journal entry before the temp file exists | `TestTempFileIsJournaledBeforeItExists` |
| Free space, collisions | `TestInsufficientSpaceFailsClearly`, `TestExistingDestinationIsNeverReplaced`, `TestDestinationAppearingBeforeCommitIsKept` |
| Gate, pause, bandwidth, progress | `TestLeadGate*`, `TestLeadSmallerThanAPartCannotStopTheLowestPart`, `TestPauseAndResumeDownload`, `TestDownloadHonoursBandwidthLimit`, `TestDownloadProgressIsBatched` |
| Real files | `TestDownloadFileStrategies`: each strategy, byte for byte |

**Tests of the tests.** Seven deliberate bugs were each caught: ignoring partial progress, dropping `If-Match`, skipping the temp-file abort, removing preemption, ignoring the lowest part in the gate, removing the free-space check, and removing the `Content-Range` total check in `s3x`.

### Open risks

- **Real S3 is still unverified until stage 0.7,** now including ranged GET behaviour: the `Content-Range` format, idle connection closing while parked, and `If-Match` with multipart ETags.
- **This SSD cannot sustain 5 Gbps for a 300 GB write** (limit 1 above). The target of stage 0.7 needs the real media drive, and a download to a slower drive will be disk-bound.
- **A sparse download leaves up to 255 extents** at 300 GiB. That is harmless for reading here. It was not tried with a defragmenter or a backup tool.
- **`journal.Recoverer` does not exist yet** (stage 2.1), so a crash during a large download leaves a temp file and a `download-temp` entry that nothing cleans up.
- **Small downloads are not journaled,** as designed, so a crash can leave a small `~s4-*.s4tmp` file.
- **Commit blocks `Close`** while it syncs.
- `engine.StatsProvider` counters are cumulative and never reset.

### Left for you

- **`F:\s4browser-bench\` still exists and is empty.** The benchmark removed everything it created, and the directory was empty at the end. My tools refused to remove the directory itself. Delete it with `rmdir F:\s4browser-bench`. Nothing else on F: was touched, and the sample `.mkv` was not used.

### Next

Stage 0.6 (`s4engine` CLI and file-drop window) starts from this commit. It is not started here. Use `engine.NewFileSink(fsx.DefaultStrategy())` for downloads.

## Stage 0.6: `s4engine` CLI and file-drop window

**Status:** done, with one check not done: **the real Explorer drag has not been performed** (see "Manual drop test"). `wails build`, `go build ./...`, `go test -race -count=1 ./...`, `go test -count=1 ./...`, `go vet ./...` and `staticcheck ./...` pass. `GOOS=darwin go vet` passes for engine, journal, fsx, s3x, fakes3 and `cmd/...`. No AWS calls were made and `~/.aws` was not read: credential tests use temporary `AWS_CONFIG_FILE` and `AWS_SHARED_CREDENTIALS_FILE` files with fake values. No engine contract changed.

### What exists

| Area | Files |
|---|---|
| CLI entry, command dispatch, exit codes | `cmd/s4engine/main.go`, `run.go` |
| Target (fake or S3), credentials, guarded store and admin calls | `cmd/s4engine/target.go` |
| upload and download, live progress, result file | `cmd/s4engine/transfer.go`, `metrics.go`, `mem_windows.go`, `mem_other.go` |
| verify, generate, cleanup | `cmd/s4engine/tools.go` |
| Size and rate parsing | `cmd/s4engine/units.go` |
| CLI tests | `cmd/s4engine/cli_test.go`, `credentials_test.go` |
| File-drop spike | `app.go`, `filedrop.go`, `filedrop_test.go`, `main.go`, `frontend/src/App.tsx`, `App.css` |

New dependency: `github.com/aws/aws-sdk-go-v2/config` (`credentials` moved to v1.21.2). The template's logo and Greet screen are gone.

### CLI usage

Build it so the binary carries the commit hash (`go run` and `go test` builds report `unknown`):

```powershell
go build -o s4engine.exe ./cmd/s4engine
```

| Command | Purpose |
|---|---|
| `upload [flags] FILE...` | Upload files to `s4browser-test/<run-id>/<file name>`, or to `--key` for a single file |
| `download [flags] --dest-dir DIR KEY...` | Download objects under the run prefix to `DIR\<last key segment>`. An existing file is never replaced |
| `verify [--result F] A B` | SHA-256 of two files, exit 0 only if equal |
| `generate --out F --size N [--seed S]` | Deterministic file (the `internal/fakes3` pattern), refuses to overwrite, prints its SHA-256 |
| `cleanup [--dry-run] --run-id ID ...` | Abort multipart uploads and delete objects under `s4browser-test/<run-id>/` |

Flags may come before or after the arguments.

| Flag | Meaning |
|---|---|
| `--fake` | Start the TLS fake S3 inside the process (the real `s3x` adapter talks to it over HTTPS) |
| `--profile P --region R --bucket B` | Real S3. Credentials come from the SDK config loader, so static keys and `credential_process` work. They are wrapped as `aws.NewCredentialsCache(s3x.MarkCredentialErrors(base))` |
| `--run-id ID` | Names the only prefix the run may touch. Generated when absent (printed in the summary). Required for `cleanup` against S3 |
| `--key K` | Upload only: destination for a single file. It must pass the guard |
| `--parallel-files N`, `--parallel-parts N`, `--total-parts N` | `MaxActiveFiles`, `MaxPartsPerFile`, `MaxActiveParts`. Engine defaults 4, 16, 32. `--total-parts` defaults to the larger of 32 and `--parallel-parts` |
| `--part-size 64MiB` | Preferred part size. Default is the engine default, 64 MiB |
| `--bandwidth 5Gbit` | Cap in bits per second (`Kbit`, `Mbit`, `Gbit`) or bytes per second (`50MiB`, `625MB`). Default unlimited |
| `--write-strategy`, `--write-lead` | Download strategy (`sparse` default = `fsx.DefaultStrategy()`, `sequential-lead`, `preallocate`, `valid-data`) and its lead |
| `--result F` | JSON result file. Default `s4engine-<command>-<run-id>.json` in the current directory |
| `--progress 1s` | Interval of the live progress lines (stderr) |
| `--journal-dir D` | Default is a temporary directory that is removed when the journal is empty at exit |
| `--seed N` | `--fake` only. Upload: after completing, the fake recomputes every part's CRC from the pattern (`fakeVerified` in the result). Download: seed of the generated object |
| `--fake-size N` | `--fake` download only: create a synthetic object of that size at each key before downloading |

Sizes accept `5368709120`, `64MiB`, `10GB`, `2TiB`.

**Exit codes:** 0 success, 1 transfer or verification failure (including a mismatch), 2 usage error or a refused destination, 130 interrupted.

**Ctrl+C** cancels every job, which aborts the multipart uploads, waits up to 30 s for the aborts, writes the result (`outcome: cancelled`) and exits 130. A second Ctrl+C ends the process immediately. This was checked with a real `CTRL_BREAK_EVENT` sent to the built exe (Go treats it like Ctrl+C): exit 130, result `cancelled`, journal directory removed. If an abort fails, the journal directory is kept, a warning names it, and `cleanup` finishes the job.

### The write guard

- Every run, fake or real, has an `engine.TestPrefixGuard` for `s4browser-test/<run-id>/`. There is no flag, environment variable or build tag that disables it. `TestNoFlagDisablesTheGuard` checks the help of every command for such an option.
- **Order of checks:** flags, then the run ID, then every destination key against the guard, and only then are credentials loaded and a connection made. A key outside the prefix ends the command with exit 2 and `refusing to start`. `TestGuardRefusalComesBeforeCredentials` proves the order with a profile that does not exist: a bad key gives exit 2, a good key reaches credential loading and gives exit 1.
- **During the run:** the engine has `Config.Guard`, and the store is wrapped with `fakes3.GuardStore`, which checks Create, UploadPart, Complete, Abort and Put. Cleanup goes through `guardedAdmin`, which checks every abort and delete, and refuses to list anything that is not exactly the run prefix.
- Downloads only read, but they are also limited to the prefix, to keep the tool inside one area.
- The fake server in `--fake` mode has the same guard and answers 403 for a refused write.

### Result file

One JSON file per transfer: command, outcome, error, commit (`+dirty` if the tree had changes), Go version, OS, start time, target (kind, bucket, region, profile name, run ID, prefix), settings (including the effective part size and strategy), per-file state, bytes, done bytes, parts and retries, total `bytes` completed, `durationSeconds`, `throughputBytesPerSec` and `throughputGbps` (whole run), `steadyStateBytesPerSec` (everything after the first 10 s, absent for runs of 10 s or less), `slowest10sWindowBytesPerSec` with its start time (absent below 10 s), `retries`, `peakHeapBytes` with its baseline, and `peakWorkingSetBytes`. It never holds keys, tokens or signatures, and a test checks it for the fake's key and secret.

**How the numbers are measured.** A poll every 200 ms sums `DoneBytes` of all jobs. Between polls, bytes are taken to arrive at a constant rate, and windows are slid over these samples. `DoneBytes` includes in-flight progress, so a failed part's bytes briefly count and then roll back. Heap is `/memory/classes/heap/objects:bytes` sampled every 100 ms after a forced GC at the start. Working set is Windows' own peak (`PeakWorkingSetSize`), read after the engine has stopped, so it covers the whole process. Duration runs from submit to the last job reaching a terminal state.

### Tests

All under `-race`, against `internal/fakes3`, in `cmd/s4engine`:

| Requirement | Test |
|---|---|
| Upload then download, hashes match, every part sent once, no temp file left | `TestUploadDownloadRoundTrip` (200 MiB + 12,345 bytes) |
| Standalone fake download of a seeded object matches `generate` | `TestDownloadFromGeneratedFakeObject` |
| Result file matches a transfer of known size, settings, no secrets | `TestResultFileMatchesKnownTransfer` |
| Cancel leaves no multipart upload and no object, exit 130 | `TestCancelAbortsMultipartUpload` |
| Transfer failure is exit 1 and leaves no file | `TestTransferFailureExitCode` |
| Guard refusal, before any request | `TestGuardRefusesDestinationOutsidePrefix`, `TestGuardRefusalComesBeforeCredentials`, `TestGuardedAdminRefusesBeforeTheNetwork`, `TestNoFlagDisablesTheGuard` |
| Cleanup stays inside the prefix (neighbours `c10/`, `c1`, `c1x` and other prefixes survive), dry run changes nothing | `TestCleanupStaysInsidePrefix` |
| Throughput, steady state, slowest window | `TestComputeThroughput` |
| Static keys, `credential_process` (the test binary is the process), a failing process is marked `ErrCredentialsUnavailable`, no secret in errors | `credentials_test.go` |
| Usage errors give exit 2, verify, generate | `TestUsageErrors`, `TestVerifyDetectsDifference`, `TestGenerateIsDeterministicAndNeverOverwrites`, `TestParseSizeAndRate` |
| Drop classification and log | `filedrop_test.go` |

### Stage 0.7 command sequence

Replace `<PROFILE>`, `<BUCKET>`, `<REGION>`. Run from `F:\s4browser` in PowerShell with the PATH from stage 0.1. Use a fixed run ID so cleanup can find the data later. The test data goes to a drive with at least 25 GB free.

```powershell
# 0. Build, so the commit hash is in the result files.
go build -o s4engine.exe ./cmd/s4engine
$run = "gate-$(Get-Date -Format yyyyMMdd-HHmm)"
New-Item -ItemType Directory -Force F:\s4gate\down | Out-Null

# 1. A 10 GB deterministic file. Note the SHA-256 it prints.
.\s4engine.exe generate --out F:\s4gate\10g.bin --size 10GB --seed 1

# 2. Safety check before any write: this must fail with exit 2 and make no request.
.\s4engine.exe upload --profile <PROFILE> --region <REGION> --bucket <BUCKET> --run-id $run --key other/x.bin F:\s4gate\10g.bin

# 3. What is already under the prefix (should be nothing).
.\s4engine.exe cleanup --profile <PROFILE> --region <REGION> --bucket <BUCKET> --run-id $run --dry-run

# 4. Upload with the defaults. Tune later with --parallel-parts, --total-parts, --part-size.
.\s4engine.exe upload --profile <PROFILE> --region <REGION> --bucket <BUCKET> --run-id $run --result F:\s4gate\up-default.json F:\s4gate\10g.bin

# 5. Download it back, then compare.
.\s4engine.exe download --profile <PROFILE> --region <REGION> --bucket <BUCKET> --run-id $run --dest-dir F:\s4gate\down --result F:\s4gate\down-default.json "s4browser-test/$run/10g.bin"
.\s4engine.exe verify F:\s4gate\10g.bin F:\s4gate\down\10g.bin

# 6. Ctrl+C test: start an upload, press Ctrl+C after about 20 s, then check that nothing is left.
.\s4engine.exe upload --profile <PROFILE> --region <REGION> --bucket <BUCKET> --run-id $run --key "s4browser-test/$run/interrupted.bin" --result F:\s4gate\up-ctrlc.json F:\s4gate\10g.bin
.\s4engine.exe cleanup --profile <PROFILE> --region <REGION> --bucket <BUCKET> --run-id $run --dry-run   # expect 0 multipart uploads

# 7. Repeat 4 and 5 with other settings, for example:
#    --parallel-parts 32 --total-parts 64 --part-size 128MiB
# Use another key (--key) for each upload, and a new --dest-dir for each download,
# because an existing local file is never replaced. To compare download strategies
# on the real media drive add --write-strategy sparse | sequential-lead | preallocate.

# 8. Remove everything under the prefix, then confirm the prefix is empty.
.\s4engine.exe cleanup --profile <PROFILE> --region <REGION> --bucket <BUCKET> --run-id $run
.\s4engine.exe cleanup --profile <PROFILE> --region <REGION> --bucket <BUCKET> --run-id $run --dry-run
```

Read the result files for the gate: `steadyStateBytesPerSec` against 80% of the measured line rate, `slowest10sWindowBytesPerSec` (a long collapse is a failure even when the average passes), `retries`, `peakHeapBytes` and `peakWorkingSetBytes` against `engine.MemoryBound`, and `commit` (it must not say `unknown` or `+dirty`).

### Manual drop test (not performed)

I cannot drag from Explorer in this session. What was checked: `wails build` succeeds, the built `s4browser.exe` starts with the title "s4browser file-drop spike" and creates its (empty) log file, and the classification and log code is unit tested. **Whether Windows actually delivers absolute paths to the callback is unverified.** To check it:

1. Run `F:\s4browser\build\bin\s4browser.exe` (rebuild with `wails build` first if sources changed). The window shows the log path, `%AppData%\s4browser\filedrop.log`.
2. Make the test items: a file, a folder (with a subfolder), and a second file.
3. Select **one file** in Explorer and drag it onto the window. Expect one entry `file  <absolute path>`.
4. Drag **one folder**. Expect `folder  <absolute path>` (the folder's own path, not its contents).
5. Select **a file, a folder and another file together** and drag them. Expect three entries in one drop, each with its own kind.
6. Drop one file from a **network share or another drive**, and one with a space and a non-ASCII character in the path.
7. Open the log file. Each drop should have a header line with the item count and one line per path with `absolute=true`. The paths on screen and in the log must be identical.
8. Check that dropping did not make the window navigate to the file, and that `s4browser.exe` is **not elevated**: if it runs as administrator, Explorer cannot drag into it (Windows blocks it).

If step 3 shows nothing: Wails delivers paths through WebView2's `postMessageWithAdditionalObjects`, which needs WebView2 1.0.1774.30 or newer (this machine has 157.0.4341.46). `DisableWebViewDrop` is deliberately not set, because it turns off the WebView2 drop that the path resolution may depend on. The Wails docs example sets it together with `EnableFileDrop`, so it is worth trying if a drop navigates the window instead.

### Open risks

- **Real S3 is still unverified.** Nothing here has run against AWS: the diagnostic GET, error codes, composite checksum format and ranged GETs are all stage 0.7 questions. Credential loading from `credential_process` is tested with a local helper process only.
- **Double credential cache.** `config.LoadDefaultConfig` already returns a cache, and the wrapper required by the docs puts another around it. Probe invalidates both, but the outer cache holds a credential until its own expiry, so a credential that the SDK would refresh early is refreshed late. A real `credential_process` with short-lived credentials (SSO, assume-role) should be watched on a long run.
- **`cleanup` does not touch versions.** It lists with `ListObjectsV2` and deletes with `DeleteObject`. If the bucket has versioning, this leaves delete markers and non-current versions that keep costing money. Confirm versioning before 0.7, as the plan says, and remove versions by hand if it is on.
- **The CLI links `internal/fakes3`** for `GuardStore` and the in-process fake. It is a test package in a production binary. Harmless for a test tool, but stage 1 should move the guard decorators out of `fakes3` before the app uses them.
- **The slowest-window figure includes ramp-up and Complete.** The first 10 s window starts at zero, so it can report connection set-up as the slowest window. Read it with the start time in the result. The steady state skips only the first 10 s.
- **A multi-file run reports whole-run figures for all files together.** Use one file at a time for per-file line-rate numbers.
- **Fake objects are synthetic.** With `--fake`, only `--fake-size` objects can be downloaded in a fresh process, and uploaded fake objects can be read back only in the same process (the fake keeps no data).
- **The drop window sends the frontend only path strings.** It does not read files. Folder contents are not expanded: that is stage 2.3.
- **A stale `s4engine-*.json` or `s4engine.exe` in the repo root is git-ignored,** but a result file written elsewhere is not.

### Next

Stage 0.7 (the real AWS gate, Opus) starts from this commit. It needs the profile, bucket and region, a measured speed-test result, and a decision on whether the bucket has versioning. It is not started here.

## Stage 0.7: real AWS gate

**Status:** done. **Decision: GO**, given by the user. Every gate check passed on commit `81fe26b` after one defect was fixed.

The test used one versioned bucket (`<BUCKET>`, us-west-2), only under `s4browser-test/gate-20261118/`. Default encryption is SSE-KMS. Nothing outside the prefix was listed, read, written or deleted.

### Gate results

The pass mark, set by the user, is a steady state of 200 MB/s (1.6 Gbps) in each direction. This is 80% of a 2 Gbps floor, because no measured line rate was supplied.

| Check | Upload | Download | Result |
|---|---|---|---|
| Steady state, after the first 10 s | **279.5 MB/s (2.24 Gbps)** | **251.3 MB/s (2.01 Gbps)** | Pass |
| Whole run | 2.30 Gbps in 34.8 s | 2.05 Gbps in 39.0 s | |
| Slowest 10 s window (the last 10 s of each run) | 218.5 MB/s | 171.0 MB/s | See limits |
| Peak Go heap, limit `MemoryBound` = 55.2 MB | 9.5 MB | 32.4 MB | Pass |
| Peak working set | 37.7 MB | 59.4 MB | |
| Retries | 0 | 0 | Pass |
| SHA-256 of the download equals the source | | Match | Pass |
| Multipart uploads left after a normal run | 0 | | Pass |
| Multipart uploads left after Ctrl+C (exit 130, `cancelled`) | 0 | | Pass |
| Write guard refuses a key outside the prefix and a `..` key, exit 2, before any write | Refused | | Pass |

Settings were the engine defaults: 16 parts per file, 32 parts in total, 64 MiB parts, `sparse` write strategy. The local files were on a WD_BLACK SN850X over 10 GbE.

### All real transfers

The limit was 4 uploads and 4 downloads. Four uploads and two downloads were used.

| Run | Commit | Settings | Result |
|---|---|---|---|
| Upload 1 | `deb8be1` | Defaults | Completed, 42.6 MB/s steady (0.34 Gbps) |
| Download 1 | `deb8be1` | Defaults | Completed, 259.3 MB/s steady (2.07 Gbps), hash match |
| Upload 2 | `deb8be1` | 64 parts per file, 64 in total | About 183 MB/s live (1.46 Gbps), Ctrl+C at 45 s, 0 uploads left |
| Upload 3 | `81fe26b` | Defaults | Completed, the gate run above |
| Download 2 | `81fe26b` | Defaults | Completed, the gate run above |
| Upload 4 | `81fe26b` | Defaults | Ctrl+C at 15 s, 0 uploads left |

### The defect and the fix

The first upload ran at 0.34 Gbps. Evidence for the cause:

- The rate was flat, and it fell as the last connections finished, so the remaining connections did not speed up.
- Four times the connections gave 4.1 times the throughput: about 2.7 MiB/s per connection at both 16 and 64.
- At the measured 29 ms round trip, 2.7 MiB/s is about 80 KB in flight per connection.
- A Go socket on this machine reports a 64 KiB `SO_SNDBUF`.

**Fix:** `s3x.TuneTransport` now sets each connection's send buffer to `s3x.SendBufferSize` (4 MiB). With no other change, the same default settings went from 42.6 MB/s to 279.5 MB/s. Raising concurrency alone would have needed about 70 connections to reach the pass mark, so no concurrency setting was changed. Tests: `TestTunedTransportSetsSendBuffer` (through the fake S3), `TestTuneTransportDialsWithoutABaseDialer`, and `TestSendBufferReachesTheSocket` (reads the value back from Windows).

### Open risks from earlier stages

| Item | Result | Evidence |
|---|---|---|
| Trailing CRC32 on parts | **Pass** | All 150 parts accepted. S3's CRC of part 1 equals one computed separately from the local file |
| Composite checksum | **Pass** | `HeadObject` returns `<base64>-150` with type `COMPOSITE`, equal to a separately computed value. `GetObjectAttributes` returns the same value **without** the `-150` suffix |
| `MpuObjectSize` | **Pass** | Complete accepted the correct size. A wrong size was not tried |
| Ranged GET `Content-Range` | **Pass** | Format `bytes a-b/total`, enforced by the adapter on every part of both downloads |
| `If-Match` with a multipart ETag | **Pass** | Sent on every part. A wrong ETag returns 412 `PreconditionFailed` |
| One-byte diagnostic GET | **Partly tested** | The request works: 206, `bytes 0-0/<size>`, correct byte. The path that follows a real HEAD 400, 401 or 403 was **not tested**, because no such error occurred |
| Upload and download under SSE-KMS | **Pass** | Objects report `aws:kms`. No KMS error or throttling |
| Error codes observed | **Pass for these** | 412 `PreconditionFailed`, 416 `InvalidRange`, GET 404 `NoSuchKey`, HEAD 404 with no error code |
| Other error codes | **Not tested** | `ExpiredToken`, `AccessDenied`, `SlowDown`, `BadDigest`, `InvalidPart`, `InvalidRequest` |
| Credential expiry during a transfer | **Not tested** | Not attempted, because it cannot be forced safely with this profile |
| Long parks on real connections | **Not tested** | `sparse` does not park |

**Credential type:** the test profile uses neither long-lived static keys nor `credential_process`. It holds temporary session keys with a session token in the shared credentials file, written by an external sign-in tool. They worked here, but **stage 1.1 should treat this as a third credential type**. It has no automatic refresh, so an expired session needs a new sign-in and `NotifyCredentialsChanged`.

### Manual Explorer drop test

The user ran the manual drop test from stage 0.6 and reported that it passed. It was not repeated in this stage.

### Limits of this evidence

- **The runs were short.** Each 10 GB run lasted 35 to 39 s, with one gate run per direction. They do not prove multi-hour throughput.
- **The slowest download window was below the pass mark.** The final 10 s averaged 171 MB/s, because connections finish and the rate falls at the end of a run. The steady-state figure includes this window and still passed.
- **The line rate is unknown.** Rates levelled off near 2.56 Gbps up and 2.35 Gbps down, which may be the path's limit, but no independent speed test was run.
- **The disk was barely tested.** 10 GB fits in this machine's RAM cache, so a long download can be slower, as stage 0.5 measured on another drive.
- **`steadyStateBytesPerSec` is too low for a cancelled run,** because cancelled parts are rolled back. Upload 2 reported 93 MB/s against about 183 MB/s live. Use it only for completed runs.

### New risks

- **Slow links could cause false stalls.** After the last body read, up to 4 MiB can still be in the socket, and `Timeouts.Response` allows 60 s for it to drain and for S3 to answer. A connection slower than about 70 KB/s would time out, which is about 18 Mbit/s across 32 connections. Stage 2.1 should cover this, for example by scaling the buffer or the timeout.
- **Kernel memory is outside `MemoryBound`.** Send buffers can hold up to 4 MiB per connection: 128 MiB at the default 32 connections.
- **Upload progress runs slightly ahead** of the network by the data held in send buffers.

### Left behind

- **In the bucket:** no current objects and no multipart uploads remain under the prefix. Two noncurrent 10 GB versions and their delete markers remain. The user chose to let the lifecycle rule remove noncurrent versions after 15 days.
- **On this machine:** all test files were deleted, but the empty directories `C:\s4gate\`, `down1` and `down2` remain, because the tool refused to remove a top-level directory on C:.

### Next

**Decision: GO**, given by the user on 2026-11-18. Stage 1.1 (profiles, vault storage and `credential_process`) may start from this commit in a new session. It is not started here.

## Stage 1.1: profiles, vault storage and `credential_process`

**Status:** done. `wails build`, `go test -race -count=1 ./...`, `go test -count=1 ./...`, `go vet ./...` and `staticcheck ./...` pass. `GOOS=darwin go vet` passes for creds, engine, journal, fsx, s3x, fakes3 and `cmd/...`. No AWS calls were made and `~/.aws` was not read: every test and the hands-on run used temporary AWS files with fake values.

### What exists

| Area | Files |
|---|---|
| Profile types, IDs, validation | `internal/creds/profile.go` |
| OS vault (`zalando/go-keyring` v0.2.8) | `internal/creds/vault.go` |
| Reading the shared AWS files | `internal/creds/awsfiles.go` |
| Hidden `credential_process` runner | `internal/creds/process.go`, `process_windows.go`, `process_other.go` |
| Profile list, active selection, credential caches | `internal/creds/manager.go` |
| Wails bindings | `profiles.go`, `app.go` |
| Working panel, replaced in stage 1.3 | `frontend/src/Profiles.tsx` |

### Credential types

| Kind | Source | Refresh |
|---|---|---|
| `vault-keys` | Static keys in Windows Credential Manager, entry `s4browser:profile/<name>` | Read from the vault again after a key change |
| `static` | Keys in the shared AWS files | Re-read by `Manager.Refresh` |
| `session` | Temporary keys and a token in the shared AWS files, written by an external sign-in tool (found in stage 0.7) | Re-read by `Manager.Refresh` after a new sign-in |
| `credential-process` | Helper command in the AWS config | Run again before expiry, one minute ahead |
| `other` | Role, SSO, web identity | Listed as unsupported. Left to the SDK |

- **Profile IDs are `app:<name>` or `aws:<name>`.** Use the ID as `engine.ObjectRef.Profile`.
- **On disk:** `%AppData%\s4browser\profiles.json` holds names, regions, the last four characters of the access key ID and the active ID. It holds no key material.
- **Switching profiles does not affect running transfers.** Each profile has one provider for the life of the `Manager`, and it survives key changes.
- **The AWS files are never written.**

### Why the SDK's `credential_process` provider is not used

| Problem with the SDK provider | What the app does |
|---|---|
| A visible console window opens from a GUI app | `CREATE_NO_WINDOW` |
| Quoted commands reach `cmd.exe` escaped as `\"` and fail | The command line is built by hand with `/D /S /C` |
| The config parser removes outer quotes, which breaks `"C:\a b\x.exe"` and `"C:\a b\x.exe" --name "c d"` | `awsFiles.processCommand` restores them from the raw file |
| Cancelling kills `cmd.exe` but not the helper it started | The helper runs in a job object, which is terminated on cancel |
| Parse errors quote the helper's output, which can hold secrets | Errors never include stdout |

### Evidence

| Requirement | Test or check |
|---|---|
| Helper runs hidden from a real GUI program | `TestHelperHasNoVisibleWindowFromGUIApp`: no console window. The control, `TestSDKProviderControl` (`S4_GUI_CONTROL=1`), reports a visible window with the SDK provider, so the check can tell the two apart |
| Quoted paths, arguments, `.cmd` helpers | `TestCredentialProcessQuotedPath`, `TestCredentialProcessKeepsQuotesTheParserRemoves`, `TestCredentialProcessBatchFile` |
| Cancel ends the helper | `TestCancelEndsTheHelperProcess`. It fails when the job is not terminated |
| Helper failures are credential errors with no secret in the text | `TestCredentialProcessFailures` |
| Real Credential Manager | `TestSystemVaultRoundTrip` (`S4_VAULT_TEST=1`), run once with a throwaway entry and fake values |
| Profile lifecycle, switching, failed vault writes, damaged state file | `TestAppProfileLifecycle`, `TestSwitchingKeepsEachProfilesCredentials`, `TestVaultFailuresLeaveNoHalfProfile`, `TestDamagedStateFileIsKept` |
| New sign-in is picked up without a restart | `TestStaticAndSessionKeysFromFile` |

**Hands-on run.** `wails dev` with fake AWS files, driven through the dev server page, which calls the same Go bindings. The list showed each kind, a `.cmd` helper in a path with a space loaded credentials, and the selection was saved. Adding fake keys created one Credential Manager entry, wrote no key material to the app folder, and deleting the profile removed the entry.

### Open risks

- **No real `credential_process` helper or real sign-in tool has been tried.** The helpers are test programs and a `.cmd` file. Expiry during a real transfer is checked in stages 2.1 and 4.3.
- **The engine does not use `creds.Manager` yet.** Stage 2.1 must build the production `CredentialProber` on `Manager.Refresh`, and whatever changes keys must call `NotifyCredentialsChanged`. Stage 1.2 should build its S3 clients from `Manager.Provider`. `s4engine` keeps its own loader.
- **`Refresh` proves only that credentials load.** It does not contact AWS, so an expired session whose file is unchanged still loads. Stage 2.1 must not treat that as recovery.
- **A refresh the caller abandons keeps running.** The SDK's cache detaches it from the caller's context, so the helper can run for up to one minute. Concurrent refreshes share that run.
- **Helpers that prompt on the console fail,** because the helper has no stdin. A helper that opens its own sign-in window should still work, but this was not tried.
- **Unsupported role profiles can still flash a console** if their `source_profile` uses `credential_process`, because that path stays inside the SDK.
- **A credentials file saved with a UTF-8 byte order mark loses its first profile.** The SDK does not recognise that section, so the app lists it as having no credentials.
- **macOS has no process containment.** Cancelling there ends only the shell. Mac work is deferred.

### Next

Stage 1.2 (bucket list, add by name, region detection, progressive listing) starts from this commit. It is not started here.

## Stage 1.2: bucket list, add by name, region detection, progressive listing

**Status:** done. `wails build`, `go test -race -count=1 ./...`, `go test -count=1 ./...`, `go vet ./...` and `staticcheck ./...` pass, and vet and staticcheck also pass with `-tags devfake`. `GOOS=darwin go vet` passes for browse, s3x, fakes3, engine, creds, journal, fsx and `cmd/...`. No AWS calls were made and `~/.aws` was not read.

### What exists

| Area | Files |
|---|---|
| Bucket list, add by name, region cache, error messages | `internal/browse/buckets.go` |
| Saved buckets (`%AppData%\s4browser\buckets.json`, per profile ID) | `internal/browse/saved.go` |
| Background listing, snapshots, windows | `internal/browse/listing.go` |
| Sort orders, natural name compare | `internal/browse/sort.go` |
| Browse client and region detection | `internal/s3x/browse.go` |
| Fake S3: ListBuckets, HeadBucket, GetBucketLocation, per-bucket regions, 301 for a request signed for the wrong region | `internal/fakes3/buckets.go`, small changes in `server.go`, `core.go`, `api.go`, `fault.go` |
| Wails bindings | `browse.go`, `app.go` |
| Dev-only fake S3 for the UI (`-tags devfake`) | `devfake.go`, `devfake_off.go` |
| Working panel, replaced in stages 1.3 and 1.4 | `frontend/src/Browse.tsx` |

**Bindings:** `Buckets(profileID)`, `AddBucket(profileID, name, region)`, `RemoveBucket`, `OpenListing(profileID, bucket, prefix)`, `ListingWindow(id, field, desc, offset, limit)` and `CloseListing(id)`. Listing progress arrives as the `s4:listing` event: about 5 per second per listing, plus one when it stops.

### How it works

- **Buckets.** `ListBuckets` is called with `MaxBuckets=1000`, because S3 reports `BucketRegion` only when the request has a parameter. This saves one HeadBucket per bucket. All pages are read. Buckets added by name are merged in. If `ListBuckets` is denied, the result says so (`listDenied`) and still returns the added buckets.
- **Region detection** (`s3x.DetectRegion`):
  1. `HeadBucket` is signed for us-east-1 and the region is read from `x-amz-bucket-region`. S3 sends this header on 200, 301 and 403.
  2. With no header, `GetBucketLocation` is tried. Empty means us-east-1 and `EU` means eu-west-1.
  3. A 404 is `ErrNoSuchBucket`. Anything else is `ErrRegionUnknown`, and only then is a region the user typed used.

  If a typed region disagrees with the detected one, the detected one is used and the result carries a note.
- **Region cache.** Regions are cached per bucket name, in memory, seeded from `buckets.json` and `ListBuckets`. If a listing gets a 301 naming another region (a bucket recreated elsewhere), it switches region once and carries on.
- **Listing.** Each listing runs in its own goroutine and calls `ListObjectsV2` with delimiter `/`, `encoding-type=url` and 1000 keys per page. URL encoding is needed because keys with control characters would otherwise break the XML. The folder's own placeholder object is skipped. Rows are kept compactly in Go. A listing stops at 1,000,000 rows (`truncated`), and at most 16 listings stay open: opening another closes the oldest. Browse calls keep the SDK's standard retries, because they are not engine part requests.
- **Sorting.** Name, size, modified and storage class, each ascending or descending.
  - Folders always come first. When sorting by size, date or class, folders are ordered by name.
  - Ties are broken by name.
  - Names compare as in Explorer: ASCII case is ignored, digit runs compare by value (`shot2` before `shot10`), and bytes decide the rest, so the order is total.
  - Each order is a cached permutation. As pages arrive, only the new rows are sorted and then merged in, so windows stay cheap while a listing loads.

### Evidence

| Requirement | Test or check |
|---|---|
| **100,000 objects list, sort and page in Go** | `TestHundredThousandObjects`. 100,000 objects plus 100 folders under one prefix are listed through the real SDK over HTTPS. All 8 orders are then walked end to end in windows of 1,000 and checked against an independent comparator. On this machine: listed in 0.42 s (101 pages), slowest full sort 58 ms, later windows under 1 ms, 11.9 MiB of heap for the listing with all 8 orders. Without the race detector the test asserts a sort under 500 ms, a window under 20 ms and heap under 48 MiB |
| Regions come from ListBuckets and listing uses the right region | `TestBucketsComeWithTheirRegions` (no HeadBucket calls, no 301s), `TestListBucketsPages` (2,345 buckets in 3 pages) |
| Every region detection path | `TestRegionDetection`: 200, 301 and 403 with the header (no GetBucketLocation call), no header so GetBucketLocation, empty location, location denied, missing bucket |
| Adding by name: works when listing is denied, kept per profile, survives a restart, and a saved region needs no lookup | `TestAddBucketByNameWhenListingIsDenied`, `TestAddBucketUsesEnteredRegionOnlyWhenDetectionFails`, `TestDamagedSavedFileIsKept`, `TestNormalizeBucketName` |
| Folders, placeholders, URL-encoded and unusual keys | `TestListingFoldersObjectsAndEncodedKeys` (space, `+`, `%`, non-ASCII, `\x01`, empty segment) |
| Stale region | `TestListingFollowsARegionChange` |
| Errors: denied, expired session, credentials fail (no request sent), missing bucket, 503 retried | `TestListingErrors` |
| Progressive loading, windows while loading, close cancels the request | `TestProgressiveLoadingAndCancel`, `TestConcurrentWindowsDuringLoading` (5 orders read at once during loading) |
| Limits | `TestListingLimitsAndEviction`, `TestRowLimitCountsFolders`, `TestWindowBounds` |
| The incremental sort equals a full sort | `TestIncrementalSortMatchesFullSort` (random rows, many ties, 40 chunks, 8 orders) |

Every browse test ends with a goroutine leak check.

**Tests of the tests.** I injected 18 deliberate bugs and the tests caught each one. They include: no URL encoding, no merge, no redirect follow, placeholder kept, region header ignored, folders last, descending ignored, numeric compare broken, ListBuckets without a parameter, row and folder limits ignored, close not cancelling (the test hangs), bucket not saved, and a typed region winning over the detected one.

**Hands-on run.** I ran `wails dev -tags devfake` with empty temporary AWS files and drove it through the dev server page. The fake held 100,040 entries under `dev-media/big/`.

- The page showed 4 buckets with their regions.
- The folder loaded in 1.21 s over 101 pages.
- Sorting by size, descending, kept the 40 folders first in natural order (reel1, reel2 … reel10).
- The first sorted page arrived in 57 ms.

Adding a bucket by name was not tried by hand. The tests cover it.

### Dev mode

```powershell
wails dev -tags devfake
```

In this mode browsing uses an in-process fake S3 with fake keys, whatever profile is selected, and added buckets are saved to a temporary folder. Set `S4_DEVFAKE_DENY_LIST=1` to make `ListBuckets` fail, so buckets must be added by name. `dev-hidden` is in ap-southeast-2. Release builds do not contain the fake: without the tag, `go list -deps .` lists no `fakes3`.

### Open risks

- **Browsing is unverified against real S3.** `BucketRegion` in `ListBuckets`, the region header on HeadBucket 301 and 403, and the 301 on a wrong-region `ListObjectsV2` follow the AWS documentation and the fake. Stage 4.3 should list the test bucket and add it by name with a real profile.
- **Opt-in regions and other partitions are untried.** Detection signs for us-east-1. It may fail for a bucket in an opt-in region the account has not enabled, or in GovCloud or China. The user can then type the region.
- **Case folding in the name sort is ASCII-only.** Non-ASCII letters compare by bytes, so `Ä` and `ä` are not treated as equal.
- **Keys with an empty segment show a folder named `/`.** Stage 2.3 decides how such keys are downloaded.
- **A listing is a snapshot.** Changes made after it loaded appear only when it is reopened. Stage 3.1 should reopen it after a delete or a create folder.
- **The working panel pages 100 rows at a time.** Virtual scrolling is stage 1.4, which should request only the visible rows from `ListingWindow` and refetch on `s4:listing` events.
- **Switching profiles leaves the old listing open.** The panel follows the active profile and reloads the bucket list on a switch. The previous profile's listing closes only when another listing opens.

### Next

Stage 1.3 (app shell and visual design system, Opus) starts from this commit and needs your approval of the design. It is not started here.

## Stage 1.3: app shell and visual design system

**Status:** done. **The design was approved by the user on 2026-11-18** from the design canvas (main window, states, tokens and components). `wails build`, `go test -race -count=1 ./...`, `go test -count=1 ./...`, `go vet ./...`, `staticcheck ./...` (also with `-tags devfake`) and `tsc` pass. No AWS calls were made and `~/.aws` was not read: the hands-on run used temporary AWS files with fake values and the devfake S3.

### The design ("Graphite")

- **Neutral greys, one blue accent.** Greys carry no tint, so the UI does not colour the eye during colour-critical media work. Blue marks selection, focus, progress and the single primary action. Amber is waiting or retrying, red is failed or delete, green is done. Every state also has a label, so colour is never the only signal.
- **Dark by default, light and "Match Windows" available.** The native title bar follows the theme through `WindowSetDarkTheme` and the related calls.
- **Windows system type.** Segoe UI Variable at 13/18 px with tabular figures; Cascadia Mono for keys, ETags and paths. No fonts are bundled (the template's Nunito is gone).
- **Dense rows for a mouse.** 28 px rows by default, 34 px "comfortable". This is below the 44 px touch-target guideline on purpose: the app is mouse-driven.
- **Layout:** top bar 44 px (profile switcher, view settings, details toggle), bucket sidebar 232 px (resizable 180 to 360), listing, details panel 300 px (260 below 1,280 px wide, can be hidden), and a full-width transfer dock (resizable, collapses to its tabs). Native Windows title bar. Window opens at 1280 × 800, minimum 1024 × 640.
- Contrast: the lowest-contrast text token (`--text3`) passes 4.5:1 on the surfaces it is used on, in both themes.

### What exists

| Area | Files |
|---|---|
| Tokens (every colour, type size, spacing, radius and size) | `frontend/src/styles/tokens.css` |
| Base elements and shared components: buttons, chips, progress bars, fields, banners, dialogs, menus | `frontend/src/styles/base.css` |
| Shell layout | `frontend/src/styles/shell.css` |
| Shell | `frontend/src/App.tsx`, `frontend/src/shell/` (`TopBar`, `Sidebar`, `Browser`, `Details`, `TransferDock`, `useProfiles`) |
| Icon set, dialog and menu helpers, splitter, formats | `frontend/src/ui/` |
| Theme, density and layout preferences (`localStorage`, `s4:` keys) | `frontend/src/settings.ts` |
| Window options | `main.go` |

The stage 1.1 and 1.2 working panels (`Profiles.tsx`, `Browse.tsx`) and the drop spike page are gone; everything they did is in the shell. The Go side is unchanged apart from `main.go`.

### Behaviour worth knowing

- **Profiles:** the top bar shows the active profile, its kind and when its session expires. Credentials are loaded (`CheckProfile`, no AWS request) whenever the active profile changes; "Load credentials again" repeats it after a new sign-in. Adding keys and deleting an app profile are in the profile menu; delete asks first.
- **Buckets:** reload, add by name (dialog), forget an added bucket (hover), and a "not allowed to list buckets" state.
- **Listing:** breadcrumb, back history, up (also Backspace and Alt+Up), reload, sortable headers with `aria-sort`, click, Ctrl-click and Shift-click selection, arrow keys, Enter or double-click opens a folder. Rows are still fetched 200 at a time with a pager: **stage 1.4 replaces the pager with the virtualized table** and should keep the row markup, classes and keyboard behaviour.
- **Details** shows what the listing knows: one object, a multi-selection total, or the current folder. Encryption, checksum, ETag, tags and presigned links need extra requests and belong to stage 3.1, which should add them to this panel as the canvas shows.
- **Not built yet, shown disabled with a tooltip naming the stage:** New folder and Delete (3.1), Upload, Download, Pause all and transfer settings (2.4).
- **Drag-in:** the listing is now the Wails drop target (`OnFileDrop(..., true)` and `--wails-drop-target: drop`). While files are over it, the drop overlay names the destination. Dropped paths are still logged by Go to `%AppData%\s4browser\filedrop.log` and listed in the dock as "Dropped, not uploaded" until stage 2.4.

### Evidence

Hands-on, `wails dev -tags devfake` driven through the dev server page with fake AWS files:

- `dev-media/big/` (100,000 objects, 40 folders) loaded in 1.1 to 1.3 s. Size descending kept folders first in natural order. A Shift-click range selected 3 objects, and the details panel and status bar showed their total.
- At 1440 × 900 and at the 1024 × 640 minimum there is no page scroll. At the minimum, toolbar labels collapse to icons and secondary status text is hidden, so the pager still fits.
- Light and dark themes, the profile menu, and the add-bucket dialog (focus moves into it, Escape closes) were checked.
- The dev server page logs `Cannot read properties of null (reading 'nodes')` from Wails' own `/wails/ipc.js`. It comes from the browser-mode bridge script, not app code.

### Not verified

- **The built exe was not looked at.** Only the dev server page in a browser was checked, so the native title bar theme switching, the window's minimum size and the WebView2 drop overlay are unverified in the real window.
- **A real Explorer drop on the new drop target was not tried.** It needs a hand drag (as in stage 0.6). The change from a whole-window drop to the listing as target is the part to check.
- No automated frontend tests exist. The plan has none for the UI; stage 1.4 may add some for the virtualized table.

### Next

Stage 1.4 (virtualized browser table and navigation) starts from this commit. It is not started here.

## Stage 1.4: virtualized browser table and navigation

**Status:** done, committed as `780f3b1`. `wails build`, `go test -race -count=1 ./...`, `go vet ./...` and `staticcheck ./...` (also with `-tags devfake`) and `tsc --noEmit` pass. No AWS calls were made and `~/.aws` was not read: the hands-on run used temporary AWS files with fake values and the devfake S3.

### What exists

- **Virtualized table** (`frontend/src/shell/Browser.tsx`, `@tanstack/react-virtual` 3). Only the rows in view plus 12 of overscan are in the DOM (27 to 41 rows at 1440 × 900). Rows are fetched from Go with `ListingWindow` in blocks of 100, only for the blocks in view. They are refetched on `s4:listing` version changes and dropped on a sort change (placeholder rows show until the new block arrives). The page never holds more than a few blocks.
- **Design kept.** Same row markup and `.cols`/`.trow`/`.thead` classes. Rows are placed with a transform inside `.vbody` (small additions at the end of the table section in `shell.css`; the pager CSS is gone). The row height is read from `--row-h` and follows the density setting (a `MutationObserver` on `data-density`).
- **Selection** is a list of index ranges in the current sort order, so Ctrl+A over 100,000 rows is `[[0, 100039]]`. New Go binding `ListingSummary` (`browse.go`, `Service.Summarize` in `internal/browse/listing.go`, one test) totals count, folders, objects and bytes for the ranges, so unrendered rows never reach the page. `App.tsx` now holds a `SelectionInfo` (totals, plus the row when exactly one is selected) instead of `Row[]`; `Details.tsx` and the status bar use it.
- **Keyboard:** click, Ctrl-click (splits or merges ranges), Shift-click, Ctrl+Shift-click, arrows with Shift to extend, PageUp/PageDown (a page minus two rows), Home/End, Ctrl+A, Ctrl+Space, Escape, Enter or double-click on a folder, Backspace or Alt+Up. The focused row stays in the DOM when scrolled out of view, so the keyboard focus is never lost to the page body.
- **Navigation:** the back history and breadcrumb are unchanged. Each folder remembers its scroll position, focused row and selection (the selection only if the listing finished with the same row count and sort). After navigating, the listing takes the keyboard focus if the focus was in the listing or the path bar (not when it was in the bucket list).
- **Accessibility:** `role="table"`, `row`, `cell`, `columnheader`, `rowgroup`, `aria-rowcount` (total + 1), `aria-rowindex` (header is 1), `aria-sort`, `aria-multiselectable`, a roving `tabindex` on rows, and `aria-busy` on rows not fetched yet.

### Evidence

`wails dev -tags devfake` driven through the dev server page, `dev-media/big/` (100,000 objects, 40 folders):

- Scroll height 2,801,120 px at 28 px rows. A 100-step sweep from top to bottom, using real scroll events, rendered at most 41 rows and left no placeholder rows behind; the longest step took 19 ms. End and Home jump across the whole listing.
- All four sorts, both directions: sort attribute and order correct (size ascending 0 B, 1 MiB, 2 MiB…; size descending 97.7 GiB first; modified; name descending reel40, reel39…). Folders stay first. A sort change was visible within 50 ms of the click except the first name-descending sort (460 ms, Go sorting 100,000 rows once).
- Shift+End from row 28 selected 100,012 items: the status bar and details panel showed 100,000 objects, 12 folders and 4.66 PiB. Ctrl+A showed 100,040 items. Both came from `ListingSummary` and took no visible time.
- Enter into a folder and Backspace or Alt+Up back, and the Back button: the scroll position (500,000 px), the selected row and the focus were restored. Focus moved into the listing after Enter.
- Comfortable density (34 px rows) and the light theme render correctly. At 1024 × 640 and at 1440 × 900 there is no page scroll and the header stays sticky.

### Behaviour worth knowing

- **A selection is dropped when the sort order changes or when the listing grows** (a new version while loading), because index ranges only mean something for one order of one version. Folders being opened from a finished listing are not affected. Keeping a selection by key across a sort change would need a Go binding that maps keys to indices; it was not built.
- The first row window for a folder is fetched after `OpenListing` returns, so while the listing is loading its first page the table is empty.

### Not verified

- **The built exe was not looked at.** Everything above ran in the dev server page in a browser pane, not in the WebView2 window. The pane was hidden during some runs, so animation-frame rates were not measured; "smooth" here means bounded DOM, no placeholder rows left and no long stalls, not a frame-rate figure.
- Mouse-wheel and scrollbar dragging by hand were not tried (scroll was driven by script and real key presses).
- Screen reader behaviour of the virtualized rows was not tested.
- A selection that includes unfetched rows and then goes through a folder round trip was checked for one row only.
- No automated frontend tests exist. The range helpers (`addRange`, `removeIndex`, `inRanges`) are the best candidates if tests are added.

### Next

Stage 2.1 (retry, pause, resume, credential recovery, orphan cleanup, Opus) is next in the plan. It is not started here.

## Stage 2.1: retry, pause, in-session resume, credential recovery, orphan cleanup

**Status:** done, not committed (waiting for the user's go-ahead). The fault suite passes, including killing a real process mid-transfer and relaunching. `wails build`, `go test -race -count=1 ./...`, `go test -count=1 ./...`, `go vet ./...` and `staticcheck ./...` (also with `-tags devfake`) pass. `GOOS=darwin go vet` passes for engine, journal, fsx, s3x, fakes3, creds, faultsuite and `cmd/...`. One full non-race run failed only in `internal/browse` (`TestHundredThousandObjects` sorted in 713 ms against its 500 ms limit while every package ran at once); it passes alone and no browse code changed. No AWS calls were made and `~/.aws` was not read: credential tests use temporary AWS files with fake session keys, and every S3 request went to the local HTTPS fake.

### What exists

| Area | Files |
|---|---|
| Launch cleanup: `journal.Recovery` (implements `Recoverer`), `FileStore.Loaded` | `internal/journal/recover.go`, `filestore.go` |
| Production `RemoteCleaner`, `CredentialProber`, per-profile client factory | `internal/s3x/recovery.go` (`NewCleaner`, `NewProber`, `ProbeAccess`, `NewClients`) |
| Credential change notifications, `Reload`, `Prober` | `internal/creds/manager.go`. `profiles.go`: `CheckProfile` now calls `Reload` |
| Drain allowance for slow links | `internal/engine/body.go`, `activity.go`, `watchdog.go`, `upload.go` (`observeUpload`), `limits.go` |
| The probe receives the waiting job's object | `internal/engine/credentials.go`, `seams.go` |
| Fault suite (tests only) | `internal/faultsuite/`: `rig_test.go`, `kill_test.go`, `faults_test.go`, `creds_test.go`, `intent_test.go` |
| Fake S3 rules can match an access key | `internal/fakes3/fault.go` (`Rule.AccessKey`) |

### How the open items were resolved

| Item from earlier stages | Resolution |
|---|---|
| Orphaned uploads and temp files | `journal.NewRecovery` and `Recover`: in the background, 4 entries in parallel, 5 tries per request, `ErrCredentials` deferred. Active entries are aborted by ID, creating entries by `MatchesIntent`, and a temp file is deleted only if `fsx.IsTempPath` accepts its name. Design section 11 |
| Guard intent matching | Recovery owns only the launch set (`FileStore.Loaded`). For a creating entry it lists first, then reads the journal, and defers if any entry of this session names the same bucket and key. A recorded upload ID is never aborted by intent. A job's intent is durable before its Create, so this order rules out aborting a live upload |
| Production `CredentialProber` | `creds.Manager.Prober(clients)`: `Manager.Refresh`, then a signed one-byte `GetObject` of the waiting job's object. Only an answer S3 gives after authenticating counts (`NoSuchKey`, `AccessDenied`, success). An expired session whose file is unchanged loads, and still fails the probe |
| Whatever changes keys notifies | `Manager.OnCredentialsChanged` fires after `AddKeys`, `UpdateKeys`, `Delete` and `Reload`. The prober uses `Refresh`, so a probe never starts a probe. Connecting it to `NotifyCredentialsChanged` and `RecoverProfile` is stage 2.2 |
| Kill and relaunch | The test binary runs the app as a child process. The test kills it with `Process.Kill` (TerminateProcess) and relaunches: upload, after-Create and download cases |
| Slow links looked like stalls | The timeout is scaled: the await limit is `Response` plus a drain allowance, from the attempt's own measured rate (bodies over two buffers) or the engine's per-connection estimate, which halves on each await-phase stall. Capped at 20 min. Design section 9 |
| Double credential cache | The app builds every client from `Manager.Provider`: one `aws.CredentialsCache` per profile with a one-minute window. The SDK does not wrap an existing cache (`TestClientsAreSharedPerProfileAndRegion`). `s3x.NewClients(manager.Provider)` is the factory for stage 2.2. The CLI keeps its own loader and its double cache |
| Session-key profiles | Jobs wait until a new sign-in. Each probe reads the file again, so new keys are picked up within `ProbeMax` (60 s) with no click, or at once through `Reload` |

### Contract changes

- **`engine.CredentialProber.Probe(ctx, ref ObjectRef)`** replaces `Probe(ctx, profile string)`. The engine passes the first waiting job's object. Design section 10. The CLI now uses `s3x.NewProber` with its own reload, so it also probes S3.
- **`engine.Config.SendBuffer`** (0 means `engine.DefaultSendBuffer`, 4 MiB), `engine.MaxDrainAllowance`, `AttemptActivity.SetDrain` and `Drain`. `s3x.SendBufferSize` is now defined from `engine.DefaultSendBuffer`.
- Additive: `journal.Recovery`, `NewRecovery`, `RecoveryOptions`, `FileStore.Loaded`. `s3x.NewClients`, `NewProber`, `ProbeAccess`, `NewCleaner`. `creds.Manager.OnCredentialsChanged`, `Reload`, `Prober`. `fakes3.Rule.AccessKey`.
- Package graph: `internal/journal` now imports `internal/fsx`, and `internal/s3x` imports `internal/journal`. Design section 1.

### Evidence

All against the HTTPS fake through `s3x.Store`, `s3x.TuneTransport` and the file journal, unless noted. Each test ends with a goroutine leak check.

| Requirement | Test |
|---|---|
| Kill during upload, after a lost Create response left an orphan. Relaunch aborts both uploads, empties the journal, and the same file then uploads | `TestKillDuringUploadThenRelaunch` |
| Kill after Create and before the ID is durable: found by intent | `TestKillAfterCreateBeforeIDIsDurable` |
| Kill during a large download: temp file and entry removed, a new download is byte-exact | `TestKillDuringDownloadThenRelaunch` |
| A second instance cannot open the live journal (checked in each kill test) | `runAndKill` |
| 503 storm, resets mid-body, lost part response, SlowDown with Retry-After, watchdog stall, control-request errors, lost Complete: each completes and stores every part once | `TestUploadFaultsOverHTTPS` |
| Download faults: 503, reset, truncate, stall, HEAD 503. Every byte written once | `TestDownloadFaultsOverHTTPS` |
| Nothing is sent while paused, and resume sends only unfinished parts. A part S3 stored just as the pause cancelled it may be sent once more, and the test allows exactly those | `TestPauseAndResumeOverHTTPS` |
| Pause during a credential wait, resume, wait again, recover | `TestPauseDuringCredentialWaitOverHTTPS` |
| Cancel while expired: the abort fails, the entry stays, the next launch aborts | `TestCancelWhileCredentialsExpiredThenRelaunch` |
| Close during Complete: outcome unknown, the next launch finds the upload gone and drops the entry | `TestCloseKeepsUnknownOutcomeForNextLaunch` |
| The session expires mid-upload. With the file unchanged, real probes keep the job waiting. A new sign-in resumes it with no notification, no part sent again and no retry charged | `TestSessionExpiresDuringUpload` |
| `Reload` resumes a waiting job at once (probes an hour apart) | `TestReloadResumesWaitingJobAtOnce` |
| Launch cleanup waits for credentials and finishes on `Reload` | `TestLaunchCleanupWaitsForCredentials` |
| Prober answers: expired, `NoSuchKey`, `AccessDenied`, 503, reset, invalid key, credentials that do not load (no request sent), no notification | `TestProberRequiresS3ToAcceptCredentials` |
| A new upload of the same key survives launch cleanup, and the orphan is removed afterwards | `TestLaunchCleanupSparesNewUploadOfSameKey`, `TestLaunchCleanupRacesNewUploads` (cleanup in a loop while 6 jobs start) |
| Recovery unit cases: launch set, read-after-list order, window, retries, parallel bound, temp names, context | `internal/journal/recover_test.go` |
| Cleaner paging (1,005 uploads), gone, credential marking. One client and one cache per profile and region | `internal/s3x/recovery_test.go` |
| Slow link: drain from the attempt's own rate, from earlier attempts, and learned from stalls (an in-process store that models a socket buffer) | `TestSlowLinkIsNotAStall`, `TestSlowLinkLearnsFromStalls` |
| Notifications | `TestCredentialChangesAreAnnounced` |

**Tests of the tests.** 20 deliberate bugs, each caught: journal read before listing, no session-entry guard, launch set from `List` instead of `Loaded`, no temp-name check, credential errors retried, deferred entries deleted, no intent window, unbounded parallelism, prober skipping the S3 request, probe passing on credential codes, `NoSuchUpload` not mapped to gone, no listing pages, cleaner credential errors unmarked, no drain allowance, no own-rate measurement, no halving on stalls, no success samples, `UpdateKeys` silent, prober using `Reload`, probe given an empty object. The halving bug was missed at first, because growth by one `Response` per stall still converged. The test now uses an 8 s drain and allows at most 3 retries, which only the halving meets.

### Open risks

- **`TestHundredThousandObjects` (stage 1.2) has a wall-clock limit** that a fully loaded machine can miss when all packages test in parallel.

- **Nothing is wired into the app yet.** Stage 2.2 must open the journal and create `journal.NewRecovery` before the engine; run `Recover` in the background and show `Report.Warnings` and `FileStore.Warnings`; build the store, prober and cleaner from `s3x.NewClients(manager.Provider)`; and connect `OnCredentialsChanged` to `NotifyCredentialsChanged` and `RecoverProfile`, the latter in a goroutine because it waits on S3. `ErrUnsupportedVersion` must disable uploads.
- **Real S3 is unverified for the new requests:** the one-byte probe on a missing key, `ListMultipartUploads` paging and `Initiated` times, and the codes S3 returns for an expired session token. Stage 4.3 should run the credential expiry check.
- **Intent matching still trusts the time window** for uploads by other clients to the same key, as designed. A launch entry whose key the user uploads again in this session is deferred until that job ends, then cleaned by the next `Recover`, `RecoverProfile` or launch.
- **The drain allowance is an estimate.** Once the estimate is low, a connection that dies right after its body was sent can take up to `Response` plus 20 min to be noticed. The slow-link tests model a socket buffer in process. They do not throttle a real network.
- **The fake's reset sends a 200 header first,** because its `hijack` flushes, so a reset at byte 0 looks like a broken 200 response. The prober reads the body to cope. Engine attempts already treat a 2xx with a broken body as retryable.
- **A `credential_process` helper runs on every probe,** 5 to 60 s apart, while jobs wait.
- **Downloads of 64 MiB or less are still not journaled,** so a crash can leave their temp file.
- **The CLI keeps the double credential cache.** It matters only for short-lived credentials on long CLI runs.

### Next

Stage 2.2 (queue service and Wails bindings) starts from this commit. It is not started here.

## Stage 2.2: queue service and Wails bindings

**Status:** done, not committed (waiting for the user's go-ahead). `wails build`, `go test -race -count=1 ./...`, `go vet ./...`, `staticcheck ./...` (also with `-tags devfake` for vet and staticcheck) and `GOOS=darwin go vet` (engine, journal, fsx, s3x, fakes3, creds, queue, `cmd/...`) pass. One full non-race `go test ./...` failed only in `internal/browse` (`TestHundredThousandObjects`, 889 ms against its 500 ms limit while all packages ran at once, the stage 2.1 flake); it passes alone. No AWS calls were made and `~/.aws` was not read: tests use temporary AWS files with fake keys and the HTTPS fake S3, and never touch `%AppData%\s4browser`. No engine contract changed.

### What exists

| Area | Files |
|---|---|
| Queue service: opens journal, recovery, engine; status; shutdown | `internal/queue/queue.go` |
| Job view model, `s4:queue` event, error text | `internal/queue/view.go` |
| Pause, resume, cancel, retry, remove, all-variants, clear | `internal/queue/commands.go` |
| Upload and download requests, validation | `internal/queue/submit.go` |
| Transfer settings file and live apply | `internal/queue/settings.go` |
| In-memory journal for the unsupported-version case | `internal/queue/memjournal.go` |
| Bindings and wiring | `transfers.go`, `app.go` (`startup`, `shutdown`) |
| devfake routing (one shared fake for browse and queue) | `devfake.go`, `devfake_off.go` |
| Tests, through the bindings | `transfers_test.go` |

### Wiring (the stage 2.1 checklist)

`App.queueOptions(dir)` connects the queue to the app. `queue.Open` then: opens `journal.OpenFile` and creates `journal.NewRecovery` **before** the engine; builds the store, prober and cleaner from `s3x.NewClients(manager.Provider)`; starts `Recover` in a background goroutine; registers `Service.CredentialsChanged` with `manager.OnCredentialsChanged`, which runs `Engine.NotifyCredentialsChanged` and starts `RecoverProfile` in a goroutine. `Report.Warnings` and `FileStore.Warnings` are in `TransferStatus().warnings`.

- `journal.ErrUnsupportedVersion`: the file is left alone, the queue uses an in-memory journal, uploads are refused with `uploadsDisabledReason`, downloads still work.
- `journal.ErrLocked` (second instance): `queue.Open` fails with `ErrUnavailable` ("another copy of s4browser is already running..."), the App keeps no queue, and `TransferStatus` returns `available:false` and `error`. The first instance's journal is untouched (tested while its upload runs).
- Shutdown (`closeQueue`, 30 s): stop cleanup, `Engine.Close` (cancels running jobs and aborts their uploads, as in design section 3), then close the journal.
- devfake: `devQueueOptions` points transfers at the same in-process fake as browsing. Journal and settings go to a temp folder. `dev-small/downloads/sample{1..4}.bin` and `dev-media/samples/sample{1..4}.bin` (3 MiB to 8 GiB) have downloadable content; the 100,000 listed objects do not.

### Bindings (methods on `App`, TypeScript in `frontend/wailsjs/go/main/App.d.ts`)

| Method | Returns | Notes |
|---|---|---|
| `TransferStatus()` | `queue.Status` | Also sent as `s4:queue-status` when cleanup progresses |
| `QueueJobs()` | `JobView[]` | Submission order. Call on open and after a `resync` event |
| `QueueUploads(UploadRequest)` | `SubmitResult` | Whole call errors if uploads are off, no profile or bucket, or region unknown |
| `QueueDownloads(DownloadRequest)` | `SubmitResult` | Same |
| `PauseJob(id)`, `ResumeJob(id)`, `CancelJob(id)`, `RetryJob(id)`, `RemoveJob(id)` | `error` | Messages are user-readable |
| `PauseAll()`, `ResumeAll()`, `CancelAll()` | `(int, error)` | Count of jobs changed |
| `ClearFinished(includeFailed bool)` | `(int, error)` | Completed and cancelled; failed only if `includeFailed` |
| `TransferSettings()` | `SettingsState` | `{current, defaults, limits}` |
| `SetTransferSettings(TransferSettings)` | `SettingsState` | Validates, saves, applies live |

Types (JSON names):
- `UploadRequest {profileId, bucket, region, prefix, paths[]}`. Empty `region` means look it up through browsing. `prefix` is `""` or `"a/b/"` (slash added). Key is `prefix + file name`.
- `DownloadRequest {profileId, bucket, region, keys[], destDir}`. The file is `destDir\<last key segment>`.
- `SubmitResult {items: [{source, jobId?, error?}]}`: one item per path or key, in order. Refused items (relative path, missing file, folder, key ending in `/`, names Windows cannot hold or `..`, reserved device names) do not stop the others. **Stage 2.3 expands folders here and replaces `localName` with the full Windows rules**; the request and result shapes are meant to take that.
- `JobView {id, direction("upload"|"download"), profile, bucket, key, localPath, name, state, phase, totalBytes, doneBytes, bytesPerSec, etaSeconds(-1 unknown), partsDone, partsTotal, retries, reason, errorCode, detail, warning, finished, canPause, canResume, canCancel, canRetry, canRemove}`. `state` is `queued|running|pausing|paused|credential-wait|cancelling|failing|cancelled|failed|completed`; `phase` is `preparing|transferring|finalizing`. `reason` is set for waits and failures (a credential wait names the profile). `detail` is the engine's full text. Use the `can*` flags for buttons.
- `Status {available, error?, uploadsDisabled, uploadsDisabledReason?, warnings[], cleanup{running, finished, uploadsAborted, tempsRemoved, pending, error?}}`.
- `TransferSettings {parallelFiles, partsPerFile, totalParts, partSize, uploadBytesPerSec, downloadBytesPerSec}`. Defaults are the engine's (4, 16, 32, 64 MiB, unlimited). `limits` gives the ranges. Saved in `transfer-settings.json` next to `profiles.json`. Concurrency applies at the next dispatch, caps at once, part size to jobs not yet planned. A damaged file is kept as `.bad-<time>` and a warning shown.

### Events

- `s4:queue` payload `{seq, at(unix ms), jobs: JobView[], removed?: string[], resync?}`. About 5 per second (the engine's 200 ms batch), only changed jobs, `seq` increases by 1. Merge `jobs` by id and drop `removed`. On `resync` call `QueueJobs`. A newly submitted job appears in the next batch (up to 200 ms), so call `QueueJobs` after a submit.
- `s4:queue-status` payload is `Status`.

### Evidence (`transfers_test.go`, all through binding methods, each ends with a goroutine leak check)

| Requirement | Test |
|---|---|
| Upload and download round trip, byte-exact, no temp left, journal empty, clear reports `removed` | `TestUploadAndDownloadRoundTrip` |
| Pause sends nothing, resume continues without resending finished parts | `TestPauseAndResume` |
| Cancel leaves no multipart upload, no object, no journal entry | `TestCancelLeavesNoMultipartUpload` |
| Pause, resume, cancel all; queued jobs; clear | `TestBulkCommands` |
| Failure reason, retry, clear rules | `TestFailedJobShowsItsReason` |
| Settings change while running (2 MiB/s cap lifted mid-upload), invalid refused, persisted, read after relaunch | `TestSettingsChangeWhileRunning`, `TestDamagedSettingsFileIsKeptAside` |
| Credential wait in job state and events, then `CheckProfile` resumes it with probes an hour apart | `TestCredentialWaitThenRecovery` |
| Events batched (20 parts, no two events within 100 ms, seq contiguous, progress visible) | `TestEventsAreBatched` |
| Launch cleanup in the background, warnings exposed, non-temp path never deleted, deferred until credentials change | `TestLaunchCleanupRunsAndReportsWarnings`, `TestLaunchCleanupResumesWhenCredentialsChange`, `TestCorruptJournalIsReported` |
| Unsupported journal, second instance | `TestUnsupportedJournalDisablesUploads`, `TestSecondInstanceIsRefused` |
| Request validation | `TestSubmitValidation` |
| Close with running jobs leaves nothing | `TestCloseWithRunningJobsLeavesNothing` |

**Tests of the tests.** 16 deliberate bugs, all caught: credential change not sent to the engine, cleanup never started, `RecoverProfile` not run, unsupported journal leaving uploads on, clear removing failed jobs, settings not applied to the engine, settings not kept in memory, settings file not written, journal not closed, `ResumeAll` and `PauseAll` doing nothing, removal not reported, event `seq` stuck, lock error lost, reserved names allowed. One was missed at first (settings not kept in memory): the test now checks the value `SetTransferSettings` returns.

### Open risks

- **Quitting cancels running jobs** (engine close semantics). Stage 2.4 should warn when closing with active transfers. Nothing resumes across a restart.
- **Not run in the real window.** The bindings were exercised from Go tests only. `runtime.EventsEmit`, `openQueue` (config folder) and `shutdown` run only in the real app.
- **Region lookup** for an empty `region` uses `browse.Service.Region`. The UI should pass the region it already has.
- **`localName` is the minimum**, not the Windows rules (stage 2.3). A destination that already exists fails the job with `ErrDestinationExists`.
- **Jobs stay until cleared.** 1,000 tracked jobs fit the memory bound.
- Real S3 behaviour of the new paths is unverified (stage 4.3), and the 2.1 risks still apply.

### Next

Stage 2.3 (folder transfers, Windows filename and path safety) extends `UploadRequest`, `DownloadRequest` and `localName` in `internal/queue/submit.go`. Stage 2.4 builds the dock on the bindings and events above. Neither is started here.

## Stage 2.3: folder transfers and Windows path safety

**Status:** done, committed as `e8233a0`. `wails build`, `go test -race -count=1 ./...`, `go test -count=1 ./...`, `go vet ./...`, `staticcheck ./...` (both also with `-tags devfake`) and `GOOS=darwin go vet` (engine, journal, fsx, s3x, fakes3, creds, queue, `cmd/...`) pass. `TestHundredThousandObjects` did not fail in this stage's full runs, so it was not loosened. No AWS calls were made and `~/.aws` was not read: tests use temporary AWS files with fake keys and the HTTPS fake S3, and never touch `%AppData%\s4browser`. No engine contract changed, so `docs/engine-design.md` is unchanged. `frontend/wailsjs/go/models.ts` was regenerated by `wails build` (new `SubmitItem` and `Refusal` types).

### What exists

| Area | Files |
|---|---|
| Name rules, key splitting, `Placer` (collisions, existing files, links, length), `MakeParents` | `internal/fsx/pathsafe.go` |
| Folder upload walker, prefix expansion, recursive listing | `internal/queue/expand.go` |
| Requests, `SubmitItem`, `Refusal`, per-item handling (`localName` is gone) | `internal/queue/submit.go` |
| Tests: path rules; walker and expansion on synthetic input; memory; round trip through the bindings | `internal/fsx/pathsafe_test.go`, `internal/queue/expand_test.go`, `internal/queue/expand_mem_test.go` (`!race`), `transfers_folders_test.go` |

### Rules (all refuse, none rename)

`fsx.CheckName` (one name), `fsx.SplitKey` (a key, or the part below a prefix) and `fsx.Placer.Place` (a path below the destination). A refusal is a `*fsx.PathError{Code, Msg}`. It matches `ErrUnsafePath`, or `ErrDestinationExists` for code `exists`. `fsx.CodeOf(err)` returns the code.

| Code | Refused when |
|---|---|
| `absolute`, `unc`, `drive-path` | the key starts with `/`, with `//` or `\\`, or with a drive letter and colon |
| `traversal` | a `.` or `..` name |
| `empty-segment` | `a//b`, an empty key, or a trailing slash where a file is expected |
| `backslash` | any backslash (Windows reads it as a separator) |
| `forbidden-char` | any of `< > : " | ? *` (this also stops `file:stream`) |
| `control-char` | below 0x20, or 0x7f |
| `trailing-dot-space` | the name ends in a dot or a space |
| `reserved-name` | CON PRN AUX NUL COM1-9 LPT1-9 (also the superscript digits), CONIN$ CONOUT$, with or without an extension or trailing spaces; also a name shaped like s4browser's own temp file |
| `name-too-long` | one name over 255 UTF-16 units |
| `too-long` | the full path over `fsx.MaxPath` (259 UTF-16 units), **or** the path of the temp file beside it (`~s4-<16 hex>.s4tmp`, 26 characters) over it |
| `invalid-text` | not valid UTF-8 |
| `collision` | same name as an earlier item of the request ignoring letter case, or a file where an earlier item needs a folder, in either order |
| `exists` | the destination exists locally (any type, any letter case on NTFS). Never replaced |
| `blocked` | a file lies where a folder is needed, or the path cannot be inspected |
| `link` | a symbolic link or junction lies on the way below the chosen folder. A download never writes through one |

The whole key is checked, parent names included, even for a single object that only uses its last name (`x/../y.bin` is refused). Codes of the queue package: `relative`, `not-found`, `unreadable`, `not-regular`, `key-too-long`, `duplicate`, `folder-root`, `folder-key`, `listing`, `stopped`.

`Placer` keeps one 64-bit hash per placed file and folder (24 bytes per object measured), never the names. A hash clash would only refuse a harmless name. Collisions are decided in listing order, so the first object keeps the name and the later one is the refused item. It remembers which folders it has seen on disk, so Lstat runs once per folder, not per object.

### Decisions

**Upload of a folder** (`uploadFolder`, `walker`): one job per file under `<prefix><folder name>/<relative path with forward slashes>`.
- **Empty folders** make no object (S3 has none to create). They are counted in `emptyFolders`. A folder that holds only refused entries is not empty.
- **Links and junctions** below the dropped folder are not followed and not uploaded: each is refused with `link`, so a loop is impossible (a test makes a junction back to the top). A link the user drops directly is followed once and keeps its own name.
- **Hidden and system files** are uploaded like any other file (tested with `attrib +h` and `+s`).
- **Files vanishing or unreadable:** the folder is read in batches of 1,024 entries and each file is checked with the entry's own info call, not opened. A vanished entry is refused `not-found`, one that cannot be examined `unreadable`, a folder that cannot be read (or read fully) `unreadable`, and the walk continues. A file that vanishes or locks after expansion fails its own job with the engine's reason and can be retried. An unreadable dropped folder is an item error.
- Devices and pipes: `not-regular`. A name that is not UTF-8, or a key over 1,024 bytes: refused per file. A drive or share root has no name: `folder-root`. Two dropped items that would upload to the same key: the second is `duplicate`.
- **Memory:** nothing keeps the file list. It holds one batch, the sub-folder names of the folders on the current path, and the capped lists below.

**Download of a prefix** (`expandPrefix`, `Service.listObjects`): a key ending in `/` is listed recursively with `ListObjectsV2` (no delimiter, URL-encoded keys, 1,000 per page, SDK retries), and every object is queued as its page arrives, to `<DestDir>\<last name of the prefix>\<relative key>`.
- Zero-byte keys ending in `/` are skipped and counted in `placeholders`. A key ending in `/` that has data is refused `folder-key`. Empty folders are not created locally.
- The folders an accepted object needs are created at that moment, one level at a time, never through a link. A refused object creates nothing. A folder made for a job that is later cancelled or fails stays, empty.
- A prefix with nothing under it is an item error (`not-found`), not a silent success. A single object keeps the 2.2 shape: `<DestDir>\<last name of the key>`.
- Memory grows with the object count, 24 bytes each (measured on 200,000 objects), for the collision check. Nothing else does.

### Bindings and result shape

`QueueUploads` and `QueueDownloads` keep their names and request types (`UploadRequest {profileId, bucket, region, prefix, paths[]}`, `DownloadRequest {profileId, bucket, region, keys[], destDir}`). A folder path or a key ending in `/` is now expanded. The call is still synchronous. Its limit is now 30 minutes (`transferTimeout`), the region lookup inside it keeps 75 seconds, and jobs already queued when it ends stay queued.

`SubmitResult {items[]}`: one item per path or key, in order.

| Field | Meaning |
|---|---|
| `source` | the path or key as requested |
| `kind` | `file`, `folder`, `object`, `prefix` |
| `jobId` | a single file or object that was queued |
| `error`, `code` | the whole item was refused, or a folder or prefix stopped early (`stopped`, `listing`). Jobs queued before that remain |
| `queued` | jobs created by this item |
| `jobIds` | a folder's job ids, the first 1,000 only |
| `refused[]`, `refusedTotal` | `{path, code, reason}` for each file or object not queued, the first 200 only. The total is exact. `path` is the local path (upload) or the object key (download) |
| `examined` | files or objects looked at |
| `emptyFolders`, `placeholders` | skipped, as above |

Go helpers: `SubmitResult.Queued()` (jobs created, folders included) and `Refused()` (refused files plus refused items). A refused single file or object has `error` and `code`, not `refused[]`. `JobView` and the `s4:queue` events are unchanged. A folder's jobs appear in the next event batch as before: call `QueueJobs` after a submit.

### Evidence

| Requirement | Test |
|---|---|
| Nested folder round trip, byte for byte (hash and pattern): empty folders, unicode, zero-byte file, multipart file, no folder objects on S3, journal empty | `TestFolderRoundTrip` |
| Key corpus listed in a prefix and requested alone: each refused with its code and reason, only the 2 safe files written, the parent folder walked | `TestUnsafeKeysAreReportedAndNothingIsWritten` |
| Collisions in both orders, file against folder, existing local file kept (in a folder and as a single object) | `TestCollisionsAndExistingFiles`, `TestCaseCollisionsAreRefusedInEitherOrder`, `TestFileAndFolderNeverShareAName`, `TestExistingFilesAreNeverReplaced` |
| Nothing is written through a link or junction | `TestLinksAreNotWrittenThrough` |
| Nothing escapes the root: every combination of up to 4 of 14 hostile fragments | `TestNothingEscapesTheRoot` |
| Path and temp-path length, exactly at the limit | `TestPathLength` |
| Placeholders skipped, nested prefix, empty prefix | `TestPlaceholdersAndNestedPrefix` |
| Real tree: hidden, system, junction loop, link to outside, dropped link, drive root, missing, relative, duplicates; paths from `describePaths` | `TestFolderUploadOnDisk` |
| Walker and expansion decisions on synthetic folders (vanished, locked, unreadable, partial read, special files, long keys, cancel, engine closed) | `TestWalker*`, `TestExpandPrefix*`, `TestDownloadKeyEntryPoints`, `TestListsAreCapped` |
| One million files expanded with 123 KiB live heap growth; 200,000 listed objects at 24 bytes each | `TestLargeTreeUsesBoundedMemory`, `TestLargeListingMemoryPerObject` (`//go:build !race`) |
| 2.2 behaviour for single files and objects | `TestSingleFilesStillWork`, `TestUploadAndDownloadRoundTrip`, `TestSubmitValidation` (updated: an empty folder is now an item with `emptyFolders`, not an error) |

Every test ends with a goroutine leak check (the rig's cleanup, or `noLeak` in the package tests).

**Tests of the tests.** 28 deliberate bugs, all caught: traversal, reserved names, trailing dot, backslash, forbidden characters, control characters, absolute key, drive letter, name length, case folding off, existing file not checked, links not checked, file/folder clash both ways, path length, temp-path length, collisions never registered, parent names of a single key unchecked, walker following links, placeholders downloaded, walker keeping every key, unbounded refusal list, unbounded job id list, empty folders not counted, duplicate dropped folders, vanished file queued, non-UTF-8 upload name, folders made before checking. One was missed at first: the backslash rule was shadowed by `SplitKey` (the same check earlier in the pipeline), so it is now also tested through `CheckName` alone. The tests also caught a real bug in my first version: the folder hash included a trailing slash, so a file never collided with a folder of the same name (fixed).

### Open risks and proposals

- **Folders are created at queue time**, after the checks, and the sink's later checks do not look at links again. A junction put in the destination between queueing and the job's start could redirect that job. The no-replace commit still protects existing files. Closing this needs a recheck in `engine.FileSink.Prepare`, an engine change that I did not make.
- **Every file is a job.** A 100,000-file folder puts 100,000 jobs in the engine and in `QueueJobs`. Expansion is bounded; the job list is not. Stage 2.4's UI must virtualize the dock, and a cap or grouping may be needed. The 2.2 note (1,000 tracked jobs) still stands.
- **Existing S3 objects are overwritten** by an upload with the same key, as in 2.2. This stage added no check. A `HeadObject` pre-check, or a conditional complete, is a later decision.
- **Long paths:** 259 characters is deliberately conservative (Go's calls would cope with more, Explorer and many tools do not). A long-path opt-in would change `MaxPath` and needs real Windows checks.
- **Unicode normalization** (NFC against NFD) is not compared, because NTFS keeps them apart. Case folding is simple Unicode folding, close to but not identical to NTFS's upcase table, so a rare name pair could differ.
- **Collision winners depend on listing order.** The first key in S3 order keeps the name.
- **Proposal, not built: opt-in safe rename.** A per-request checkbox "rename unsafe names" that replaces forbidden characters with `_`, appends `_` to reserved names and `-2`, `-3` to collisions, and lists every rename in the result with old and new name. The default stays refuse. It needs a UI decision (2.4) and a mapping from the S3 key to the local name that the job and journal can show.
- Not run in the real window. A drop of real Explorer folders (stage 4.3 hands-on check 11) is unverified; the tests feed `describePaths` output instead.

### Next

Stage 2.4 builds the queue UI on the bindings and events above, including the refusal view (`refused[]`, `code`, `reason`), the `emptyFolders` and `placeholders` counts, and a virtualized job list. Not started here.

## Stage 2.4: queue UI, drag-in and transfer settings

**Status:** done, committed as `447ddea`. Full transfers work from the UI in `wails dev -tags devfake`: folder and file uploads, drops, selection downloads (folders included), pause, resume, cancel, cancel all, retry, clear and live settings. `wails build`, `go test -race -count=1 ./...`, `go vet ./...` and `staticcheck ./...` (both also with `-tags devfake`), `GOOS=darwin go vet` (engine, journal, fsx, s3x, fakes3, creds, queue, browse, `cmd/...`) and `tsc --noEmit` pass. No AWS calls were made and `~/.aws` was not read. No engine contract changed.

### What exists

| Area | Files |
|---|---|
| UI bindings: `StartUploads`, `StartDownloads` (listing selection), `ChooseUploadFiles`, `ChooseUploadFolder`, `ChooseDownloadFolder`, quit warning | `transfers_ui.go`, `main.go` (`OnBeforeClose`) |
| Selection to keys, refused when the listing version changed | `internal/browse/listing.go` (`Service.Keys`, `ErrSelectionChanged`) |
| Bounded submit report, unfinished-job count | `internal/queue/submit.go` (`SubmitReport`, `SubmitResult.Report`), `internal/queue/view.go` (`Service.Unfinished`) |
| devfake picker answers (`S4_DEVFAKE_PICK_FILES`, `_FOLDER`, `_DEST`) | `devfake.go`, `devfake_off.go` |
| Queue state: events merged by id, seq gaps and `resync` reload | `frontend/src/shell/useQueue.ts` |
| Dock: tabs, virtualized job rows, bulk actions, banners, last-submit line | `frontend/src/shell/TransferDock.tsx` |
| Settings dialog, refusal list | `frontend/src/shell/SettingsDialog.tsx`, `RefusalDialog.tsx` |
| Upload menu, Download button, "uploads finished here · Reload" | `frontend/src/shell/Browser.tsx` |
| Drop confirmation, pickers, submit flow, top bar caps | `frontend/src/App.tsx`, `TopBar.tsx` |
| Tests | `transfers_ui_test.go`, `internal/queue/report_test.go`, `TestSelectionKeys` in `internal/browse/browse_test.go` |

### Behaviour

- **Upload:** the Upload button opens a menu (Files…, Folder…) with the native pickers, which start in the last folder used (`localStorage`). A **drop** on the listing opens a confirmation naming the items and the destination, and saying that an object with the same key is replaced; Enter uploads. A drop with no bucket open shows a notice instead.
- **Download:** the Download button sends the selection as index ranges, sort and listing version. Go resolves them to keys (`browse.Service.Keys`), so a Ctrl+A over 100,000 rows never sends keys through the page. If the listing changed since the selection was made, the request is refused with "select the items again". The destination picker starts in the last folder used, or Downloads.
- **Reports:** `StartUploads` and `StartDownloads` return a `SubmitReport` (`requested`, `queued`, `refused[]` capped at 200, exact `refusedTotal`, `emptyFolders`, `placeholders`, `examined`). Whole-item failures and folders that stopped early are listed under their own path. The dock shows one line ("Queued 3,003 files to upload · 1 not queued · 1 empty folder skipped") and **Show why** opens the list with a label per code, the reason, and Copy list (tab-separated). `QueueUploads` and `QueueDownloads` are unchanged.
- **Dock:** In progress (submission order), Failed and Done (newest first). Rows are virtualized (`--queue-row-h`). Each shows the name, the S3 key with the local path, the progress bar with bytes and parts, retries in amber, rate and ETA while running, a state chip in words, the reason when waiting or failed, and the commands the job's `can*` flags allow. The engine's full error text is the row's tooltip. The header has the aggregate (running, rate, time left), the settings summary, Pause all or Resume all, and a menu: Resume all, Retry all failed, Clear done and cancelled, Clear all finished, Cancel all (confirmed).
- **Banners:** transfers unavailable (second instance), uploads turned off, journal and cleanup warnings (dismissable), command errors, and a spinner line while a submit is being expanded. That can take minutes for a large folder, and other submits are blocked meanwhile.
- **Settings:** files at once, parts per file, parts in total (with the Go limits), part size (powers of two within the limits), upload and download caps in Mbit/s or MiB/s. Applied live through `SetTransferSettings`. The top bar shows the caps.
- **Listing:** a listing is a snapshot, so the status bar counts uploads that completed under the open folder and offers **Reload**. It never reloads by itself, so a selection is not lost.
- **Quit:** with unfinished jobs, closing the window asks first (native Yes/No), because quitting cancels them, aborts uploads and deletes partial downloads.

### Evidence

Go, through the bindings (`transfers_ui_test.go`), against the HTTPS fake:

- A folder upload with a missing and a relative path gives 3 queued and 2 refusals with their codes.
- Downloading the listing's folder row gets all 3 files byte for byte.
- A size-descending selection of two files that now exist gives two `exists` refusals.
- A stale version is `ErrSelectionChanged`. An empty selection and a closed listing are refused with messages.
- `Unfinished` counts a paused job and not a cancelled one. Closing with nothing unfinished does not ask.
- `TestReportCondensesResult` checks counts, the cap and ordering. `TestSelectionKeys` checks ranges, overlap, version and errors.

Hands-on, `wails dev -tags devfake`, driven through the dev server page in the browser pane at 1440 × 900 and 1024 × 640, dark and light, with temporary AWS files and pickers answered from the environment:

- Folder upload (3 files, a unicode name, an empty folder): reported, completed, and "3 uploads finished here · Reload" showed the new folder.
- File upload (2 files), and a simulated drop of a folder, a file and a missing path: confirmation dialog, then "Queued 4 files · 1 not queued · 1 empty folder skipped".
- Three downloads under a 200 Mbit/s cap ran at about 22.7 MiB/s with an ETA. One was paused and resumed from 168 MiB. The 3 MiB file and the paused 1 GiB file match the seeded pattern (`s4engine verify`: MATCH).
- Downloading the same three again gave three `exists` refusals in the dialog. An 8 GiB download cancelled through Cancel all left no temp file.
- A folder of 40 objects without content showed retries per row. An upload whose source was deleted while queued went to the Failed tab with the reason, and Retry completed it once the file was back.
- 3,003 jobs: the Done tab kept 11 rows in the DOM and scrolled to the last one.

Two layout bugs found this way were fixed: the settings dialog's right column overflowed, and at 1024 px the Upload button wrapped to a second toolbar line.

### Not verified

- **The built exe was not looked at, and no real Explorer drag was made.** The drop was simulated by emitting `s4:drop` in the page. The native pickers and the quit dialog were not seen: the pickers were answered by the devfake variables, and the quit dialog appears only in the real window. Hands-on check 11 (stage 4.3) covers the drag.
- Screen reader behaviour of the dock and dialogs.
- Settings changed outside the dialog (only tests do that) do not refresh the dock label or the top bar until the app restarts.

### Open risks

- **Failure reasons are the engine's text** ("engine: local I/O failed: open source: …"). They are readable but not phrased for users. `queue.failureReason` could map the common cases.
- **Every job stays in the page** as a `JobView` until cleared, about 1 KB each. 100,000 jobs fit, but nothing prompts the user to clear them.
- **A drop is delivered only over the listing** (`--wails-drop-target`), as in stage 1.3. Whether a drop elsewhere in the window is ignored by Wails or reaches Go is unverified in the real window.
- Existing S3 objects are still overwritten by an upload with the same key. The drop dialog says so; the pickers do not ask.
- `.claude/launch.json` (untracked) points the browser pane at a script in the session scratchpad. The app does not need it.

### Next

Stage 3.1 (delete, create folder, presign, metadata view, tag editing) starts from this commit. It is not started here.

## Stage 3.1: object operations

**Status:** done, committed as `fdb02b8`. Delete, create folder, presigned links, the metadata view and tag editing work against the fake S3 in Go tests and in `wails dev -tags devfake`. `go test -race -count=1 ./...`, `go vet ./...` and `staticcheck ./...` (both also with `-tags devfake`), `GOOS=darwin go vet` (objects, fakes3, browse, engine), `tsc --noEmit` and `wails build` pass. No AWS calls were made and `~/.aws` was not read. No engine contract changed.

### What exists

| Area | Files |
|---|---|
| Delete plan and run, versioning check, create folder, presign, metadata, tags | `internal/objects/` (`delete.go`, `objects.go`) |
| Bindings: `PrepareDelete`, `RunDelete`, `DiscardDelete`, `CancelObjectWork`, `CreateFolder`, `PresignObject`, `ObjectMetadata`, `ObjectTags`, `SaveObjectTags` | `objects_ui.go`, fields in `app.go` |
| Selection with folder flags and the listing's bucket; a client for a bucket's region | `internal/browse/listing.go` (`Service.Selection`, `Keys` now uses it), `internal/browse/buckets.go` (`Service.BucketClient`) |
| Fake S3: `DeleteObjects`, object tagging, `GetBucketVersioning`, stored headers and user metadata, presigned-URL expiry | `internal/fakes3/objects.go`, small changes in `core.go`, `server.go`, `fault.go` |
| UI: delete, new folder and share-link dialogs; metadata and tags in the details panel | `frontend/src/shell/ObjectDialogs.tsx`, `ObjectExtras.tsx`, `Details.tsx`, `Browser.tsx`, `App.tsx` |
| Tests | `internal/objects/objects_test.go`, `objects_ui_test.go` |

### Behaviour

- **Delete** (toolbar button or the Delete key) runs in two calls. `PrepareDelete` resolves the selection in Go and lists every object under each selected folder (no delimiter), stores the keys under a plan ID and returns counts, bytes, placeholders, 8 sample keys, the bucket's versioning state and how many unfinished transfers touch those keys. Nothing is deleted. The dialog shows the counts and samples, and wording for each versioning state: off ("cannot be undone"), on (delete markers, versions stay and cost), suspended, or unknown (with the reason). At 100 objects or more you must type `delete`. `RunDelete` deletes exactly the counted keys with `DeleteObjects` in batches of 1,000. A plan runs once and expires after 15 minutes. One delete is capped at 1,000,000 objects. Keys S3 refuses are listed (first 200, with an exact total). A failed request or Stop ends the run, and the keys not sent are counted. The listing reloads afterwards.
- **New folder** writes the zero-byte `name/` placeholder. Names with slashes, leading or trailing spaces, `.`/`..`, control characters, or keys over 1,024 bytes are refused, both while typing and in Go. If anything already exists under `name/`, the request is refused.
- **Share link** signs a GET for 15 min, 1 h, 12 h, 1 day or 7 days. The link lasts for the shortest of the requested time, 7 days and the credentials' remaining life. Credentials are fetched once and the URL is signed with exactly those, so the expiry shown is real. If the credentials expire within a minute, no link is signed. The dialog shows "Works until …", warns when the credentials shortened the link, and copies the link. The URL is never logged.
- **Details panel**, for one object: Share link, metadata from `HeadObject` with checksum mode (content type, ETag, part count, checksum, encryption, version, cache and content headers, object lock, restore, user metadata), and tags. Both are fetched 250 ms after the selection settles. **Tag editing** replaces the whole set (empty removes them). S3's rules are checked in the page and in Go: at most 10 tags, key 1–128 and value up to 256 characters, allowed characters, no `aws:` keys, no duplicates. Before writing, Go reads the tags again. If they differ from the set that was edited, nothing is written and "Reload tags" is offered. Tagging never copies the object.
- **Guard change:** `engine.TestPrefixGuard` now allows exactly one trailing slash, so folder placeholders such as `<prefix>reel/` can be written in the test prefix. `<prefix>/`, `//` and dot segments before a trailing slash are still refused (see `guard_test.go`). Before this stage a trailing slash was refused.

### Evidence

Go, against the HTTPS fake (`internal/objects`, `objects_ui_test.go`):

- A folder of 2,500 objects plus nested folders and placeholders is counted exactly. `a/` does not reach `ab/`, and a file selected twice is counted once. It is deleted in 3 `DeleteObjects` calls, and only the unselected objects remain.
- With the test-prefix guard: a key outside the prefix and a key failed by a rule are listed as `AccessDenied`. In a versioned bucket every delete reports a marker.
- A batch refused as a whole stops after 1,000 deleted with 500 not tried. A cancelled context sends nothing. A delete over the cap is refused while counting.
- Versioning off, suspended, on, and denied (unknown with the error).
- Folder creation: unicode name, exists (folder or placeholder), the invalid names, and an object `taken` next to a new folder `taken/`.
- Presign: a 1 h link downloads 4,096 bytes and gets 403 after the fake clock passes its expiry. 30 days is capped at 7. Credentials expiring in 20 min give a credential-limited link carrying their session token. Credentials with 30 s left are refused, and so is a 1 s link.
- `HeadObject` returns content type, cache control, user metadata, tag count, CRC32 and storage class.
- Tags on a 6 TiB object: set, stale edit refused, cleared, with no PUT or multipart request. Each invalid tag set is refused before it is sent.
- Through the bindings: preview counts, the result, progress events, a plan that runs once, a discarded plan, stale and other-bucket selections, the cap, and every other command. Writes outside the test prefix are denied.

Hands-on, `wails dev -tags devfake` in the browser pane at 1440 × 900 and 1024 × 640:

- Metadata and user metadata of a seeded object. A reserved tag key was flagged while typing. A new tag was saved and shown.
- A 1 h link: the dialog said "59 min from now". The URL downloaded all 3 MiB with `curl`, and a variant two hours old got 403.
- A "Reel 7" folder was created and appeared after the reload. "a/b" was flagged.
- `Reel 7/` plus an 8 GiB file deleted with the Delete key ("versioning is off").
- `dev-media/big/` (versioned): counted 100,040 objects with live progress, showed the delete-marker warning and asked for `delete`. Stopped partway: 69,000 deleted with 69,000 markers, 31,040 not deleted.

One bug was found this way and fixed: a progress event arriving after `RunDelete` returned put the dialog back to "Deleting". Progress now updates only the step it was sent for.

### Not verified

- **No real S3.** Real `DeleteObjects` needs a checksum header, which the SDK adds and the fake ignores. Real presigned-URL expiry, real version IDs and markers, and tags on a real multipart object are for stage 4.3 (checks 9 and 10).
- The built exe was not looked at. Dialogs were checked only in the dark theme.
- Screen reader behaviour of the new dialogs and the tag editor.

### Open risks

- **A delete is a snapshot.** Objects added under a selected folder after counting are not deleted, and the confirmation does not re-count. An upload still running into the folder puts its object back. The confirmation warns when such transfers exist.
- **A failed batch is reported as not tried**, but S3 may have applied part of it before failing. The next listing shows the truth.
- **Tag writes are not atomic.** The re-read narrows the window but cannot close it, because S3 has no conditional tag write.
- **Delete holds up to 1,000,000 keys in memory** (about 100 MB at worst) for up to 15 minutes until confirmed or discarded.
- Deleting in a versioned bucket removes only the current version. There is no way to delete versions or markers, by design (the versioning UI is out of scope).

### Next

Stage 4.1 (independent review of engine, journal and path safety) starts from this commit. It is not started here.

## Stage 4.1: independent review of engine, journal and path safety

**Status:** done, committed as `4b0c86b`. The findings are filed and ranked in `docs/review-4.1.md`. No code was changed. The review used the session's model (Opus 5.5), as the plan assigns.

### Scope and method

The source of `internal/engine`, `internal/journal` and `internal/fsx` was read in full against `docs/engine-design.md`, along with the parts of `internal/queue` (folder expansion, download placement, shutdown) and `internal/s3x` (`GetObjectRange`, `HeadObject`, error mapping) that feed them. Each finding names the file and lines, what happens, a fix and the test that should prove it. One finding (F2) was reproduced with a throwaway test, which was then deleted. The others were traced through the code.

### Result

| Severity | Findings |
|---|---|
| High | F1: free space is checked per job, so parallel large downloads can fill a drive, and each then fails and loses everything it downloaded |
| Medium | F2: a journal from a newer build is moved aside as corrupt, orphaning its uploads (reproduced). F3: retrying a download whose temp file could not be removed always fails |
| Low | F4: a cancel during finalizing is ignored before and between Complete tries. F5: mid-part download progress does not count against the retry budget. F6: two requests for the same destination are both queued. F7 (design): credential wait holds slots and blocks other profiles |
| Info | F8 to F11: "cancelled" after a shutdown-time success, 8.3 short names, link check timing, no end-to-end download checksum |

No path traversal, data race or deadlock was found. The review lists what was checked and found sound, and two rules later changes must keep: the progress observer must never call the engine, and queue code must not call the engine while it holds `Service.mu`.

### For stage 4.2

- F1 part 2 (a full disk keeps the job resumable) and F7 change engine contracts or the state table. The plan says those go to Opus. F1 part 1 (a reservation per volume), F2 to F6 and the optional F8 to F10 fit Sonnet.
- Every finding has a test to write first.

### Next

Stage 4.2 (fix findings, run the full suite under the race detector) starts from this commit. It is not started here.

## Stage 4.2: fix review findings, full suite under the race detector

**Status:** done, committed as `9656b8a`. F1 to F7 from `docs/review-4.1.md` are fixed, each with a test written for it. `go test -race -count=1 ./...` passes in every package. `go vet ./...` and `staticcheck ./...` (both also with `-tags devfake`) and `GOOS=darwin go vet` (engine, fsx, journal, queue) pass. The new tests also passed 10 runs in a row under `-race`. No AWS calls were made and `~/.aws` was not read. The work used the session's model (Opus 5.5), because F1 part 2 and F7 change engine contracts. `docs/engine-design.md` is updated to match.

### What changed

| Finding | Fix | Files |
|---|---|---|
| F1 (High), part 1 | `FileSink` reserves space per volume: each open temp file counts the bytes it still has to write, or nothing if its clusters were taken at Create. A new download must fit in free space after the others' remainder and the 64 MiB reserve. Checking and creating happen under one lock. A file stops counting at Commit or its first Abort. | `internal/engine/filesink.go`, `internal/fsx/space.go`, `fsx.VolumeOf` and `Preallocated` in `strategy_windows.go` and `strategy_other.go` |
| F1 (High), part 2 | A write that finds the drive full returns `ErrDiskFull` (new sentinel, permanent). The scheduler interrupts the part and pauses the job instead of failing it. `JobSnapshot.PauseReason` (new field) is `DiskFullNotice`, and the queue shows it as the job's reason. The temp file, journal entry and written bytes are kept, and Resume continues. A full drive while creating the file is `ErrInsufficientSpace`. | `internal/engine/upload.go` (`pauseForDiskFull`), `api.go`, `job.go`, `errors.go`, `internal/queue/view.go` |
| F2 (Medium) | `journal.Decode` reads the version first, without strict fields. A newer journal returns `ErrUnsupportedVersion` and stays in place, whatever new fields it holds. A file with no version, or one that is not a JSON object, is still corrupt. | `internal/journal/journal.go` |
| F3 (Medium) | `FileSink.Prepare` removes a leftover temp file with the job's own name before creating it. If that fails, the job fails saying so, and can be retried later. | `internal/engine/filesink.go` |
| F4 (Low) | Cancel during finalizing waits only while a request is in flight. `advance` applies a waiting cancel before sending the next finalizing request, so a cancel made while the source was being verified sends no Complete. A cancel during a Complete backoff applies at once. Once the object or file is in place, the job still finishes as completed. | `internal/engine/scheduler.go` (`cancelJob`), `upload.go` (`advance`) |
| F5 (Low) | A download attempt that wrote bytes resets its part's attempt count and the progress clock, because the next attempt resumes after those bytes. An attempt that wrote nothing is charged as before. | `internal/engine/download.go` |
| F6 (Low) | `SubmitDownloads` reads the destinations of unfinished downloads (not completed or cancelled; failed counts, because Retry writes there) from one engine snapshot. It refuses a destination already in use, ignoring case on Windows and macOS, with code `exists`. Requests are placed one at a time under a new `placeMu`, never held with `Service.mu`. A folder's files get the same check. | `internal/queue/submit.go`, `expand.go`, `queue.go` |
| F7 (Low, design) | A job in credential wait keeps its slot but does not count against `MaxActiveFiles`. Queued jobs of the waiting profile are set aside in order and return to the front of the queue when no job of that profile waits any more (credentials restored, or the waiting jobs paused or cancelled). After a wait, running jobs can exceed the file limit until enough finish. Parts stay within `MaxActiveParts`. | `internal/engine/scheduler.go` (`pump`, `slots`, `releaseHeld`) |

Contract changes: `ErrDiskFull`, `DiskFullNotice` and `JobSnapshot.PauseReason` are new. The engine can now pause a job itself. Credential-wait slots no longer count toward admission. Cancel during finalizing is deferred only while a request is in flight. No state-table edge was added: the full-drive pause uses the existing running → pausing edge.

### Evidence (tests)

- `internal/engine/diskspace_test.go`:
  - `TestFullDrivePausesAndKeepsTheDownload`: a strategy that fills after 20 MiB pauses the job with the reason, keeps the temp file and journal entry, and on Resume fetches no more than the missing bytes. Every byte matches.
  - `TestFullDriveThenCancelRemovesTheTempFile`.
  - `TestSinkReservesSpaceAcrossDownloads`: the second of two downloads that fit alone is refused, the reservation shrinks as bytes are written, Abort returns it, and another volume is separate.
  - `TestPreallocatedFileReservesNothing` (Windows).
  - `TestParallelDownloadsToOneDriveAreAdmittedInTurn`: through the engine, the second download fails before fetching anything and the first completes.
  - `TestRetryAfterTempFileWasLeftBehind`: the first abort leaves the file, and Retry completes.
- `internal/engine/finalize_test.go`:
  - `TestCancelWhileVerifyingSourceSendsNoComplete`: no Complete is sent and no object is created.
  - `TestCancelWhileCompleteWaitsForRetry`: with a one-hour backoff, the cancel applies at once and only one Complete was sent.
  - `TestDownloadFinishesThroughRepeatedResets`: every GET is reset after 1 MiB, so each part needs 5 attempts with a budget of 3. The download completes with each byte written once.
  - `TestDownloadWithoutProgressStillFails`: a reset before any byte arrives is charged.
  - `TestCredentialWaitDoesNotBlockOtherProfiles`: with one file at a time, the other profile's job completes during the wait, the waiting profile's queued job stays queued, and both finish once credentials return.
  - `TestHeldJobsReturnWhenTheWaitEndsWithoutCredentials`.
- `internal/journal`: a version-2 journal with unknown fields and kinds is `ErrUnsupportedVersion` and is left untouched by `OpenFile`. A missing or non-numeric version, and a non-object, are corrupt.
- `internal/fsx`: `IsDiskFull` matches the two Windows codes and the sentinel, and nothing else. `VolumeOf` gives the same lower-case mount point for a folder and its parent.
- `transfers_test.go` `TestSecondDownloadToSameDestinationIsRefused` (through the bindings): the same key again, and another key whose name differs only in case, are refused with `exists` while the first download runs. The first then completes. `internal/queue` `TestFolderDownloadKeepsSubmitRefusalCode`.
- The existing tests, including the fault suite, pass unchanged. `TestSubmitValidation` was only reformatted by `gofmt`.

### Not fixed

- **F8 to F11 (Info)** are left as filed. F11 (no end-to-end download checksum) belongs in the README's known limits (stage 4.4).
- **A full drive during commit** (the final sync or rename) still fails the job and removes the temp file, because finalizing cannot pause. The bytes are all written by then, so this needs metadata space only and is unlikely.

### Not verified

- **No real full drive.** The full-drive tests use a strategy that reports `ErrDiskFull`. The mapping from `ERROR_DISK_FULL` is unit-tested, but no download has filled a real volume. A small VHD or a quota would show it, if wanted in stage 4.3.
- **The UI was not looked at.** The paused reason reaches `JobView.Reason`, which the queue already shows for paused jobs. It was not seen in the running app. A refused duplicate destination appears in the existing "not queued" report.
- `GetVolumePathName` on a folder mounted from another volume, and on network shares, was not tried. A failure there makes `Prepare` fail with a clear message rather than skip the check.

### Open risks

- **Over the file limit after a credential wait.** When credentials return, every waiting job resumes alongside the jobs admitted meanwhile, up to twice `MaxActiveFiles`. Parts, and so memory and connections, stay within their limits. Open file handles and part tables do not.
- **F4 cancel after a failed Complete.** A Complete that failed retryably may have succeeded. Cancelling during its backoff aborts, which finds `NoSuchUpload`, and the job ends cancelled although the object exists. This was already possible before (see F8).
- **F6 holds a failed download's destination** until it is removed from the queue, because Retry would write there. The refusal says so.

### Next

Stage 4.3 (real AWS verification with your hands-on checks) starts from this commit. It is not started here.

## Stage 4.3: real AWS verification

**Status:** done, committed as `9319b03`. Every real-AWS and hands-on check in the plan's Verification list (7 to 11) passed on 2026-11-20. The user did the drag-in checks and started the uploads for the cancel, kill and credential checks. `go test -race -count=1 ./...`, `go vet ./...` and `staticcheck ./...` (both also with `-tags devfake`) and `GOOS=darwin go vet` (`cmd/...`, engine) pass on the new code.

### Target

- **Profile:** `<PROFILE>` (session keys from the shared credentials file).
- **Bucket:** `<BUCKET>`, us-west-2.
  - Versioning is on.
  - Default encryption is SSE-KMS.
  - Lifecycle rules expire noncurrent versions after 15 days and abort incomplete multipart uploads after 7 days.
- **Prefix:** every write went to `upload-test/s4b-43-20261120/`, enforced in the app (new `S4_WRITE_PREFIX`) and in the CLI (new `--root`).
- `upload-test/` was empty before the run. Nothing outside the prefix was written or deleted.

### Results

| # | Check | Result | Evidence |
|---|---|---|---|
| 7 | 10 GiB round trip at 80% of line rate, matching hashes | **Pass** | `s4engine`, defaults. Upload 297.7 MiB/s (2.50 Gbps), steady 298.0, slowest 10 s 286.7, 0 retries, peak heap 8.6 MiB. Download 247.3 MiB/s (2.07 Gbps), steady 241.8, slowest 10 s 159.8 (the end of the run), 0 retries, peak heap 34.3 MiB. SHA-256 match. Pass mark 200 MB/s from stage 0.7 |
| 8 | Credentials expire during an upload, and the transfer recovers | **Pass** | Profile `s4-credtest` uses the new `s4credtest` helper with a 3-minute expiry. Upload of 10 GiB at a 25 MiB/s cap. Bad credentials handed out from 21:56:10. At 21:57:56 the job was in `credential-wait` with 33 of 160 parts, 0 retries and the waiting reason shown. It stayed at 33 parts while probes ran 5, 10, 20 and 40 s apart. Good credentials restored at 21:59:05, and the next probe resumed the job without any click. It completed with 160 parts and 0 retries, and a download of the object matched the source |
| 9 | No multipart uploads under the prefix after cancel and after relaunch | **Pass** | Cancel in the queue aborted `cancel-me.bin` at once, and no object was created. `kill-me.bin` was force-killed at 21:47:15 with its upload and journal entry left. The relaunched app had aborted it and emptied the journal by 21:47:36 |
| 10 | Presigned URL works before expiry and fails after. Tags on a multipart object | **Pass** | 15-minute link from the app: a 1 MiB ranged GET was 206 and matched the source. At 21:53:15, after the expiry the dialog showed (21:52:37), 403 `AccessDenied`. Tag `s4-stage=4.3` saved on the 160-part object, confirmed with the AWS CLI. Still one version with the same ETag, so nothing was copied |
| 11 | Drag in a file, a folder and a mixed selection from Explorer | **Pass** (the user) | `single.bin` (70 MiB), `shoot/` (3 files in 2 levels) and a mixed `m1.bin` plus `m-folder/`. All 7 objects arrived with their sizes. `shoot/day2/c.mov` (200 MiB) downloaded and matched |

Also seen:

- **Deferred launch cleanup, real S3.** An app restart (see below) left `credtest.bin`'s upload open while the helper handed out bad credentials. Launch cleanup reported it would clean up when `aws:s4-credtest` worked again, and left it. After good credentials and "Load credentials again", it aborted the upload. Nothing was left on S3 and the journal was empty.
- **Write guard in the app.** "New folder" in `upload-test/` was refused before any request: "write refused by guard: key "upload-test/guard-probe/" is outside "upload-test/s4b-43-20261120/"".
- Metadata of the real object showed the 160 parts, the composite CRC32, SSE-KMS with its key and the version ID.
- The new F1 reservation code ran in three real 10 GiB downloads without a problem.

### New code

| What | Files |
|---|---|
| `engine.NewPrefixGuard(prefix)`: a guard for any prefix of at least two levels. A mistyped prefix is refused | `internal/engine/guard.go`, `guard_test.go` |
| `S4_WRITE_PREFIX` in the app. When set, the queue's engine gets the guard, and delete (when counting), new folder and tag saves check every key before any request. An invalid value refuses every write rather than turning the guard off. Unset means no change | `guard.go`, `guard_test.go`, `app.go`, `transfers.go`, `objects_ui.go` |
| `s4engine --root` places `<run-id>/` under another prefix (default `s4browser-test/`). Guard, cleanup and results follow it | `cmd/s4engine/target.go`, `run.go`, `main.go`, `cli_test.go` |
| `s4credtest`, a `credential_process` helper for check 8. It hands out a source profile's credentials with a short expiry. A control file switches between `valid`, `broken` (damaged session token, so S3 answers `InvalidToken`) and `fail`. Each run is logged without key material | `cmd/s4credtest/` |

**A first version of `broken` changed only the secret, and the upload kept working.** The SDK's SigV4 signer caches the derived signing key by access key ID and day, without the secret. A credential change that keeps the access key ID therefore has no effect until the next day. Real expiry changes or invalidates the session token, which the fixed helper now does. This matters only to tests that fake bad credentials.

### Findings for later

The user's two new requirements, R1 (refresh the listing after uploads) and R2 (keep profile, bucket and folder across a window reload, with Refresh meaning "reload this folder"), are in the implementation plan under the new stage 4.5. Three smaller issues are listed with them:

- The bucket list stays empty after credentials recover, until the window is reloaded.
- The deferred-cleanup warning stays after the cleanup ran.
- The profile badge says "expired" for a `credential_process` profile whose helper already gave new credentials.

Two things seen are only artifacts of the test setup:

- **The drop prompt appeared in both views.** In `wails dev` it showed in the native window and in the browser pane. A built exe has one window.
- **Rebuilding a Go file restarted the app.** `wails dev` restarts it when any Go file in the repo changes, which cut off the first check-8 upload. It is covered by the kill test, and was redone.

### Not verified

- **Long runs:** the real runs were short, as in stage 0.7.
- **A real session expiry:** check 8 used a damaged token, not a session reaching its end. S3's `ExpiredToken` code is still only tested against the fake.
- **The built exe:** the app ran as `wails dev`. The built exe was not run against AWS.

### Left behind

- **In the bucket, under `upload-test/s4b-43-20261120/`:**
  - `clip10g.bin` and `credtest.bin` (10 GiB each), plus 5 small files (about 312 MiB), are current objects.
  - No unfinished multipart uploads remain.
  - Nothing was deleted, so there are no delete markers. The aborted uploads left no parts.
- **On this machine, in `C:\s4gate\s43\`:**
  - The 10 GiB source and its hard links.
  - Three downloads (about 30 GiB).
  - The test AWS config and the helper's log.
  - The two built tools.
- `.claude/launch.json` (untracked) has a `wails-real-43` entry.

### Next

Stage 4.4 (release build, README, deferred Mac checklist), or stage 4.5 first if the user wants R1 and R2 in the release. Neither is started here.

## Stage 4.5: listing refresh and location kept across a reload

**Status:** done, committed as `68fe608`. It was done before stage 4.4, as the user chose. R1, R2 and the three smaller issues from stage 4.3 are fixed. `go test -race -count=1 ./...`, `go vet ./...` and `staticcheck ./...` (both also with `-tags devfake`), `GOOS=darwin go vet` (browse, queue, journal, creds), `tsc --noEmit` and `wails build` pass. No AWS calls were made in this stage.

### What changed

| Item | Behaviour | Files |
|---|---|---|
| R1, refresh after upload | Uploads that finish into the folder on screen, or below it, reload its listing on their own. The first reload comes 1 s after the first finished upload, and later reloads at most every 5 s, so a folder upload of many files does not relist the folder per file. A folder of more than 20,000 rows keeps the "N uploads finished here · Reload" button instead, because relisting it is costly | `frontend/src/App.tsx` |
| Reloads keep the view | Every reload of the folder on screen keeps the scroll position, and the selection and focused row on the same objects. That covers the Reload button, F5, a new folder, a delete and R1. Before the reload, the selected keys (up to 10,000) are read from Go, and afterwards they are located in the new listing by key. Rows added above them no longer shift the selection onto other objects, and deleted rows drop out of it. Before this stage, a reload kept the selection only when the row count was unchanged | `frontend/src/shell/Browser.tsx`, `internal/browse/listing.go` (`Service.Locate`, `MaxLocate`), `browse.go` (`ListingKeys`, `ListingLocate`) |
| R2, location kept on reload | The profile, bucket and folder are kept in `sessionStorage`. A reload of the window (the WebView's right-click Refresh) returns to them once the profiles have loaded, if that profile is still the active one. A restart still opens with no bucket. F5 and Ctrl+R reload the folder's listing and no longer reload the page | `frontend/src/settings.ts`, `App.tsx`, `Browser.tsx` |
| Bucket list after credentials return | Go sends `s4:credentials` whenever a profile's credentials change (keys added, "Load credentials again", and so on), and the sidebar reloads a failed list then. A failed list also retries 4 times, 15, 30, 60 and 120 s apart, for credentials that return without a change the app hears of. A role refused `ListBuckets` (`listDenied`) is not retried | `app.go` (`emit`), `frontend/src/shell/Sidebar.tsx` |
| Deferred-cleanup warning | `journal.Report.Deferrals` holds the warning of each entry a pass left in the journal, by entry ID. The queue shows a deferral warning only while its entry is still pending, so "will be cleaned up when the credentials … work again" disappears once the cleanup has run. Other warnings, such as a path that was not a temp file, stay | `internal/journal/journal.go`, `recover.go`, `internal/queue/queue.go` |
| Profile badge expiry | When the badge's expiry passes, the window asks `CurrentCredentials`, which reads the profile's credential cache without discarding good credentials or notifying anyone (`creds.Manager.Current`). A `credential_process` profile then shows the expiry of the credentials its helper gave last, rather than "expired" | `internal/creds/manager.go`, `profiles.go`, `frontend/src/shell/useProfiles.ts` |

### Evidence

- **Go tests:**
  - `browse.TestLocateAfterReload`: keys selected before objects were added are found at their new indexes, in both sort directions. A missing key is -1, and more than `MaxLocate` keys are refused.
  - `creds.TestCurrentDoesNotReload`: three `Current` calls run the helper once and tell no listener, and `Reload` runs it again and tells them.
  - `TestLaunchCleanupResumesWhenCredentialsChange` now also checks the warnings. The deferral warning is shown while the cleanup waits and is gone after it ran, and the "not a temp file" warning stays.
- **Hands-on** in `wails dev -tags devfake`, in the browser pane at 1440 × 900:
  - In `dev-small/downloads`, `sample2.bin` and `sample3.bin` were selected, then `a.bin` and `b.bin` were uploaded. The listing reloaded on its own, showed the two new files above, and kept `sample2.bin` and `sample3.bin` selected, with the focus on `sample3.bin`.
  - F5 reloaded the listing with the selection kept, and the page itself was not reloaded (a marker in `window` survived).
  - A full page reload came back to `dev-small/downloads`.
  - A folder upload (`shoot/`) made the new folder appear without a reload.

### Not verified

- **The native window:** F5, Ctrl+R and the right-click Refresh were tried only in the browser pane. Wails turns the WebView2 context menu off in production builds (its `EnableDefaultContextMenu` defaults to false), so the built exe should have no Refresh entry. Whether WebView2 lets the page cancel F5 was not checked.
- **Real S3:** this stage was not run against it. Nothing here changes requests to S3, except the extra `ListBuckets` retries.
- **Screen readers:** the focus restored after a reload was not checked with one.

### Open risks

- **The selection is not kept across a full page reload,** only the location. It is index ranges of a listing that the reload closes.
- **Auto-reload relists the whole folder,** up to 20,000 rows, at most every 5 s while uploads finish into it. The 20,000 limit is a guess and was not measured on real S3.
- **A deleted object's row drops out of the selection silently.** The delete dialog already said what was deleted.

### Also

The app's real profile store (`%AppData%\s4browser\profiles.json`) still has `aws:s4-credtest` active from stage 4.3. That profile exists only in the test AWS config, so a normal launch shows "No profile" until another profile is chosen.

### Next

Stage 4.4 (release build, README, deferred Mac checklist). It is not started here.

## Stage 4.4: release build, README and deferred Mac checklist

**Status:** done, not committed (waiting for the user's go-ahead). The release exe runs from a clean folder. No Go or frontend code changed in this stage, only build settings, the icon and documentation. `GOOS=darwin GOARCH=arm64 go vet` passes for every package, including `main`. No AWS calls were made.

### Release build

- **Command:** `wails build -clean -trimpath -ldflags "-s -w"`.
- **Output:** `build\bin\s4browser.exe`, 21.2 MiB.
  - SHA-256 `33C06A5DEDFD5720380B8AC47D56C24B0654C40D29ACD82C04AEB640381B73AB`. Two builds from the same tree gave the same hash.
  - Build tags: `desktop,wv2runtime.download,production`. No `devfake`.
- **Version:** 1.0.0, from the new `info` section of `wails.json`. Windows' version API reads ProductName `s4browser`, ProductVersion and FileVersion `1.0.0`, the copyright and the comment.
- **The version resource needed two fixes** in `build/windows/info.json`:
  - Its language key was the template's `0000`, which Windows does not show.
  - The obvious fix, `040904B0`, is silently dropped by `tc-hib/winres`, the library Wails uses, which expects a 4-digit language ID. The key is now `0409`, and a `FileVersion` string was added.
  - .NET's `FileVersionInfo` still reports the strings as empty, although `GetFileVersionInfo`/`VerQueryValue`, which Explorer uses, return them. Use the Win32 API to check.
- **Icon:** `build/appicon.png` was the Wails "W". It is now drawn from the app's own logo: two overlapping rounded squares in the dark theme's accent blue on its background colour. `build/windows/icon.ico` was regenerated from it by Wails.

### Clean-folder run

- The exe was copied alone into a new folder (`C:\s4gate\release-check\`) and started.
- It ran with the window title "s4browser", and a capture of the window showed the full UI and the new icon.
- It closed normally (`CloseMainWindow`), and the folder still held only the exe. The folder was then removed.
- Data lives in `%AppData%\s4browser\`, and WebView2's cache in `%AppData%\s4browser.exe\` (Wails' default).
- The window said "No profile", because `profiles.json` still names `aws:s4-credtest` from stage 4.3 (see "For you").

### Documentation

- **`README.md`** replaces the Wails template's:
  - install and data locations
  - credential types
  - what each part of the app does
  - known limits, including F11 (no end-to-end download checksum), no resume after closing, and versioned-bucket deletes
  - building and testing
  - the test tooling (`s4engine`, `s4credtest`, `S4_WRITE_PREFIX`).

  Its file names and limits were checked against the code.
- **`docs/mac-checklist.md`** has:
  - where portability stands
  - the platform split by file
  - decisions to make (bundle ID, signing, Keychain prompts, window options)
  - code to check: hard-link rename on exFAT and SMB, Unicode normalization of keys, Windows path rules, `F_FULLFSYNC`, send buffer limits, helper `PATH`, the data folder
  - a smoke test.

### Not verified

- **Other machines:** the exe ran only on this one, which has WebView2 157. The WebView2 download bootstrapper, for machines without the runtime, was not tried.
- **Real S3:** the release exe was not run against AWS. The stage 4.3 checks ran `wails dev` builds of the same code, before the stage 4.5 changes.
- **macOS:** nothing has been built or run on a Mac.

### For you

- **Active profile:** `%AppData%\s4browser\profiles.json` has `aws:s4-credtest` active. Pick your usual profile in the app once, and it is saved.
- **Test leftovers:**
  - The test objects from stage 4.3 are still under `upload-test/s4b-43-20261120/` in `<BUCKET>`.
  - About 40 GiB of test files are in `C:\s4gate\s43\`.
  - `.claude/launch.json` (untracked) has the `wails-real-43` entry.

### Next

v1 is complete per the plan. Follow-ups, including the Mac build, are in [roadmap.md](roadmap.md). Cleaning up the stage 4.3 test objects is left to you.

## Stage 5.1: Mac build

**Status:** done. On a Mac with Apple silicon (macOS 27.2, Go 1.27.4, Node 26.13.1, Wails 2.17.1), `wails build -platform darwin/arm64` makes `build/bin/s4browser.app`. `go test -race -count=1 ./...`, `go vet` and `staticcheck` (both also with `-tags devfake`, and with `GOOS=windows`) and `tsc --noEmit` pass. No AWS calls were made.

### What changed

| Item | Behaviour | Files |
|---|---|---|
| Bundle | Bundle ID `com.dprestegard.s4browser`, minimum macOS 13.0 (Go 1.27's minimum). Build with `CGO_CFLAGS`/`CGO_LDFLAGS=-mmacosx-version-min=13.0`, or the linker warns, because Wails defaults to 10.13 | `build/darwin/Info.plist`, `Info.dev.plist`, README |
| Title bar | Wails' theme calls are no-ops on macOS, so a normal title bar would follow the system appearance, not the app's theme. The title bar is hidden and inset (`mac.TitleBarHiddenInset`). On a Mac the top bar is 52 px, leaves 84 px for the traffic lights, and drags the window by its wordmark and empty space. Also an About box with the icon | `main.go`, `frontend/src/main.tsx`, `styles/shell.css` |
| Wording | "macOS Keychain" and "Match macOS" on a Mac, from the user agent | `frontend/src/ui/platform.ts`, `shell/TopBar.tsx` |
| No-replace rename | `renamex_np(RENAME_EXCL)` on APFS and HFS+. Then the hard link. On exFAT and FAT32, which support neither (tried on an exFAT disk image: both return `ENOTSUP`), it checks that the name is free and renames, which is not atomic | `internal/fsx/rename_darwin.go`, `rename_unix.go`, `strategy_other.go` |
| Helper `PATH` | A `credential_process` helper on a Mac gets `/opt/homebrew/bin` and `/usr/local/bin` added to its `PATH`, because an app started from Finder has only launchd's | `internal/creds/process_other.go` |
| Test | `TestFolderUploadOnDisk` used `\` as the volume root. It now uses the platform's separator | `transfers_folders_test.go` |

### Evidence

- The only test that failed on the Mac before these changes was `TestFolderUploadOnDisk` (the Windows root).
- `TestRenameNoReplaceDarwin` passes on APFS and, with `TMPDIR` on a mounted exFAT image, on exFAT. On exFAT, `TestRenameNoReplace/link` (the bare hard link) and `TestUnsafeKeysAreRefusedWithReasons` (an AppleDouble `._dest` file appears) fail, as expected for that volume; neither runs there normally.
- `TestHelperPathAddsHomebrew`.
- A throwaway Keychain entry was set, read and deleted through `osVault` with no prompt.
- The release app ran: data in `~/Library/Application Support/s4browser/` (`journal.lock`, `filedrop.log`), WebKit data in `~/Library/WebKit` and `~/Library/Caches` under the bundle ID. A second copy (`open -n`) starts and holds no journal lock.
- In `wails dev -tags devfake` in a browser: the Mac top bar styles apply, buttons are not drag regions, and the Add keys dialog and theme menu use the Mac wording.

### Not verified

- **The native window was not seen:** Accessibility and Screen Recording access were not granted to Claude, so the title bar, icon, About box, Finder drops and the second copy's message were not looked at.
- Real S3, a full volume, kill-and-relaunch, and a real `credential_process` helper. See [roadmap.md](roadmap.md).

### Next

The Mac release checklist in [roadmap.md](roadmap.md).

## Stage 5.2: CI builds

**Status:** done on branch `ci/build-workflow`. `.github/workflows/build.yml` builds `s4browser-<version>-windows-amd64.exe` on `windows-latest` and `s4browser-<version>-macos-universal.zip` on `macos-latest`, after the frontend build (which runs `tsc`), `go vet` and `staticcheck` (both also with `-tags devfake`), `go test -race -short -count=1 ./...`, and on Windows `go run ./tools/notices -check`. It runs on `v*` tags and by hand only. A tag must match `productVersion` and creates a draft GitHub Release.

### What changed

| Item | Behaviour | Files |
|---|---|---|
| Workflow | One job per OS (checks, then build), Go 1.27.4 exact (the notices file records it), Node 26, Wails 2.17.1, staticcheck 2026.2.2. The frontend is built before vet, because `main.go` embeds `frontend/dist`. Actions are on their Node 24 majors | `.github/workflows/build.yml` |
| Mac build | `darwin/universal` with `-mmacosx-version-min=13.0`. `lipo` must report both architectures. Zipped with `ditto` to keep the ad hoc signature | same |
| Docs | CI builds section, quarantine note, roadmap notes on universal builds and signing secrets | README, `docs/roadmap.md` |

### Evidence

- Run 44991003548: both jobs pass. Every package passes `-race -short` on both OSes, `notices -check` passes, and `lipo` reports `x86_64 arm64`.
- Duration: macOS about 6 to 7 minutes and Windows about 12 to 15 minutes, so roughly 90 to 100 billed minutes per run in this private repo (macOS at 10x, Windows at 2x).
- `-short` skips the 32 GiB and 2 TiB engine runs and the 100,000-object listing, so CI does not run them.

### Not verified

- **The release job has not run:** no `v*` tag has been pushed.
- **The CI-built apps were not launched,** neither the exe on Windows nor the universal app on a Mac. The universal app has not run on an Intel Mac.
