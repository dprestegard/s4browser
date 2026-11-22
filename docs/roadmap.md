# Roadmap

Planned work after v0.0.1. History is in [progress.md](progress.md).

## Mac release

Stage 5.1 (see progress.md) made the app build and pass its tests on macOS 13+ on Apple silicon. Left before a Mac release:

- [ ] **Look at the native window.** Check the inset title bar (traffic lights over the top bar's left end, dragging by the wordmark and empty space), the icon, and the About box. Only the page was checked, in a browser.
- [ ] **Finder drag and drop** of a file, a folder and a mixed selection onto the listing.
- [ ] **Real S3** with `S4_WRITE_PREFIX`: a large upload and download with matching hashes, throughput (the 4 MiB send buffer is capped by `kern.ipc.maxsockbuf`), commit time of a large download (`F_FULLFSYNC`), and memory.
- [ ] **Kill mid-upload and relaunch:** no multipart uploads are left.
- [ ] **Full volume:** a download to a nearly full volume pauses, and resumes after space is freed.
- [ ] **A real `credential_process` helper** started from Finder (Homebrew's `/opt/homebrew/bin` and `/usr/local/bin` are now added to its `PATH`).
- [ ] **Second copy:** `open -n` starts a second copy, which says another copy owns the transfer journal. Decide whether a Mac copy should refuse to open instead (Wails' `SingleInstanceLock` would bring the first window forward).
- [ ] **Signing:** a Developer ID and notarization, so the app opens without clearing the quarantine flag. This would add the certificate and notarization credentials to the CI workflow as secrets. go-keyring stores keys through `/usr/bin/security`, so the app's own signature does not affect Keychain access.
- [ ] **Universal build** (`darwin/universal`) for Intel Macs. CI builds it (`.github/workflows/build.yml`), but it has not been run on an Intel Mac.
- [ ] **Decide on path rules for Mac downloads.** Windows rules apply on every system (README, Known limits). Relax them on darwin only if users hit them.
- [ ] **Decide on NFC normalization** of upload keys.

## VPC endpoints

Not started.
