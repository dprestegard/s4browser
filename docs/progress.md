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
