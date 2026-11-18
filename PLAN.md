# Plan: personal S3 desktop client for mezzanine-scale transfers

## Context

The goal is a reduced-scope replacement for the S3 Browser Windows app, for one user, focused on moving very large media files quickly. The project folder is empty, so this is a new build. The app gets its own name and visual design.

## Platform verdict: build Windows + Mac from one codebase, with Windows as the primary target

Cross-platform is reasonable because the interview removed the features that are expensive to make work on both systems. Drag-out to Explorer, a built-in local file pane, signing and Mac parity are all out of scope. What remains uses cross-platform APIs: the S3 client, file I/O, native drag-in, file dialogs and the OS credential vault.

Mac costs one extra build on the Mac and a smoke test. Windows gets all performance tuning.

Revisit this decision if drag-out to Explorer/Finder or signed Mac distribution becomes a requirement.

## v1 scope

**In scope**
- Sign-in with AWS CLI profiles, or static access keys stored in Windows Credential Manager / macOS Keychain
- Browse buckets and prefixes, with up to 100,000 objects under one prefix
- Upload files and folders, including drag-in from Explorer/Finder
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

**Assumptions (the access question had no preference)**
- One active profile at a time, with a profile switcher
- Buckets can be added by name when a role cannot list all buckets
- Each bucket's region is detected automatically
- Uploads inherit the bucket's default encryption

## Recommended stack

| Layer | Choice | Reason |
|---|---|---|
| Desktop shell | Wails v2 (stable) | Go backend with a system webview. Its file-drop API gives the backend absolute paths, so file data never passes through the UI. Wails v3 is still beta. |
| Backend | Go + AWS SDK for Go v2 low-level S3 client | Good fit for highly parallel I/O, and the same code builds on both platforms. |
| Transfer engine | Custom multipart engine | The app needs direct control of part size, concurrency, bandwidth, retries and progress. The Go `feature/s3/manager` module is deprecated and its replacement `feature/s3/transfermanager` is still v0.x. |
| UI | React + TypeScript + Vite + a virtualized table | Handles 100,000 rows without rendering them all. |
| Node version | Current Active LTS | Confirm on nodejs.org at scaffold time and pin it. |

## Transfer engine requirements

- **Upload parts:** read each part directly from its file offset with no full-part buffering. Otherwise memory use becomes part size × concurrency.
- **Part size:** use the larger of the user's setting and `object size / 10,000`, rounded up, within S3's 5 MiB to 5 GiB part limits.
- **Downloads:** use parallel ranged GETs into a temporary file and rename it on completion.
- **NTFS behavior:** out-of-order writes far past the end of a large file can trigger zero-fill stalls. Milestone 0 tests this and sets the download write strategy.
- **Credentials:** use a refreshing credential provider. A 2 TB transfer at 2.5 Gbps takes about 1.8 hours, which outlasts a 1-hour role session. If refresh fails, pause the job, keep completed parts in memory, and continue when credentials return.
- **Orphaned uploads:** abort multipart uploads on cancel and failure. Keep a small on-disk list of active upload IDs and abort leftovers on next launch, so a crash does not leave multi-TB parts accruing storage cost.
- **Windows filenames:** S3 keys can contain characters or names Windows forbids. Downloads must report these clearly and never silently overwrite a colliding file.

## Milestones

### M0: Prove the risky parts on Windows (gate before UI work)
Build a headless engine command natively on Windows and test a 100 GB+ object in both directions.
- **Pass:** sustain at least 2.5 Gbps, with 5 Gbps as the target, and keep memory bounded.
- Compare download write strategies on NTFS.
- Build a minimal Wails window that accepts a dropped file and folder and logs absolute paths.
- If throughput fails, find the bottleneck before building further.

### M1: Connect and browse
Profiles, static keys, profile switcher, bucket list, add bucket by name, region detection, and progressive listing with a virtualized table.

### M2: Transfers
Connect the engine to the UI: queue, progress, pause/cancel, concurrency and bandwidth controls, drag-in, folder upload and download, retry, credential recovery and orphan cleanup.

### M3: Object operations
Delete with confirmation, create folder, presigned URLs with the effective expiry shown, metadata view and tag editing.

### M4: Hardening and Mac pass
Run the verification below, fix defects, build on the Mac, and complete the Mac smoke test.

## Project setup

- Keep the repo on the Windows filesystem, such as `C:\src\<app-name>`, because Windows builds from a `\\wsl$` path are slow.
- Install Go, the current Node Active LTS and the Wails CLI on Windows.
- Read profiles from Windows `%USERPROFILE%\.aws`. Profiles that exist only in WSL will not appear.
- Build the Mac app on the Mac with Xcode Command Line Tools. A local build avoids the Gatekeeper quarantine that downloaded unsigned apps receive.

## Verification

1. Upload and download a 100 GB+ object, recording sustained throughput and peak memory against the M0 target.
2. Round-trip a multi-TB object once and compare source and downloaded hashes.
3. Disconnect the network mid-transfer, reconnect, and confirm the job continues without re-sending completed parts.
4. Let a 15-minute role session expire during an upload and confirm the transfer recovers.
5. Cancel an upload, then kill the app during another. Confirm `ListMultipartUploads` is empty after cancel and after next launch.
6. Browse a prefix with 100,000 objects and confirm scrolling and sorting stay responsive.
7. Drag in a file, a folder and a mixed selection on Windows and Mac.
8. Download keys containing Windows-invalid names and confirm they are reported correctly.
9. Open a presigned URL before and after expiry, and edit tags on an object over 5 GB.
10. On Mac, run a smoke test of sign-in, browse, upload, download, delete and presign.
