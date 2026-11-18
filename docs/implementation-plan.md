# s4browser: full implementation plan

## Context

`F:\s4browser\PLAN.md` defines a personal S3 desktop client for moving very large media files quickly. The folder holds only that file, so this is a new build. This plan turns it into stages that each run in a fresh session on the cheapest model that can do the work safely.

## Decisions from the interview

| Topic | Decision | Effect on PLAN.md |
|---|---|---|
| App name | `s4browser`, personal use | Name used for binary, config folder and vault entries |
| Media location | Local NVMe (C:, E:, F:) | Tune for NTFS on NVMe only. HDD, SMB and exFAT are not tuned |
| Uplink | 2 to 5 Gbps | Pass mark becomes 80% of measured line rate, replacing fixed 2.5/5 Gbps |
| Real AWS testing | Minimal, about 10 GB | Replaces the real 100 GB and multi-TB runs. Scale is proven against a local fake S3 |
| Credentials | Static keys and `credential_process` | Only these two are tested. Other profile types may work but are unsupported |
| Test bucket | One real bucket, dedicated prefix | Test tooling refuses writes outside `s4browser-test/<run-id>/` |
| Mac | Deferred until after v1 | Keep code portable, compile-check engine packages for macOS, no Mac stage |
| Execution | New session per stage | Each stage has a self-contained brief and a model assignment |

**Correction to PLAN.md:** Wails v3 is no longer beta. v3.0.0 shipped on 2026-10-26. Stay on **Wails v2.17.1** anyway, because v3 has been stable for only three weeks and v2 covers everything this app needs.

## Toolchain

Already installed: Git 2.51, AWS CLI 2.34, WebView2 runtime 157, winget, Docker. Hardware: Ryzen 9 9950X3D, 93 GB RAM, 10 GbE.

Install in stage 0.1:

| Tool | Version | Install |
|---|---|---|
| Go | 1.27.4 | `winget install --id GoLang.Go --exact` |
| Node.js (Active LTS) | 26.13.1 | `winget install --id OpenJS.NodeJS.LTS --exact` |
| Wails CLI | v2.17.1 | `go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.1` |
| staticcheck | latest | `go install honnef.co/go/tools/cmd/staticcheck@latest` |

Not needed: GCC (Wails on Windows builds without CGO, but `go test -race` does need CGO, so see below), NSIS, UPX, code signing, GitHub CLI.

**Race detector:** `go test -race` requires a C toolchain on Windows. Stage 0.1 also installs `BrechtSanders.WinLibs.POSIX.UCRT` through winget, used only for tests. The race detector is mandatory for this engine.

Libraries: `aws-sdk-go-v2` (`service/s3` v1.115, `config` v1.33), `golang.org/x/time/rate`, `golang.org/x/sys/windows`, `zalando/go-keyring` v0.2.8, React 19, Vite, TypeScript, `@tanstack/react-virtual` v3. Stage 0.1 keeps the Vite and TypeScript versions that work with the Wails template rather than forcing the newest majors.

## Architecture

```
F:\s4browser
├─ main.go, app.go           Wails entry point and bindings (thin)
├─ cmd/s4engine/             Headless CLI for M0 and benchmarks
├─ internal/engine/          Scheduler, upload, download, retry, limiter, progress
├─ internal/journal/         On-disk list of active multipart upload IDs
├─ internal/fsx/             NTFS write strategies, path safety (build-tagged)
├─ internal/s3x/             Client factory, tuned transport, region cache, listing
├─ internal/creds/           Profiles, static keys in vault, credential_process
├─ internal/objects/         Delete, create folder, presign, metadata, tags
├─ internal/fakes3/          Test server: synthetic data and fault injection
├─ frontend/                 React UI
└─ docs/                     implementation-plan.md, engine-design.md, progress.md
```

Key design rules:

- **File data never crosses the UI bridge.** Drops deliver absolute paths to Go.
- **Go owns the listing.** The UI requests visible row windows, and Go sorts. This keeps 100,000 rows out of webview memory.
- **Progress is batched** and sent to the UI about 5 times per second.
- **Uploads** use `io.SectionReader` over a shared file handle, so memory does not scale with part size. Parts carry CRC32 checksums.
- **Downloads** use `If-Match` on the ETag, so an object replaced mid-transfer fails clearly instead of producing a corrupt file.
- **Stalled connections** are cancelled by an idle-byte watchdog and retried.
- **Engine retries own the policy.** SDK retries are disabled on part requests so attempts are not multiplied.
- **Limiter and concurrency settings** can be changed while transfers run.

Risks the original plan did not cover:

- **Path traversal:** S3 keys containing `..` or absolute paths must never write outside the chosen download folder.
- **`credential_process` console flash:** a GUI app launching a console helper can pop up a window on Windows. Stage 1.1 must run the helper hidden, which may need a custom provider.
- **Presign expiry:** the effective expiry is the shortest of the requested time, 7 days and the credential's own expiry.
- **Versioned buckets:** delete creates delete markers, so the confirmation wording must say so.
- **Disk space:** check free space before starting a download.

## Stages

Each stage runs in a new session. It reads `docs/implementation-plan.md`, `docs/engine-design.md` and the last entry in `docs/progress.md`, finishes its done criteria, commits, and appends a short handoff note.

| Stage | Work | Model | Done when |
|---|---|---|---|
| **M0: prove the risky parts** | | | |
| 0.1 | Install tools, `git init`, scaffold Wails, copy this plan into `docs/` | Haiku | `wails build` produces an exe, `go test -race ./...` runs |
| 0.2 | Engine design: interfaces, job state machine, error taxonomy, concurrency model | **Opus** | `docs/engine-design.md` and compiling Go interfaces |
| 0.3 | Fake S3 with synthetic data, fault injection, and test-prefix guard | Sonnet | Multipart and ranged GET pass, faults are scriptable |
| 0.4 | Scheduler, limiter, part sizing, upload path | **Opus** | Simulated 2 TB upload passes under race detector with bounded memory |
| 0.5 | Download path and NTFS write strategy comparison | Sonnet | Strategy results recorded for a 300 GB local file |
| 0.6 | `s4engine` CLI and minimal Wails file-drop window | Sonnet | CLI transfers against fake S3, dropped paths are logged |
| 0.7 | **Gate:** real AWS test, tuning, go/no-go | **Opus** | At least 80% of line rate on real 10 GB transfers, memory bounded |
| **M1: connect and browse** | | | |
| 1.1 | Profiles, vault storage, `credential_process`, profile switching | Sonnet | Both credential types work, helper runs hidden |
| 1.2 | Bucket list, add by name, region detection, progressive listing | Sonnet | 100,000 fake objects list, sort and page in Go |
| 1.3 | App shell and visual design system | **Opus** | Shell and tokens approved by you |
| 1.4 | Virtualized browser table and navigation | Sonnet | 100,000 rows scroll and sort smoothly |
| **M2: transfers** | | | |
| 2.1 | Retry, pause, in-session resume, credential recovery, orphan cleanup | **Opus** | Fault suite passes, including kill and relaunch |
| 2.2 | Queue service and Wails bindings | Sonnet | Queue is driven through bindings in tests |
| 2.3 | Folder transfers, Windows filename and path safety | Sonnet | Invalid, colliding and traversal keys are reported, never written |
| 2.4 | Queue UI, drag-in, transfer settings | Sonnet | Full transfers work from the UI |
| **M3: object operations** | | | |
| 3.1 | Delete, create folder, presign, metadata view, tag editing | Sonnet | All operations pass against fake S3 and in the UI |
| **M4: hardening** | | | |
| 4.1 | Independent review of engine, journal and path safety | **Opus** | Findings are filed and ranked |
| 4.2 | Fix findings, run the full automated suite | Sonnet | Suite is green under race detector |
| 4.3 | Real AWS verification with your hands-on checks | Sonnet | Verification checklist is complete |
| 4.4 | Release build, README, deferred Mac checklist | Haiku | Exe runs from a clean folder |

**Model rationale:** Opus handles the 6 stages where a subtle mistake is expensive or judgment sets direction: engine contracts, concurrency core, failure recovery, the performance gate, visual design and final review. Sonnet implements against those contracts. Haiku handles only mechanical setup and packaging.

**Escalate from Sonnet to Opus when:** a race or deadlock survives two fix attempts, an engine contract must change, a throughput target is missed, or data integrity is in doubt.

## What you need to do

- **Before 0.7:** provide the AWS profile, bucket name and region, plus a measured speed test result. Confirm whether the bucket has versioning on, because deleted test objects would otherwise keep costing money.
- **At 0.7:** decide go or no-go from the results.
- **At 1.3:** approve the visual design.
- **At 4.3:** perform drag-in checks and the credential expiry test.

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
