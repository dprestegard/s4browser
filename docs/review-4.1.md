# Stage 4.1 review: engine, journal and path safety

Reviewed on 2026-11-20 at commit `fdb02b8`. In scope: `internal/engine` (scheduler, upload and download paths, metered body, watchdog, limiter, retry, credential wait, file source and sink), `internal/journal` (file store, lock, launch cleanup), `internal/fsx` (path safety, write strategies, no-replace rename) and the parts of `internal/queue` and `internal/s3x` that feed them (folder expansion, download placement, `GetObjectRange`, `HeadObject`). The code was checked against `docs/engine-design.md`.

Stage 4.2 fixes these. No code was changed in this stage.

**Verdicts:** *confirmed* means reproduced by a test or a run. *Code* means the path was traced in the source and the outcome is certain, but no test was run. *Design* means the code does what the design says, and the design is what should be reconsidered.

## Ranking

| ID | Severity | Area | Summary | Verdict |
|---|---|---|---|---|
| F1 | **High** | Download sink | Free space is checked per job, ignoring other downloads to the same drive. Sparse temp files reserve nothing, so parallel large downloads can all pass and then all fail with the drive full, and each loses everything it downloaded | Code |
| F2 | **Medium** | Journal | A journal written by a newer build is moved aside as corrupt instead of being refused as a newer version, so its unfinished uploads are forgotten and keep costing money | Confirmed |
| F3 | **Medium** | Download sink | Retrying a failed download whose temp file could not be removed always fails, because the retry uses the same temp name and creating it requires that it not exist | Code |
| F4 | Low | Upload finalize | A cancel requested while the source is being verified does not stop Complete from being sent. While Complete is retried, the cancel stays pending for up to 30 minutes | Code |
| F5 | Low | Download progress | The retry budget counts mid-part download attempts that made progress, because only a finished part counts as progress | Code |
| F6 | Low | Download placement | Two separate download requests for the same destination both pass the checks. The second downloads the whole object and then fails at the rename | Code |
| F7 | Low | Scheduler | Jobs waiting for credentials keep their slots, so one profile's expired session stops queued jobs of every other profile | Design |
| F8 | Info | Shutdown | A Complete or a download commit that succeeds while the app is closing ends the job as "cancelled", although the object or file exists | Code |
| F9 | Info | Path safety | A key name that is a Windows 8.3 short name (such as `PROGRA~1`) can resolve to an existing folder with another name inside the destination | Code |
| F10 | Info | Path safety | Links on the destination path are checked when the download is queued, not when its temp file is created, which can be much later | Code |
| F11 | Info | Download integrity | A download is not verified against the checksum S3 stores. Integrity rests on TLS, `If-Match` and the range and size checks | Design |

**Path traversal:** no way was found for an S3 key to write outside the chosen folder. `fsx.SplitKey` and `fsx.CheckName` refuse absolute, UNC and drive forms, backslashes, `.` and `..`, `:` (which also covers alternate data streams), trailing dots and spaces, device names, control characters and invalid text. `Placer.Place` re-checks the joined path with `filepath.Rel` and refuses links and junctions on the way. `MakeParents` creates folders one level at a time and checks each with `Lstat`. Every commit uses a rename that never replaces. F9 and F10 are refinements, not escapes.

**Concurrency:** no data race or deadlock was found. Every goroutine is registered before it starts, and the loop drains results until the count reaches zero. The progress observer (`queue.observer`) never calls back into the engine, and no queue method calls the engine while it holds its own lock. Without either property, `finishClose` could deadlock. Both should stay true (see "Keep" below).

## Findings

### F1 (High): parallel downloads can fill the drive and lose all progress

**Where:** `internal/engine/filesink.go:58-65` (`FileSink.Prepare`), `internal/fsx/strategy_windows.go` (sparse default).

**What happens:** `Prepare` requires `size + 64 MiB` of free space at the moment that one job is admitted. It does not count bytes that other downloads to the same volume still have to write. The default strategy is `sparse`, which reserves no clusters. With 3 downloads of 2 TiB to a drive with 5 TiB free, all three pass, because each sees 5 TiB. About 5 TiB later, `WriteAt` fails with ERROR_DISK_FULL. That is `ErrLocalIO`, which is permanent, so every job writing to the drive fails. Cleanup deletes each temp file, and **Retry starts from zero**. For a tool built to move very large media files, that is hours of transfer lost on a condition the app could have predicted.

The same happens when another program fills the drive during a long download. That cannot be prevented, but it should not throw the work away.

**Fix:**
1. Keep an in-process reservation per volume in `FileSink`: the remaining bytes (size minus written) of every prepared, uncommitted temp file on that volume. Check `size + reserve + reserved` against free space in `Prepare`, and release the reservation at Commit or Abort. Identify the volume with `GetVolumePathNameW` on Windows, and with the parent directory elsewhere.
2. Make a full disk recoverable. A write error that is a full disk should pause the job, or fail it while keeping the temp file and part table, with a message saying to free space and resume. It should not abort and delete. This needs a new error class or sentinel, which is an engine contract change, so per the plan it needs Opus.

**Test:** a `FileSink` whose `FreeSpace` returns a fixed figure. Two jobs that each fit alone must not both be accepted. A sink whose `WriteAt` returns a disk-full error must leave the job resumable, with the temp file kept.

### F2 (Medium): a newer journal is treated as corrupt

**Where:** `internal/journal/journal.go:158-183` (`Decode`), `internal/journal/filestore.go:82-97`.

**What happens:** `Decode` uses `DisallowUnknownFields` and validates every entry before it looks at `version`. A future format that adds a field or an entry kind fails decoding with `ErrCorrupt`. `OpenFile` then renames the file to `journal.corrupt-<time>.json` and starts empty, and the queue keeps uploads on. The newer build's unfinished multipart uploads are no longer tracked: nothing aborts them, and S3 keeps charging for their parts. The design (section 11, launch cleanup step 3) says a newer version must leave the file alone and turn uploads off.

**Reproduced:** a `journal.json` with `"version":2` and one extra field in an entry. `OpenFile` returned no error, logged the "was corrupt and was moved" warning and renamed the file.

**Fix:** decode only `{"version": int}` first, without strict fields. If the version is not `FormatVersion`, return `ErrUnsupportedVersion` before any strict decoding. Only then decode strictly.

**Test:** the case above must return `ErrUnsupportedVersion` and leave `journal.json` in place. A version-1 file with an unknown field is still corrupt.

### F3 (Medium): retrying a download after a failed temp-file removal always fails

**Where:** `internal/fsx/fsx.go:92` (`TempPath(final, jobID)`), `internal/engine/download.go:549-571` (`startSinkAbort`), `internal/engine/scheduler.go:292` (`Retry` keeps the job ID).

**What happens:** the temp name depends only on the job ID, and a retried job keeps its ID. If cleanup could not remove the temp file (for example, antivirus or an indexer holds it), the job fails with "will be removed at next launch" and the file stays. Retry then calls `Create`, which opens with `O_EXCL`, so it fails with "file exists". The job can never be retried in this session.

**Fix:** have `Prepare` remove a leftover temp file of the same name before creating it. It is this job's own file, so this is safe, and `IsTempPath` confirms the form. Alternatively, put the run number into the name. That would need `IsTempPath`'s pattern and the journal entry to agree.

**Test:** a sink whose first `Abort` fails, then Retry. The second run must succeed.

### F4 (Low): a cancel during finalizing is ignored too long

**Where:** `internal/engine/upload.go:486-509` (`startVerifySource`), `511-548` (`startComplete`), `237-259` (`controlFailed`).

**What happens:** during `PhaseFinalizing` a user cancel only sets `cancelRequested`. Verifying the source is a local `Stat`, but when it passes, `advance` goes straight to `startComplete` without looking at the flag. So a cancel made before Complete was sent still creates the object. If Complete then fails retryably, `controlFailed` schedules another try, and the flag is only acted on in credential wait or on a fatal error. The cancel can wait for the whole `OutageTolerance` (30 minutes) while the UI shows the job still finalizing.

**Fix:** in `advance`, check `cancelRequested` before `startComplete`, which covers the first send and each retry. When it is set and no Complete is in flight, apply the cancel. The design's rule "deferred until the outcome is known" only needs a Complete in flight to be waited for.

**Test:** cancel while `VerifySource` is in flight: no Complete is sent and the upload is aborted. Cancel while Complete waits to be retried: the job is cancelled without another Complete.

### F5 (Low): mid-part download progress does not count as progress

**Where:** `internal/engine/upload.go:192` (`Exhausted` with `now.Sub(j.lastProgress)`), `internal/engine/download.go:377-405`.

**What happens:** `lastProgress` moves only when a part finishes. A download attempt that writes most of a large part and is then reset is charged an attempt, and the next one resumes after the written bytes. On a link that drops connections often, with large parts (210 MiB for 2 TiB, up to 5 GiB for the largest objects), 10 such attempts can pass with no part finished for 30 minutes. The job then fails while bytes were flowing the whole time.

**Fix:** in `onDownloadPartDone`, when `written > 0`, set `lastProgress = now` and do not charge the attempt, or reset the part's `attempts`. Uploads cannot resume mid-part, so they are unchanged.

**Test:** a fake that resets every GET after a fixed number of bytes. The download must finish however many resets that takes.

### F6 (Low): two requests for the same destination

**Where:** `internal/queue/submit.go:351-381`. One `fsx.Placer` serves each request.

**What happens:** collision checks are per request, and `Lstat(final)` sees nothing until a job commits. Downloading `a.mov` into the same folder twice while the first is still queued queues both. The second transfers the whole object and then fails at the rename with "already exists". Nothing is overwritten, but the time and bandwidth are wasted, and the refusal comes late.

**Fix:** the queue keeps a set of the destinations of unfinished download jobs, compared ignoring case, and refuses a new one that is already there with code `exists`.

**Test:** two `StartDownloads` calls for the same key and folder. The second is refused at once.

### F7 (Low, design): credential wait blocks other profiles

**Where:** `docs/engine-design.md` section 2 (credential-wait holds a slot), `internal/engine/credentials.go`.

**What happens:** jobs waiting for credentials keep their file slots, and the wait has no time limit. If profile A's session expires with 4 jobs running, which is the default number of files at once, queued jobs of profile B never start until A signs in again or the user pauses A's jobs.

**Fix:** release the slot in credential wait and re-admit FIFO when the probe passes, or let other profiles' jobs exceed the limit while every slot is waiting. Either one changes the state table, so it needs Opus.

### F8 (Info): "cancelled" although the work finished during shutdown

**Where:** `internal/engine/scheduler.go:611-626` (`beginClose`), `internal/engine/upload.go:555-564`, `internal/engine/download.go:516-543`.

When the app closes while Complete or a download commit is in flight, the job is cancelled at once. If the request then succeeds, cleanup removes the journal entry correctly, but the job ends as `cancelled` although the object or file exists. The app is closing, so this matters only if it is ever logged or shown at next launch. One possible fix is a warning on the job: "finished as the app closed".

### F9 (Info): 8.3 short names

**Where:** `internal/fsx/pathsafe.go:213-297`.

On volumes with 8.3 names turned on (common on C:), `Lstat("dest\PROGRA~1")` finds an existing `Program Files` inside the destination. A key `PROGRA~1/x.bin` is then placed inside that folder under its long name. The path stays inside the chosen folder, and the no-replace rename stops any overwrite. Optionally, refuse a name of the form `<up to 6 chars>~<digit>[.<up to 3 chars>]` when `Lstat` finds it, or compare the name `FindFirstFile` reports.

### F10 (Info): link check and temp-file creation are apart in time

**Where:** `internal/fsx/pathsafe.go` (checks at submit), `internal/engine/filesink.go:45-56` (`os.Stat(dir)` follows links at admission).

A queued job can wait hours before `Prepare`. A folder replaced by a junction in the meantime would be written through. That needs another program acting on the user's own folders, so it is outside the threat model. If wanted, `Prepare` can walk up from the file with `Lstat` to the request's root. That means passing the root in `SinkSpec`.

### F11 (Info, design): downloads are not checked against S3's checksum

Each range is fetched under `If-Match` with the HEAD's ETag, and its `Content-Range` and total are checked. There is no comparison with the object's stored CRC32 or composite checksum. Corruption between the network buffer and the disk would go unnoticed. The plan lists checksum verification as out of scope for v1, which is reasonable, but it should stay a known limit in the README (stage 4.4).

## Checked and sound

- **State machine:** commands and events go through `NextJobState` and `NextPartState`. Rejected async events are ignored, and an illegal part edge panics instead of corrupting a transfer. A stale queue entry (pause or resume while queued) is skipped because admission checks for `JobQueued`.
- **Shutdown order:** busy work drains before any abort, aborts run under their own deadline, and a Complete in flight keeps its journal entry. The loop exits only when `open` and `outstanding` are both zero.
- **Create in doubt:** a lost Create gets its own `creating` entry, and launch cleanup aborts only uploads that `MatchesIntent` accepts and no entry names. It defers when a session entry names the same key. A cancel during Create waits for the ID and aborts it. If that abort fails, the `creating` entry stays and is resolved by intent matching.
- **Lost Complete:** a `NoSuchUpload` on a retried Complete leads to a HEAD with checksum mode. Success requires the composite checksum to match.
- **Download consistency:** `If-Match`, 206 with an exact `Content-Range`, a total equal to the HEAD's size, and a check that every byte was written before commit. A part write that comes up short is `ErrLocalIO`.
- **Metered body:** a seek to the start rolls back published bytes. A body retired after its attempt cannot change progress, and local read errors are reported as local.
- **Journal writes:** a temp file with fsync, then `MoveFileEx` with write-through, retried briefly for antivirus. Memory is updated only after the file is durable, and the process lock is held for the process's lifetime. Entries are an allowlist with no secrets. Launch cleanup deletes only files whose name matches the temp pattern.

## Keep (rules for later changes)

1. `Observer.OnProgress` and `OnTransition` must never call the engine. `finishClose` waits for the progress goroutine before closing `stopped`, so a call into the engine from there would deadlock at shutdown.
2. Queue code must not call the engine while it holds `Service.mu`. The observer takes that lock on the progress goroutine.
