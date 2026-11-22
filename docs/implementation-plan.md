# s4browser: implementation plan

This is the plan v1 was built from, kept as the record of scope and decisions. v1 is complete. What each stage did is in [progress.md](progress.md), current user-facing limits are in the [README](../README.md#known-limits), and work after v1 is in [roadmap.md](roadmap.md).

## Goal

A reduced-scope, personal replacement for the S3 Browser Windows app, focused on moving very large media files quickly. The app has its own name and visual design.

## v1 scope

**In scope**
- Sign-in with AWS CLI profiles, or static access keys stored in Windows Credential Manager
- Browse buckets and prefixes, with up to 100,000 objects under one prefix
- Upload files and folders, including drag-in from Explorer
- Download files and folders
- Delete objects and create folders
- Transfer queue with controls for parallel files, parallel parts, part size and bandwidth cap
- Automatic retry and in-session resume while the app stays open
- Presigned URLs
- View object metadata, and view/edit tags

**Out of scope for v1**
- Resume after the app closes, end-to-end checksum verification UI, folder sync/compare
- S3-to-S3 copy/move, rename (S3 implements rename as copy + delete), storage class changes, Glacier restore, versioning UI
- Metadata editing, drag-out, dual-pane view, tabs, recursive search, folder size calculation, preview
- ACLs, CloudFront, browser-based SSO sign-in, S3-compatible endpoints

**Assumptions**
- One active profile at a time, with a profile switcher
- Buckets can be added by name when a role cannot list all buckets
- Each bucket's region is detected automatically
- Uploads inherit the bucket's default encryption

## Decisions

| Topic | Decision | Effect |
|---|---|---|
| Platform | Windows first, one portable codebase | Mac deferred until after v1. Code stays portable and engine packages are compile-checked for macOS. Drag-out, signing and Mac parity are out of scope |
| Media location | Local NVMe (NTFS) | HDD, SMB and exFAT are not tuned |
| Uplink | 2 to 5 Gbps | Pass mark is 80% of measured line rate |
| Real AWS testing | Minimal, about 10 GB | Scale is proven against a local fake S3 |
| Credentials | Static keys and `credential_process` | Only these are tested. Session keys in the shared files were added as a third type in stage 1.1. Other profile types are listed but unsupported |
| Test bucket | One real bucket, dedicated prefix | Test tooling refuses writes outside `s4browser-test/<run-id>/` |
| Execution | New session per stage | Each stage has a self-contained brief and a model assignment |

## Stack

| Layer | Choice | Reason |
|---|---|---|
| Desktop shell | Wails v2.17.1 | Go backend with a system webview. Its file-drop API gives the backend absolute paths, so file data never passes through the UI. Wails v3 shipped stable on 2026-10-26 but was too new, and v2 covers everything the app needs |
| Backend | Go + AWS SDK for Go v2, low-level S3 client | Good fit for highly parallel I/O, and portable |
| Transfer engine | Custom multipart engine | Direct control of part size, concurrency, bandwidth, retries and progress. `feature/s3/manager` is deprecated and `feature/s3/transfermanager` is still v0.x |
| UI | React + TypeScript + Vite + `@tanstack/react-virtual` | Handles 100,000 rows without rendering them all |

Exact tool and library versions are recorded in progress.md, stage 0.1 and 0.2.

## Architecture rules

These held through v1. The engine's own contracts are in [engine-design.md](engine-design.md).

- **File data never crosses the UI bridge.** Drops deliver absolute paths to Go.
- **Go owns the listing.** The UI requests visible row windows, and Go sorts. This keeps 100,000 rows out of webview memory.
- **Progress is batched** and sent to the UI about 5 times per second.
- **Uploads** read each part with `io.SectionReader` over a shared file handle, so memory does not scale with part size. Parts carry CRC32 checksums.
- **Part size** is the larger of the user's setting and `object size / 10,000`, rounded up to 1 MiB, within S3's 5 MiB to 5 GiB limits.
- **Downloads** use parallel ranged GETs with `If-Match` on the ETag into a temp file that is renamed on completion, so an object replaced mid-transfer fails clearly.
- **Stalled connections** are cancelled by an idle-byte watchdog and retried.
- **Engine retries own the policy.** SDK retries are disabled on part requests so attempts are not multiplied.
- **Limiter and concurrency settings** can be changed while transfers run.
- **Credentials refresh.** A long transfer outlasts a role session, so an expired credential pauses the job, keeps completed parts, and continues when credentials return.
- **Orphaned uploads are aborted.** An on-disk journal of active upload IDs lets the next launch abort leftovers after a crash.
- **Downloads are path-safe.** Keys with `..`, absolute paths, or names Windows cannot hold are reported and never written, and an existing local file is never overwritten.
- **`credential_process` helpers run hidden**, so a console window never flashes.
- **Presign expiry** is the shortest of the requested time, 7 days and the credential's own expiry.
- **Free space is checked** before a download starts.

## Stages

Each stage ran in a new session. It read this plan, `engine-design.md` and the last entry in `progress.md`, finished its done criteria, committed, and appended a handoff note.

| Stage | Work | Model | Done when |
|---|---|---|---|
| **M0: prove the risky parts** | | | |
| 0.1 | Install tools, `git init`, scaffold Wails | Haiku | `wails build` produces an exe, `go test -race ./...` runs |
| 0.2 | Engine design: interfaces, job state machine, error taxonomy, concurrency model | **Opus** | `docs/engine-design.md` and compiling Go interfaces |
| 0.3 | Fake S3 with synthetic data, fault injection, and test-prefix guard | Sonnet | Multipart and ranged GET pass, faults are scriptable |
| 0.4 | Scheduler, limiter, part sizing, upload path | **Opus** | Simulated 2 TB upload passes under race detector with bounded memory |
| 0.5 | Download path and NTFS write strategy comparison | Sonnet | Strategy results recorded for a 300 GB local file |
| 0.6 | `s4engine` CLI and minimal Wails file-drop window | Sonnet | CLI transfers against fake S3, dropped paths are logged |
| 0.7 | **Gate:** real AWS test, tuning, go/no-go | **Opus** | At least 80% of line rate on real 10 GB transfers, memory bounded |
| **M1: connect and browse** | | | |
| 1.1 | Profiles, vault storage, `credential_process`, profile switching | Sonnet | Both credential types work, helper runs hidden |
| 1.2 | Bucket list, add by name, region detection, progressive listing | Sonnet | 100,000 fake objects list, sort and page in Go |
| 1.3 | App shell and visual design system | **Opus** | Shell and tokens approved |
| 1.4 | Virtualized browser table and navigation | Sonnet | 100,000 rows scroll and sort smoothly |
| **M2: transfers** | | | |
| 2.1 | Retry, pause, in-session resume, credential recovery, orphan cleanup | **Opus** | Fault suite passes, including kill and relaunch |
| 2.2 | Queue service and Wails bindings | Sonnet | Queue is driven through bindings in tests |
| 2.3 | Folder transfers, Windows filename and path safety | Sonnet | Invalid, colliding and traversal keys are reported, never written |
| 2.4 | Queue UI, drag-in, transfer settings | Sonnet | Full transfers work from the UI |
| **M3: object operations** | | | |
| 3.1 | Delete, create folder, presign, metadata view, tag editing | Sonnet | All operations pass against fake S3 and in the UI |
| **M4: hardening** | | | |
| 4.1 | Independent review of engine, journal and path safety | **Opus** | Findings are filed and ranked ([review-4.1.md](review-4.1.md)) |
| 4.2 | Fix findings, run the full automated suite | Sonnet | Suite is green under race detector |
| 4.3 | Real AWS verification with hands-on checks | Sonnet | Verification checklist is complete |
| 4.4 | Release build, README, deferred Mac checklist | Haiku | Exe runs from a clean folder |
| 4.5 | Listing refresh after uploads, location kept across a window reload, and smaller fixes found in 4.3 | Sonnet | Both pass in `wails dev` and the built exe |

**Escalate from Sonnet to Opus when:** a race or deadlock survives two fix attempts, an engine contract must change, a throughput target is missed, or data integrity is in doubt.

## Verification

Simulated, against the local fake S3:

1. A 2 TB upload and download with synthetic data, with matching hashes and bounded memory.
2. Network drops, 503 storms and stalls mid-transfer, with no completed part re-sent.
3. Expired credentials pause the job, and it continues after credentials return.
4. Cancel leaves no multipart uploads. Killing the app leaves none after the next launch.
5. 100,000 objects scroll and sort responsively.
6. Invalid Windows names, collisions and traversal keys are reported and never written.

Real AWS, about 10 GB of data, all inside the test prefix:

7. Upload and download round trip with matching hashes, at 80% or more of line rate.
8. A short-lived `credential_process` credential expires during an upload and the transfer recovers.
9. `ListMultipartUploads` is empty for the test prefix after cancel and after relaunch.
10. A presigned URL works before expiry and fails after it. Tags can be edited on a multipart object.

Hands-on in the app:

11. Drag in a file, a folder and a mixed selection from Explorer.

**Known limit:** a 10 GB real run lasts well under a minute at these speeds, so it cannot prove multi-hour sustained throughput on real AWS. The simulated 2 TB runs cover engine stability, but not long-duration behavior of the real network path.
