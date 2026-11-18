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
