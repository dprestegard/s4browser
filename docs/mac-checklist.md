# Deferred Mac checklist

The plan defers the Mac build until after v1: keep the code portable, compile-check it for macOS, and do no Mac stage (`docs/implementation-plan.md`, "Decisions"). This is the list to work through when a Mac build is wanted. Nothing here has been run on a Mac.

## Where things stand (stage 4.4, 2026-11-20)

- `GOOS=darwin GOARCH=arm64 go vet` passes for every package, including the app's `main` package, `internal/...` and `cmd/...`.
- Platform code is split by build tags:

  | Area | Windows | macOS (and other Unix) |
  |---|---|---|
  | Download write strategies | `fsx/strategy_windows.go`: NTFS sparse, preallocate, valid-data | `fsx/strategy_other.go`: `truncate` (sparse on APFS) |
  | No-replace rename | `MoveFileExW` without `REPLACE_EXISTING` | `fsx/rename_link.go`: hard link, then remove the temp name |
  | Free space, volume, full disk | `GetDiskFreeSpaceEx`, `GetVolumePathName`, `ERROR_DISK_FULL` | `statfs`, device number, `ENOSPC`/`EDQUOT` |
  | Journal lock | `LockFileEx` | `flock` (`journal/lock_unix.go`) |
  | `credential_process` | Hidden `cmd.exe /c` in a job object | `sh -c` (`creds/process_other.go`) |
  | Key storage | Windows Credential Manager | Keychain, through `zalando/go-keyring` |
  | Memory metrics (CLI) | `cmd/s4engine/mem_windows.go` | `mem_other.go` |
- `build/darwin/Info.plist` is still the Wails template. Its bundle ID is `com.wails.s4browser`, and its minimum macOS is 10.13.

## Build on the Mac

1. Install Xcode Command Line Tools (`xcode-select --install`), Go 1.27, Node.js 26 LTS and the Wails CLI v2.17.1. Run `wails doctor` and fix what it reports.
2. Clone the repo, then run `wails build -clean -trimpath -platform darwin/universal`, or `darwin/arm64` for Apple silicon only.
3. Run `go test -race ./...`. Every test was written on Windows. Expect failures in tests that assume Windows behaviour (see below) and decide for each: skip it on darwin, or fix the code.
4. Run the app from `build/bin/s4browser.app`.

## Decide before building

- [ ] **Bundle ID and signing.**
  - Change `CFBundleIdentifier` from `com.wails.s4browser` to your own.
  - An unsigned app is blocked by Gatekeeper. For personal use, sign ad hoc (`codesign --force --deep -s - s4browser.app`) or remove the quarantine flag. Signing and notarization were out of scope for v1.
- [ ] **Keychain prompts.** `go-keyring` stores keys as generic passwords. An unsigned or ad hoc signed app gets a new identity on every build, so macOS asks again for access to stored keys after each rebuild. Signing with a stable identity avoids that.
- [ ] **Window options.** `main.go` sets only `Windows: &windows.Options{Theme: windows.Dark}`. Add `Mac: &mac.Options{...}` (title bar and appearance) to match the dark theme.

## Check in code

- [ ] **No-replace rename on volumes without hard links.** `fsx/rename_link.go` needs hard links, which exFAT, FAT32 and some SMB shares do not support. There, the download fails at commit and its temp file is removed. On macOS, use `renamex_np(..., RENAME_EXCL)` (APFS and HFS+), and keep the hard link as the fallback.
- [ ] **Unicode normalization of names.** Finder hands out names as written, but HFS+ stores them decomposed (NFD), and S3 keys are compared byte for byte. Decide whether uploads normalize keys to NFC. Without it, a file uploaded from a Mac and one from Windows can have keys that look the same but differ.
- [ ] **Path rules.** `fsx/pathsafe.go` applies Windows rules on every platform: reserved names (`CON`, `NUL`…), forbidden characters, trailing dots and spaces, and the 259-character `MaxPath`. That is safe on a Mac but stricter than needed. Relax `MaxPath` and the Windows-only names for darwin if keys are refused that macOS could hold. The case-insensitive collision check is right for the default APFS.
- [ ] **Durability of commit.** Go's `File.Sync` on darwin uses `F_FULLFSYNC`, which is slower than on Windows. Measure commit time for a large download.
- [ ] **Socket send buffer.** `s3x.TuneTransport` asks for a 4 MiB send buffer per connection. macOS caps it at `kern.ipc.maxsockbuf`. Measure upload throughput, as stage 0.7 did on Windows, before relying on the defaults.
- [ ] **`credential_process` helpers.** They run through `sh -c` with the user's environment. A helper that needs a login shell's `PATH` (Homebrew in `/opt/homebrew/bin`) may not be found when the app is started from Finder. Test with the real helper.
- [ ] **Data folder.** `os.UserConfigDir()` is `~/Library/Application Support/s4browser`. Check that the journal, profiles, buckets and settings land there, and that the WebView data does not land next to the app.

## Smoke test on the Mac

Run these against the fake first (`wails dev -tags devfake`), then against a real bucket with `S4_WRITE_PREFIX` set to a test prefix:

- [ ] The window opens, the theme follows the setting, and the app icon shows (`build/appicon.png`).
- [ ] Profiles from `~/.aws` are listed. Adding access keys stores them in the Keychain, and they still work after a restart.
- [ ] Bucket list, add by name, region detection, and a folder of 100,000 objects scrolls and sorts.
- [ ] Drag a file, a folder and a mixed selection from Finder onto the listing. The drop asks, then uploads. Check that Wails' file drop is enabled on macOS in v2.17.1 (`DragAndDrop.EnableFileDrop`).
- [ ] Upload and download a large file. Hashes match, throughput is acceptable, and memory stays bounded.
- [ ] Cancel, and kill the app mid-upload, then relaunch. No multipart uploads are left.
- [ ] A download to a nearly full volume pauses with "the drive is full", and resumes after space is freed.
- [ ] Delete, new folder, share link, metadata and tag editing.
- [ ] Cmd+R and F5 reload the folder's listing, not the page. A reload keeps the location.
- [ ] Quitting with transfers running asks first.
