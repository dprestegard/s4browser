# Engine design

This document and the Go contracts are normative. The contracts live in `internal/engine`, `internal/journal`, `internal/fsx` and `internal/s3x`. If an implementation stage needs a contract change, escalate to Opus as the plan requires.

Facts were checked on 2026-11-17 against the AWS documentation and these SDK versions: `aws-sdk-go-v2` v1.48.0, `service/s3` v1.115.0, `service/internal/checksum` v1.11.13.

## 1. Packages and dependencies

```
internal/engine  -> internal/journal, internal/fsx       (no AWS SDK, no OS-specific code)
internal/s3x     -> internal/engine, AWS SDK             (implements engine.ObjectStore)
internal/fakes3  -> internal/engine                      (implements engine.ObjectStore, serves HTTPS)
```

`internal/engine` and `internal/journal` must pass `GOOS=darwin go vet`. Windows code goes in `_windows.go` files in `internal/fsx`.

## 2. State machines

The tables are code: `NextJobState` and `NextPartState` in `internal/engine/state.go`. Exhaustive tests fix every edge. The scheduler must change state only through these functions.

### Job states

| State | Slot held | Meaning |
|---|---|---|
| `queued` | no | Waiting for an active-file slot |
| `running` | yes | Dispatching work |
| `pausing` | yes | Attempts cancelled, waiting for them to exit |
| `paused` | no | Nothing in flight. Part table, upload ID and open handles are kept |
| `credential-wait` | yes | No new dispatch. Waiting for a probe to pass |
| `cancelling` | yes | Draining, then cleaning up |
| `failing` | yes | Same as `cancelling`, caused by a fatal error |
| `cancelled`, `completed` | no | Terminal |
| `failed` | no | Terminal unless the user retries |

### Job transitions

| From | Event | To | Trigger |
|---|---|---|---|
| queued | Admit | running | Scheduler grants a slot, FIFO |
| queued | Pause | paused | User |
| queued | Cancel | cancelling | User or engine close |
| running | Pause | pausing | User, not allowed in `PhaseFinalizing` |
| running | Cancel | cancelling | User or engine close, deferred in `PhaseFinalizing` |
| running | CredentialError | credential-wait | Any request on the profile returns `ClassCredential` |
| running | Fatal | failing | `ClassPermanent`, or retry budget exhausted |
| running | Finished | completed | Object complete and durable |
| pausing | Drained | paused | Last attempt exited |
| pausing | Cancel | cancelling | User or engine close |
| pausing | Fatal | failing | An attempt returned a permanent error while draining |
| paused | Resume | queued | User. The job must be admitted again |
| paused | Cancel | cancelling | User or engine close |
| credential-wait | CredentialsRestored | running | Probe passed |
| credential-wait | Pause | pausing | User |
| credential-wait | Cancel | cancelling | User or engine close |
| credential-wait | Fatal | failing | An in-flight attempt returned a permanent error |
| credential-wait | Finished | completed | A Complete already in flight succeeded |
| cancelling | CleanupDone | cancelled | Attempts drained and cleanup finished |
| failing | CleanupDone | failed | Attempts drained and cleanup finished |
| failed | Retry | queued | User. All parts reset, new upload ID, new journal entry |

Commands rejected by the table return `ErrIllegalState`. Asynchronous events rejected by the table are ignored, because they can race with a command that already moved the job.

**Finalizing:** during `PhaseFinalizing`, Pause returns `ErrIllegalState`. Cancel sets a flag and returns. If finalizing succeeds the job is `completed`, otherwise the flag sends it to `cancelling`.

**Cleanup:** an upload aborts its multipart upload and deletes the journal entry. A download calls `SinkWriter.Abort` and deletes the journal entry. If the abort fails after `MaxAbortAttempts`, or hits a credential error, the job still ends. The journal entry stays, `JobSnapshot.Warning` says so, and the abort is retried at next launch.

### Part states

| From | Event | To | Attempt charged |
|---|---|---|---|
| pending | Dispatch | in-flight | no |
| in-flight | Succeed | done | no |
| in-flight | Retry | backoff | yes |
| in-flight | Interrupt | pending | no |
| in-flight | Fail | failed | no |
| backoff | BackoffElapsed | pending | no |
| backoff | Interrupt | pending | no |
| pending, done, failed | Reset | pending | counters cleared |

Interrupt covers pause, cancel, preemption and credential errors. Entering `pausing`, `cancelling` or `failing` interrupts all parts in backoff. A part in `done` is never sent again unless the user retries a failed job.

## 3. Concurrency model

### Goroutines

| Goroutine | Count | Owner | Exits when |
|---|---|---|---|
| Scheduler | 1 | `New` | `Close` finishes step 6 |
| Watchdog | 1 | Scheduler | Its context ends |
| Progress | 1 | Scheduler | Its context ends, after one final batch |
| Part attempt | at most `MaxActiveParts` | Scheduler | It sends exactly one result |
| Control request (Create, Complete, Abort, Head, commit) | at most `MaxActiveFiles` plus one abort per closing job | Scheduler | It sends exactly one result |
| Credential probe | at most 1 per profile | Scheduler | It sends exactly one result |

**The scheduler is the single owner of all job and part state.** Every other goroutine communicates with it through two channels, `commands` and `results`. No mutex protects job state.

Rules that prevent leaks:

- Every spawned goroutine is registered in one `sync.WaitGroup` before it starts.
- Every attempt runs under a context derived from its job's context, created with `context.WithCancelCause`.
- The scheduler keeps receiving from `results` until the WaitGroup reaches zero, so a worker can never block forever on send.
- Backoff uses one scheduler-owned timer over a min-heap of retry times. No goroutine sleeps for a retry.
- A response body is closed by the attempt that opened it, with `defer`, on every path.

### Limits and dispatch

The scheduler admits queued jobs FIFO while active jobs are below `MaxActiveFiles`. It then assigns parts round-robin across running jobs while both limits hold:

- in-flight parts of the job < `MaxPartsPerFile`
- in-flight parts overall < `MaxActiveParts`

Within a job the lowest-numbered pending part goes first. Use a cursor plus a min-heap of returned parts, never a scan of the part table.

### Settings changes

Limits are plain values read by the scheduler, so no semaphore is resized.

| Setting | Takes effect |
|---|---|
| `MaxActiveFiles`, `MaxPartsPerFile`, `MaxActiveParts` | Next dispatch. Raising starts work immediately. Lowering never cancels an attempt, and the count falls as attempts finish |
| `UploadBytesPerSec`, `DownloadBytesPerSec` | Immediately through `BandwidthLimiter.SetRate` |
| `PartSize` | Jobs not yet planned |

### Shutdown order for `Engine.Close`

1. Reject new commands with `ErrEngineClosed`.
2. Apply Cancel to every non-terminal job, with cause `ErrEngineClosed`. Unlike a user cancel, this also cancels a Complete in flight. Its journal entry stays for next launch.
3. Wait for all attempts to exit.
4. Run aborts in parallel within `Timeouts.ShutdownAbort`. Entries not aborted stay in the journal for next launch.
5. Close all source handles and abort all sink writers.
6. Stop the watchdog, deliver the final progress batch, stop the progress goroutine and exit the scheduler.
7. The caller closes the journal after `Close` returns.

Each test that starts an engine must end by calling `Close` and asserting that the goroutine count returned to its baseline.

## 4. Memory bound

`MemoryBound` in `internal/engine/settings.go` is the formula:

```
peak live heap <= MemBase (16 MiB)
                + (MaxActiveParts + MaxActiveFiles) * MemPerConnection (384 KiB)
                + MaxActiveParts * DownloadBufferSize (1 MiB)
                + MaxActiveFiles * MaxPartCount (10,000) * MemPerPartEntry (96 B)
                + trackedJobs * MemPerJobRecord (2 KiB)
```

At default settings with 1,000 tracked jobs the bound is about 67 MiB. Part size and object size do not appear in it.

To keep it true:

- Upload bodies stream from `io.SectionReader` and are never buffered.
- Download buffers come from a `sync.Pool` of `DownloadBufferSize` slices.
- The part table is allocated at admission and released at a terminal state. Queued jobs hold no part table.

**How tests assert it** (stage 0.4, in a file tagged `//go:build !race`, because the race detector multiplies memory use):

1. Force a GC and record the baseline from the `runtime/metrics` sample `/memory/classes/heap/objects:bytes`.
2. Sample every 100 ms during a simulated transfer.
3. Assert `peak - baseline <= MemoryAssertGCFactor (2) * MemoryBound(settings, jobs)`.
4. Run with part sizes of 64 MiB and 1 GiB, and assert the peaks differ by less than 16 MiB.

If a measured term is larger than budgeted, update the constant and this section together.

## 5. Part sizing

`PlanParts` in `internal/engine/partplan.go` is the rule for uploads and downloads:

1. Start from the preferred size, or `DefaultPartSize` (64 MiB) when it is 0.
2. Clamp to 5 MiB through 5 GiB.
3. Raise to `ceil(size / 10,000)` if that is larger.
4. Round up to a 1 MiB multiple, capped at 5 GiB.

| S3 limit | Constant | Value |
|---|---|---|
| Minimum part, except the last | `MinPartSize` | 5 MiB |
| Maximum part | `MaxPartSize` | 5 GiB |
| Maximum parts | `MaxPartCount` | 10,000 |
| Maximum object (AWS documents 48.8 TiB) | `MaxObjectSize` | 5 GiB x 10,000 |
| Maximum single PUT | `MaxSinglePutSize` | 5 GiB |

**Single PUT:** files of `SinglePutThreshold` (64 MiB) or less use `PutObject`, including zero-byte files. Larger files use multipart upload. The threshold does not depend on the user's part size.

A 2 TiB object with default settings uses 210 MiB parts and 9,987 parts.

## 6. Error taxonomy and retry

`ClassifyResponse`, `Classify` and `ClassifyAttempt` in `internal/engine/errors.go` are the mapping.

| Class | Engine reaction | Sources |
|---|---|---|
| `ClassRetryable` | Part to backoff, attempt charged | HTTP 408, 429, 5xx except 501 and 505. Codes `SlowDown`, `InternalError`, `ServiceUnavailable`, `RequestTimeout`, `RequestTimeTooSkewed`, `IncompleteBody`, `BadDigest`, `OperationAborted`, `ConditionalRequestConflict`, other throttle codes. All transport errors. `ErrStalled` |
| `ClassCredential` | Job to `credential-wait`, no attempt charged | `ExpiredToken`, `TokenRefreshRequired`, `InvalidToken`, `InvalidAccessKeyId`, `InvalidSecurity`, `SignatureDoesNotMatch`, `RequestExpired` and similar. `ErrCredentialsUnavailable` |
| `ClassPermanent` | Job to `failing` | All other 3xx and 4xx, including `AccessDenied`, `NoSuchKey`, `NoSuchBucket`, `InvalidPart`, `InvalidRange`, and 412. Engine sentinels such as `ErrObjectChanged` and `ErrLocalIO` |
| `ClassCancelled` | Part interrupted, nothing reported | `ErrJobPaused`, `ErrJobCancelled`, `ErrEngineClosed`, `ErrPartPreempted`, `context.Canceled` |

The error code decides first, then the status. `AccessDenied` stays permanent because it describes policy, which waiting will not fix.

**Adapter mapping in `internal/s3x`:** extract the code with `errors.As(err, &smithy.APIError)`, the status with `errors.As(err, &awshttp.ResponseError)` and the request ID from the same error. Return `*engine.OpError`. Map 412 on a GET to `ErrObjectChanged`. Requests that never reach the network because the input is invalid become `ClassPermanent`.

**Attempts** must call `ClassifyAttempt(ctx, err)`. The context's cause outranks the returned error, because the SDK reports every cancellation generically.

**Local I/O:** the upload body wrapper stores any `ReadAt` error in the attempt before returning it to the SDK. The attempt checks that stored error first and reports `ErrLocalIO`, otherwise a disk failure would look like a network failure.

### Backoff and limits

`RetryPolicy` in `internal/engine/retry.go`, using full jitter:

```
ceiling = min(30 s, base * 2^(attempt-1))     base = 500 ms, or 2 s when throttled
delay   = max(ceiling * jitter, min(Retry-After, 30 s))
```

| Operation | Attempt limit |
|---|---|
| Part upload or download | 10 |
| Create, Complete, Head | 8 |
| Abort | 5, then left to the journal |

A retryable failure fails the job only when **both** conditions hold: attempts reached the limit, and the job has finished no part for `OutageTolerance` (30 minutes). Below that, the part keeps retrying at the backoff ceiling. This stops a network outage from destroying a long transfer. The progress clock resets when the job enters `running`, so paused and credential-wait time does not count.

## 7. Upload path

1. **Prepare:** `Source.Open`, record `SourceStat`, check `WriteGuard`, then call `PlanParts`.
2. **Journal intent:** `Store.Put` with `PhaseCreating`. A failure here fails the job with `ErrJournal`, and no upload is created.
3. **Create:** `CreateMultipartUpload` with `ChecksumAlgorithm=CRC32` and `ChecksumType=COMPOSITE`.
4. **Journal active:** `Store.Put` with `PhaseActive` and the upload ID. On failure, abort the upload and fail with `ErrJournal`.
5. **Parts:** one attempt per part, each building a fresh body:

```go
section := io.NewSectionReader(handle, part.Offset, part.Length)
body := newMeteredBody(section, attempt) // Exposes only Read and Seek.
store.UploadPart(ctx, engine.UploadPartRequest{Body: body, ContentLength: part.Length, ...})
```

6. **Verify source:** `SourceHandle.Stat` must match the recorded size and modification time, otherwise fail with `ErrSourceChanged`.
7. **Complete:** send parts in ascending order, each with ETag and `ChecksumCRC32`, plus `MpuObjectSize`.
8. **Verify checksum:** compare the response with `CompositeCRC32` of the part values. A mismatch fails with `ErrChecksumMismatch`, and the warning must say the object exists on S3.
9. **Journal delete:** `Store.Delete`. A failure is logged only, because next launch's abort will get `NoSuchUpload`.

**A part response with no `ChecksumCRC32` is a retryable failure.** The value is required for Complete.

**Lost Complete response:** if a retry of Complete returns `NoSuchUpload`, an earlier attempt may have succeeded. Call `HeadObject`, which the adapter must always send with `ChecksumMode=ENABLED`. If the checksum equals the local composite value, the job succeeded. Otherwise fail.

**Single PUT:** `PutObject` with the same body wrapper and CRC32, with no journal entry.

### Verified SDK behaviour

`internal/s3x/sdk_contract_test.go` pins each of these against a local TLS server.

| Topic | Behaviour | Required action |
|---|---|---|
| Disabling retries | Per-call `o.Retryer = aws.NopRetryer{}` makes one attempt, even on a client built with `RetryMaxAttempts: 5` | Pass `s3x.SingleAttemptOptions` on every call. Do not set `o.RetryMaxAttempts` per call, because that re-wraps the retryer |
| HTTPS upload | Trailing checksum: `aws-chunked`, `STREAMING-UNSIGNED-PAYLOAD-TRAILER`, `x-amz-trailer: x-amz-checksum-crc32`. The source is read exactly once, as a single chunk with no buffering | Set `ChecksumAlgorithm=CRC32` and `ContentLength` |
| Plain HTTP upload | The SDK reads the whole body to hash it, rewinds, then sends it. The source is read **twice** | `internal/fakes3` must serve TLS. The body's `Seek` to start must roll back metered bytes |
| Ranged GET checksum | Arbitrary ranges carry no checksum | `SingleAttemptOptions` sets `ResponseChecksumValidation=WhenRequired`, so no `x-amz-checksum-mode` header is sent |
| Credential failure | Reaches the caller wrapped as `get identity: get credentials: failed to refresh cached credentials`, with the chain intact and no request sent | Build `aws.NewCredentialsCache(s3x.MarkCredentialErrors(base))` |
| `Expect: 100-continue` | Added for bodies over 2 MiB | Keep the transport's 1 s `ExpectContinueTimeout`. A zero value sends the body without waiting |
| Error inside a 200 | Complete and UploadPart surface these as API errors | Classified by code, already covered |

**Transport settings for stage 0.4:** the SDK default `MaxIdleConnsPerHost` is 10, which would force repeated TLS handshakes. Set `MaxConnsPerHost` to 0, and both `MaxIdleConns` and `MaxIdleConnsPerHost` to `MaxActivePartsLimit`. Set read and write buffers to 64 KiB.

## 8. Download path

1. **Plan:** `HeadObject` records size and ETag. Then call `PlanParts`.
2. **Journal:** when size is at least `JournalTempMinSize`, `Store.Put` a `KindDownloadTemp` entry using `fsx.TempPath`. This happens **before** the temp file exists.
3. **Prepare:** `Sink.Prepare` checks free space and collisions, then creates the temp file. On failure, delete the journal entry.
4. **Parts:** `GetObjectRange` with `Range: bytes=a-b` and `If-Match: <etag>`. Read into a pooled buffer, then `WriteAt`.
5. **Commit:** `SinkWriter.Commit` syncs, closes and renames without replacing.
6. **Journal delete:** `Store.Delete`.

**Response checks in the adapter:** status 206, `Content-Range` start and end equal to the request, total equal to the recorded size and ETag equal to the recorded ETag. A total or ETag difference or a 412 returns `ErrObjectChanged`, which is permanent. A short body is `io.ErrUnexpectedEOF`, which is retryable.

**Mid-part retry:** the part records bytes written. A retry requests only the remaining range, so finished bytes are not fetched again.

**Zero-byte objects** send no GET. Prepare and commit an empty file.

**Temp file:** `fsx.TempPath(final, jobID)` gives `<final dir>/~s4-<jobID>.s4tmp`. The same directory keeps the rename atomic, and the fixed length cannot exceed filename limits.

**Finalize:** `fsx.Finalizer.RenameNoReplace`. On Windows use `MoveFileExW` with `MOVEFILE_WRITE_THROUGH` and without `MOVEFILE_REPLACE_EXISTING`. A collision returns `ErrDestinationExists`. Go's `os.Rename` must not be used, because it replaces the target.

### Write strategies

`fsx.WriteStrategy` is the plug-in point for stage 0.5, with `Create(path, size)` and `MaxLead()`. Candidates are named by the `Strategy*` constants.

**Lead gate:** `frontier` is the offset of the lowest unfinished byte in the job. When `MaxLead() > 0`:

- The scheduler does not dispatch a part whose offset is beyond `frontier + MaxLead`.
- An attempt that would write beyond `frontier + MaxLead` parks until the frontier advances. It calls `AttemptActivity.Park` so the watchdog ignores the wait.

**Preemption prevents deadlock:** parked attempts could fill every slot while the lowest part waits to be dispatched. If the job's lowest unfinished part is pending and cannot be dispatched, the scheduler cancels the job's highest-offset parked attempt with `ErrPartPreempted`. Its written bytes are kept.

## 9. Data path hooks

At 5 Gbps the data path makes roughly 20,000 reads per second at 32 KiB each. Nothing below runs per byte.

| Hook | Mechanism | Cost per Read |
|---|---|---|
| Stall watchdog | `AttemptActivity.Touch`, one uncontended atomic add (3.7 ns measured). No clock read | 1 atomic |
| Progress | Local counter, flushed to the job's atomic every `MeterQuantum` (256 KiB) | 1 addition |
| Bandwidth limit | `BandwidthLimiter.WaitN` at the same `MeterQuantum` flush. Skipped when unlimited | none between flushes |

**Metered body** implements only `Read` and `Seek`. It must not expose `WriterTo` or `ReaderFrom`, so reads cannot bypass metering. `Seek` to the start subtracts the bytes counted so far.

### Stall watchdog

One goroutine scans registered attempts every `WatchdogTick` (1 s). For each attempt it calls `Observe`:

- If parked, or if ticks changed, reset that attempt's idle time.
- Otherwise add the tick. When idle time reaches the phase's limit, cancel the attempt with `ErrStalled`.

| Phase | Set when | Limit |
|---|---|---|
| `AttemptAwaitResponse` | Attempt starts, or the upload body reached EOF | `Timeouts.Response` (60 s) |
| `AttemptSend` | First body read | `Timeouts.Stall` (30 s) |
| `AttemptReceive` | Response headers arrived | `Timeouts.Stall` (30 s) |

Control requests use `context.WithTimeout` with `Timeouts.Control` or `Timeouts.Complete` instead.

### Bandwidth limiter

Implement with `golang.org/x/time/rate`, one limiter per direction shared by all jobs. Use `ReserveN(clock.Now(), n)` and wait on a `Clock` timer, so tests control time. The burst is `max(MeterQuantum, rate/10)`. Split any `n` larger than the burst. Park the attempt during the wait.

### Batched progress

The progress goroutine ticks every `ProgressInterval` (200 ms), reads job counters and sends one `ProgressBatch` to `Observer.OnProgress`. Only changed jobs are included. A failed attempt's bytes are subtracted, so `DoneBytes` can fall. Transitions are buffered up to `MaxBufferedTransitions`. On overflow the oldest are dropped and `Resync` is set, so the UI calls `Snapshot`.

## 10. Credential recovery

1. A request returns `ClassCredential`. The part is interrupted with no attempt charged.
2. Every running job on that profile receives `JobEvCredentialError`.
3. In-flight attempts continue. Those that succeed are recorded as done, and those that fail with credential errors return to pending.
4. Part tables, upload IDs, journal entries and open handles stay as they are. Nothing is aborted.
5. The scheduler runs `CredentialProber.Probe` starting at `ProbeMin` (5 s), doubling to `ProbeMax` (60 s). `NotifyCredentialsChanged` runs a probe at once.
6. When a probe passes, every waiting job on that profile receives `JobEvCredentialsRestored` and dispatch resumes at the first pending part.

The wait has no time limit. The user can pause or cancel at any time. If a probe passes but requests still fail, the job re-enters the wait, and the probe interval keeps growing until a request succeeds.

## 11. Journal

**Files**, in the app's config directory:

| File | Purpose |
|---|---|
| `journal.json` | Committed state |
| `journal.json.tmp` | Next snapshot, never read |
| `journal.lock` | Held exclusively for the process lifetime |
| `journal.corrupt-<timestamp>.json` | A journal that failed to parse |

**Format:** `{"version":1,"entries":[...]}`, written by `journal.Encode` and read by `journal.Decode`. Entries are small and few, because only active work is recorded.

**Atomic write rule:** write the whole snapshot to `journal.json.tmp`, `fsync` it, close it, rename it over `journal.json` with write-through, then `fsync` the directory on Unix. `Put` and `Delete` return only after this finishes. Concurrent calls may share one snapshot write.

**No secrets:** `Entry` is an allowlist containing a profile *name*, region, bucket, key, upload ID, temp path and timestamps. A test fails if a field is added, and `Decode` rejects unknown fields.

**Single instance:** `Store` holds `journal.lock` and returns `ErrLocked` if another process has it. Without the lock, a second instance would abort the first instance's live uploads at launch.

### Crash windows

| Crash happens | State found at launch | Cleanup |
|---|---|---|
| Before the intent is durable | No entry, no upload | None |
| After intent, before S3 received Create | `creating` entry, no upload | List finds nothing, delete entry |
| After Create, before the upload ID is durable | `creating` entry, unknown upload | `ListUploads`, abort each `MatchesIntent` match, delete entry |
| While parts upload | `active` entry | Abort by ID, delete entry |
| After Complete, before entry delete | `active` entry, object exists | Abort returns `ErrUploadGone`, delete entry |
| During journal write | Old `journal.json` plus a stray `.tmp` | Delete `.tmp`, use the old file |
| During download | `download-temp` entry | Delete the temp file if `fsx.IsTempPath` accepts it, delete entry |

`MatchesIntent` requires an exact key and an initiation time within `[CreatedAt-5 min, CreatedAt+1 h]`, so other clients' uploads are left alone.

### Launch cleanup (`journal.Recoverer`, stage 2.1)

1. Open the store and take the lock. Delete any `journal.json.tmp`.
2. If `Decode` returns `ErrCorrupt`, move the file to the `journal.corrupt-` name, start empty and warn the user.
3. If it returns `ErrUnsupportedVersion`, leave the file alone and disable uploads until the user resolves it.
4. Run cleanup in the background with at most 4 parallel requests. It does not block the UI or new jobs.
5. On `ErrCredentials`, keep the entry and count it as deferred. Retry when `NotifyCredentialsChanged` fires for that profile and at each launch.

## 12. Test seams

| Seam | Production | Simulation |
|---|---|---|
| `engine.ObjectStore` | `internal/s3x` | `internal/fakes3`, in process or over HTTPS |
| `engine.Source` | File handles | Synthetic bytes computed from the offset |
| `engine.Sink` | Temp file through `fsx.WriteStrategy` | Verifies the pattern, stores nothing |
| `engine.Clock` | Real time | Fake clock |
| `Config.Jitter` | `math/rand/v2` | Fixed value |
| `engine.CredentialProber` | `internal/s3x` | Scripted results |
| `journal.Store` | File store | In-memory store, or a file store in `t.TempDir()` |

**Simulated 2 TB run:** use the in-process fake with the synthetic source and sink, so no data touches disk or the network stack.

**Synthetic pattern:** the 8-byte little-endian word at offset `8*i` is `SplitMix64(seed + i)`. Any byte can be computed from its offset alone, so a part can be generated or verified independently.

**The fake must store only** each part's length, CRC32 and ETag, never its data.

**Test-prefix guard:** `engine.TestPrefixGuard` refuses any key outside `s4browser-test/<run-id>/`, along with `.`, `..`, empty segments, backslashes and NUL. All test tooling must set `Config.Guard` and wrap its `ObjectStore` in a decorator that calls `CheckWrite` before Create, UploadPart, Complete, Abort and Put, so a bug in the engine cannot bypass it. `journal.RemoteCleaner` in test tooling must be wrapped the same way.

## 13. Stage map

| Stage | Implements |
|---|---|
| **0.3** | `internal/fakes3`: `engine.ObjectStore` in process and an HTTPS server, synthetic pattern, scriptable faults (status codes, resets, stalls, expired credentials, lost Complete responses), the guard decorator, `engine.Clock` fake, synthetic `Source` and `Sink` |
| **0.4** | `engine.New` and `engine.Engine`: scheduler, attempts, metered body, watchdog, limiter, progress, upload path. `internal/s3x` `ObjectStore` and transport. File `Source`. `journal.Store` file implementation with the lock. Memory and goroutine-leak tests |
| **0.5** | Download path in the engine. `fsx.WriteStrategy` implementations and `fsx.Finalizer` with `_windows.go` and portable variants. File `Sink`. Strategy comparison on a 300 GB file |
| **0.6** | `cmd/s4engine` against `engine.Engine`, using the guard for every test run. Minimal Wails file-drop window |
| **2.1** | `journal.Recoverer` and `journal.RemoteCleaner`. `engine.CredentialProber` in `internal/s3x`. Pause, resume and credential recovery hardened through the fault suite, including kill and relaunch |
