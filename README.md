# s4browser

A Windows desktop client for Amazon S3, built for moving very large media files quickly. You browse buckets, drag files and folders in from Explorer, download files and folders, and watch a transfer queue that retries on its own. It also deletes objects, creates folders, shares presigned links and edits tags.

Version 1.0.0 is for Windows 10 and 11 (x64). A Mac build is deferred: see [docs/mac-checklist.md](docs/mac-checklist.md).

## Install

1. Copy `s4browser.exe` to any folder. There is no installer, and nothing is written next to the exe.
2. Run it.

It needs the Microsoft Edge WebView2 runtime, which Windows 11 includes. On a machine without it, the exe offers to download it.

The app keeps its data in your user profile:

| Path | What |
|---|---|
| `%AppData%\s4browser\profiles.json` | Profile names, regions, the active profile. No keys |
| `%AppData%\s4browser\buckets.json` | Buckets added by name, per profile |
| `%AppData%\s4browser\transfer-settings.json` | Transfer settings |
| `%AppData%\s4browser\journal.json`, `journal.lock` | Unfinished multipart uploads and download temp files, so they can be cleaned up after a crash. No secrets |
| `%AppData%\s4browser\filedrop.log` | Paths dropped on the window |
| `%AppData%\s4browser.exe\` | WebView2's cache |
| Windows Credential Manager, `s4browser:profile/<name>` | Access keys you add in the app |

Only one copy of the app runs at a time. A second copy is refused, because it would clean up the first one's uploads.

## Credentials

The profile menu at the top left lists:

- **Profiles from your AWS files** (`~/.aws/config` and `~/.aws/credentials`, or `AWS_CONFIG_FILE` and `AWS_SHARED_CREDENTIALS_FILE`):
  - static keys
  - session keys written by a sign-in tool
  - `credential_process` helpers, which run hidden and run again a minute before their credentials expire.

  Role, SSO and web-identity profiles are listed but not supported.
- **Access keys you add** with "Add access keys…". The secret goes to Windows Credential Manager, never to a file.

One profile is active at a time. When a profile's credentials expire during a transfer, the transfer waits with the reason shown. Other profiles' transfers keep going. When you sign in again and choose "Load credentials again", the transfer continues where it stopped. It also resumes on its own if the profile's helper starts handing out working credentials.

## Using it

- **Buckets:** the bucket list shows every bucket the profile can list. A role that cannot list buckets can add one by name with **+**. Each bucket's region is detected.
- **Browsing:** a folder of up to 1,000,000 objects loads in the background and scrolls without delay. Sort by name, size, date or storage class. Select with click, Shift, Ctrl and Ctrl+A. Backspace goes up a folder. F5 or the reload button reloads the folder and keeps the selection.
- **Uploads:**
  - Drop files and folders from Explorer on the listing, or use Upload. A drop asks before it starts.
  - Folders upload with everything inside them.
  - An object with the same key is replaced.
  - The listing updates as uploads finish into the folder on screen.
- **Downloads:** select objects or folders and choose Download. A file that already exists locally is never overwritten. Names Windows cannot hold are refused, and so are paths longer than 259 characters. The download report lists what was refused and why.
- **Transfer queue:** pause, resume, cancel, retry and clear, per job or for all. Failed parts are retried automatically, and a broken connection resumes inside a part. Transfer settings:
  - files at once (default 4)
  - parts per file (16)
  - parts in total (32)
  - part size (64 MiB, raised automatically for very large files)
  - upload and download bandwidth caps.
- **Delete:** the dialog counts everything that will be deleted first and says what versioning means for the bucket. Deleting 100 objects or more asks you to type `delete`. In a versioned bucket a delete adds delete markers, and the old versions stay and keep costing money.
- **New folder** creates an empty `name/` object.
- **Details panel:** metadata, including checksum, encryption and version, plus tags (view and edit) and **Share link** (a presigned download link valid for 15 minutes to 7 days, shortened if the credentials expire sooner).

Closing the app while transfers run asks first. Closing cancels them and aborts their multipart uploads on S3.

## Known limits

- **No resume after closing.** Transfers continue only while the app is open. After a crash, the next launch aborts unfinished multipart uploads and removes partial downloads.
- **Downloads are not checked against S3's stored checksum.** Each part is fetched with `If-Match` on the object's ETag, and its range and the total size are checked, over TLS. A download is not compared end to end with the object's CRC32. Uploads are: each part carries a CRC32, and S3's composite checksum must match.
- **A full drive pauses a download.** Free space, then resume.
- **No S3-compatible endpoints,** ACLs, versioning UI, rename, copy, storage-class change or restore from Glacier.
- **Session-key profiles have no automatic refresh.** Sign in again with your tool, then use "Load credentials again".

## Building from source

Requirements: Go 1.27, Node.js 26 LTS, and the Wails CLI v2.17.1 (`go install github.com/wailsapp/wails/v2/cmd/wails@v2.17.1`).

```powershell
wails build -clean -trimpath -ldflags "-s -w"
```

The exe is `build\bin\s4browser.exe`. Its version comes from `info.productVersion` in `wails.json`. The icon is drawn from `build\appicon.png`, and Wails makes `build\windows\icon.ico` from it when that file is missing.

Development:

```powershell
wails dev
```

To run against an in-process fake S3 with no AWS access, build with the `devfake` tag:

```powershell
wails dev -tags devfake
```

Tests (the race detector needs a C compiler, such as WinLibs, and `CGO_ENABLED=1`):

```powershell
go test -race ./...
```

Also run `go vet ./...`, `staticcheck ./...` and `npx tsc --noEmit` in `frontend`.

### Test tooling

- **`cmd/s4engine`:** a command-line transfer tool on the same engine. It writes only under `<root><run-id>/` (default `s4browser-test/`) and has no flag that turns this off.
- **`cmd/s4credtest`:** a `credential_process` helper for testing credential expiry.
- **`S4_WRITE_PREFIX`:** when set, for example to `upload-test/run-1/`, the app refuses every upload, delete, new folder and tag change outside that prefix. Use it for verification against a real bucket.

## Documentation

- [docs/implementation-plan.md](docs/implementation-plan.md): scope, stages and verification.
- [docs/engine-design.md](docs/engine-design.md): the transfer engine.
- [docs/progress.md](docs/progress.md): what each stage did and found.
- [docs/review-4.1.md](docs/review-4.1.md): the engine review.
