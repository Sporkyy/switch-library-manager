# Testing

How to test Switch Library Manager on macOS, Linux and Windows, from unit
tests up to the real app on network shares. Steps 1–4 are safe to run
anywhere. They use synthetic files in scratch directories that the tests create
and remove themselves.

Organize and cleanup move and delete files. Never point a test, or a test build,
at a real game library. Use a folder holding a copy of a few files.

For background on SMB move behavior, see [organize-smb.md](organize-smb.md).

## Requirements

| Tool | Version | Notes |
| --- | --- | --- |
| Go | 1.27 | Module is in `src/`; run Go commands from there |
| Wails CLI | v2.15.0 | Only for building the app (step 5) |
| `gh` | any | Optional, for CI builds and artifact downloads |

Platform build dependencies:

- **macOS:** Xcode Command Line Tools (`xcode-select --install`).
- **Linux (Arch/CachyOS):** `sudo pacman -S --needed go base-devel gtk3 webkit2gtk-4.1 pkgconf`
- **Linux (Debian/Ubuntu):** `sudo apt-get install -y build-essential libgtk-3-dev libwebkit2gtk-4.1-dev`.
  On Ubuntu 22.04 and older, use `libwebkit2gtk-4.0-dev` and leave out the
  `webkit2_41` tag in step 5.
- **Windows:** the WebView2 runtime, which ships with Windows 10 and 11.

Install Wails:

```sh
go install github.com/wailsapp/wails/v2/cmd/wails@v2.15.0
export PATH="$(go env GOPATH)/bin:$PATH"   # PowerShell: $env:Path += ";$(go env GOPATH)\bin"
wails doctor
```

## Run everything at once (macOS and Linux)

`scripts/test-mounts.sh` runs steps 1, 3 and 4 against every mounted SMB
share and prints one PASS/FAIL line per check:

```sh
scripts/test-mounts.sh --list    # show the shares and servers it found
scripts/test-mounts.sh           # run everything
scripts/test-mounts.sh --no-race /Volumes/peach_data /Volumes/luigi_data3
```

It runs these checks:

- **Code checks:** `gofmt`, `go vet`, `go test` and `go test -race`.
  `--no-race` skips the race run.
- **For each share:** the file-ID probe, and the scratch Organize test with the
  intake folder on local disk.
- **Across servers:** the scratch Organize test with the library on one server
  and the intake on another, in both directions. It uses one share per server.
- **Cleanup:** a check that no `.slm-*` scratch entries are left on any share.

Which shares it tests:

- With no share paths, it finds them itself: `cifs`/`smb3` mounts on Linux,
  `smbfs` mounts on macOS.
- Passing paths tests only those. A path that isn't a mounted share still gets
  the per-share tests, but it isn't used for the cross-server runs.

How to read the results:

- **Failures:** each FAIL line is followed by the message from each failing
  test. The summary at the end points to the full log.
- **Lock warning:** a `lock refused` note means a Linux mount refused the
  synchronous-close lock. Record its options from step 2.
- **Exit code:** 0 when everything passes, 1 when a check fails, 2 for a usage
  or setup error.

A full run takes about 30 seconds per share, plus about 30 seconds per pair of
servers. On Windows, follow the steps below by hand.

## 1. Unit tests

From `src`:

```sh
gofmt -l .
go vet ./...
go test ./...
go test -race ./...
```

The tests point `HOME`, `USERPROFILE` and `XDG_CONFIG_HOME` at a temporary
directory (`src/internal/testhome`), so a real `~/.switch/prod.keys` is never
loaded. The tests run on synthetic fixtures and filename-only metadata. No keys
or game files are needed. The race detector needs cgo; on Windows that means a C
compiler on `PATH`.

Platform-specific files only compile and run on their own OS. A cross-compile
check catches build errors early:

```sh
GOOS=linux   go vet ./...
GOOS=windows go vet ./...
GOOS=darwin  go vet ./...
```

That checks compilation only. It doesn't run anything, so run step 1 natively
on each OS you care about.

## 2. Inspect the share

Record the share type and mount options before testing on it. Results differ
by server and by client.

**macOS:**

```sh
mount | grep smbfs
smbutil statshares -m /Volumes/SHARE
```

Check that `FILE_IDS_SUPPORTED` is listed. Without it, macOS invents file IDs
for that share. Organize still works there, but IDs change when a file is
renamed.

**Linux:**

```sh
findmnt -t cifs,smb3 -o TARGET,SOURCE,FSTYPE,OPTIONS
findmnt --target /path/to/library -o TARGET,SOURCE,FSTYPE,OPTIONS
sudo dmesg | grep -i -E 'cifs|serverino'
```

What to look for:

- `serverino`, the default, uses the server's file IDs. `noserverino`
  generates them on the client.
- The kernel message "Autodisabling the use of server inode numbers" means the
  kernel fell back to client IDs.
- A Dolphin/KIO-FUSE path (`fuse.kio-fuse`) is a different environment from a
  kernel `cifs` mount. Record which one is actually in use.

**Windows:**

```powershell
Get-SmbConnection | Format-List ServerName,ShareName,Dialect
net use
```

Test against both mapped drive letters and UNC paths (`\\server\share\...`) if
you use both.

## 3. File-ID stability probe

This checks the guarantee the copy fallback depends on: after closing the copy
it created, the destination path still identifies the same file. Each root gets
a `.slm-fileid-test-*` scratch folder, which is removed afterwards. Nothing else
is touched.

The roots must already exist. Separate them with `:` on macOS and Linux, and
with `;` on Windows.

```sh
# from src
SLM_FILE_ID_TEST_ROOTS=/Volumes/share1:/Volumes/share2 \
  go test ./backend/process -run '^TestFileIdentityStabilityOnMount$' -v -count=1
```

```powershell
# from src, PowerShell
$env:SLM_FILE_ID_TEST_ROOTS = 'Z:\;\\server\share'
go test ./backend/process -run '^TestFileIdentityStabilityOnMount$' -v -count=1
```

How to read the summary line:

- **`handle vs path after close N/10 mismatched`:** the line that matters. Any
  N above 0 fails the test. On that share, copy-fallback moves keep both the
  copy and the source and report an error. No data is lost.
- **`path vs reopen`, `after 3s`, `after rename`:** information only. A change
  in these points to IDs generated on the client.
- **ID values:** on Windows they show as `n/a`, because only `os.SameFile` can
  compare them. The comparison results are still valid.

Without either variable set, the test is skipped. `SLM_ORGANIZE_TEST_ROOT`
works as a single-root fallback.

## 4. Scratch Organize test on a real mount

This runs the real backend Organize workflow against a mount, inside uniquely
named `.slm-organize-test-*` folders, using small synthetic `.nsp` files named
with title IDs. It doesn't exercise encrypted parsing or the GUI.

```sh
# from src
SLM_ORGANIZE_TEST_ROOT=/path/on/library/mount \
SLM_ORGANIZE_TEST_INTAKE_ROOT=/path/on/other/mount \
  go test ./backend/app -run '^TestOrganizeLibraryScratchMount$' -v -count=1
```

There are four scenarios:

| Scenario | What it checks |
| --- | --- |
| `library_rename` | A file inside the library is renamed. |
| `intake_same_mount` | A file in an intake folder on the same mount is imported. |
| `intake_other_mount` | A file is imported from `SLM_ORGANIZE_TEST_INTAKE_ROOT`. If that's local disk and the library is a share, this is a move across filesystems. |
| `late_collision` | A file appears at the destination after planning. Both files must survive and an error is logged. |

Each root left unset defaults to a local temp directory. Running with no
variables at all is therefore a local-disk test.

On a real mount the test waits 2.5 seconds before backdating each fixture and
again before checking the destination, so each scenario takes about 7 seconds.
The waits outlast the Linux CIFS deferred close (`closetimeo`) and attribute
cache (`actimeo`), so the check sees the time the server keeps. On Linux the
check reads it with `statx(AT_STATX_FORCE_SYNC)`. A mount with a `closetimeo`
above 2 seconds can still fail the fixture check.

## 5. Build the app

**From CI.** You can start this from any machine with `gh`, against any branch
pushed to the fork:

```sh
gh workflow run build-master.yml --ref BRANCH
gh run watch
gh run download --name ARTIFACT
```

| Artifact | Use it on |
| --- | --- |
| `switch-library-manager.darwin-arm64` | Apple Silicon Mac |
| `switch-library-manager.darwin-amd64` | Intel Mac |
| `switch-library-manager.linux-amd64-webkit41` | Arch/CachyOS, Ubuntu 24.04+, Fedora |
| `switch-library-manager.linux-amd64-webkit40` | Ubuntu 22.04 and older |
| `switch-library-manager.windows-amd64` | Windows |

**Locally.** Start from the repository root:

```sh
bash scripts/prepare-wails-build-assets.sh   # Windows: scripts\prepare-wails-build-assets.ps1
cd src
```

Then run the commands for your platform:

- **macOS:**

  ```sh
  wails generate module
  wails build -platform darwin/amd64   # or darwin/arm64
  ```

- **Linux, WebKitGTK 4.1:**

  ```sh
  wails generate module -tags webkit2_41
  wails build -platform linux/amd64 -webview2 embed -tags webkit2_41
  ldd build/bin/switch-library-manager | grep -E 'webkit|not found'
  ```

  The `ldd` output should list `libwebkit2gtk-4.1.so`, with no `not found`
  lines.

- **Windows:**

  ```sh
  wails generate module
  wails build -platform windows/amd64 -webview2 embed
  ```

The output goes to `src/build/bin`.

## 6. Real-app testing

Keep the test build's state apart from your normal install:

| OS | Where settings, the database and `slm.log` live | How to isolate |
| --- | --- | --- |
| Linux, Windows | Next to the executable | Copy the binary into its own folder, for example `~/slm-test`. |
| macOS | `~/Library/Application Support/Switch Library Manager` | Shared with any installed copy. Back up that folder first, or quit the normal app and restore the folder afterwards. |

Before running Organize:

1. Point the library folder at a **copy** of a few files on the share under
   test.
2. Rename one file so that Organize has something to do. For a move across
   filesystems, also enable an intake folder on a different filesystem.
3. Watch the log while Organize runs:
   - macOS: `tail -f ~/Library/Application\ Support/Switch\ Library\ Manager/slm.log`
   - Linux: `tail -f ~/slm-test/slm.log`
   - Windows: `Get-Content .\slm.log -Wait`

Useful log lines:

- `Organization plan built`: includes `planned_moves`. If it's 0, no move was
  attempted.
- `Exclusive rename unavailable; copying to an exclusively created destination`:
  the copy fallback was used.
- `Moved file:`: one line per completed move.
- `Organization failed`: the failing operation, its paths, and the move counts.
  Execution stops at the first failure.

## Reporting results

For each environment, record:

- the OS and version (`sw_vers`, `uname -srmo`, or `winver`);
- the share server and type, and the mount options from step 2;
- the step 3 summary line for each share;
- the step 4 result for each scenario;
- for any failure: the exact `slm.log` text; whether the source, the
  destination, or both still exist afterwards; and any leftover `.slm-move-*`
  or `.slm-probe-*` files in the destination folder.

Say whether the run used synthetic fixtures, filename-only metadata, or real
encrypted files. Never commit keys or game images.

### Known results

| Environment | Probe (close / rename) | Scratch Organize |
| --- | --- | --- |
| macOS 15 (Darwin 24.6), Intel, local APFS | 10/10 stable / stable | passed |
| macOS smbfs, luigi and mario shares (`FILE_IDS_SUPPORTED`) | 10/10 stable / stable | passed (luigi_data3), with intake on local disk, and from luigi_data3 into local disk |
| macOS smbfs, peach_data, UGreen NAS (no `FILE_IDS_SUPPORTED`) | 10/10 stable / changed 10/10 | passed, with intake on local disk and on luigi_data3, and from peach_data into local disk; before the reopen-and-restore change, moved files kept the copy time |
| Linux (CachyOS), kernel cifs, luigi, mario and peach shares (`serverino`) | 10/10 stable / stable | passed on all seven shares, with intake on local disk, the same server and another server |
| Windows | not run | not run |

### Pending verification

#### Linux: shared close-and-restore refactor (November 15, 2026)

The SMB close-and-restore step that Linux added in `37061df` now lives in
`src/backend/process/close_exclusive_copy_unix.go`, and macOS uses it too.
Linux behavior should not have changed. It still locks before the close, then
reopens and restores the times. On macOS its tests were only compiled for
Linux, never run. On the CachyOS machine, from the repository root:

```sh
git switch fix/organize-exclusive-copy-fallback
git pull --ff-only
scripts/test-mounts.sh --list   # all seven cifs shares on luigi, mario and peach should appear
scripts/test-mounts.sh
```

Then confirm that the Linux-only close tests ran, not just compiled:

```sh
cd src
go test ./backend/process -run 'CloseExclusive|LinuxSynchronous|LinuxFileSystem' -count=1 -v | grep -E '^(--- |ok|FAIL)'
```

That command should list 13 passing tests: six `TestCloseExclusiveCopy*`, five
`TestCloseExclusiveCopyOnLinux*`, `TestLinuxSynchronousCloseLockOnLocalFile`
and `TestLinuxFileSystemTypeReportsClosedFile`.

A moved file losing its time shows up as an Organize FAIL with a
`want 2020-02-03 04:05:06` message. That would be a regression from
`37061df`'s Linux results. When everything passes, add the results to the
Linux row of the known-results table, delete this section, and commit.
