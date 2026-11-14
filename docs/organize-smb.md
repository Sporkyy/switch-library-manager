# Organize on SMB

Investigation dated November 13, 2026. Organization experiments use uniquely created scratch directories and synthetic files, never an existing game library.

## Upstream comparison

Before this fix, this fork's `master` matched upstream `master` and tag `2.1.0` at `98d686ed8848900f728be798f8a1421fd54c2221`. The public upstream source also matched the local files by Git blob ID:

| File under `src/backend/process` | Upstream 2.1.0 blob |
| --- | --- |
| `move_file.go` | `97fb6b49ac5305374b4deed520b84407b6d78114` |
| `rename_noreplace_darwin.go` | `5b4cfcff6c6fd86266d33b80c5d28c0f9155b52b` |
| `rename_noreplace_linux.go` | `33eedbcfef95c4574bb26df4e2a6cd078feeefa3` |
| `organization_plan.go` | `0efe81ae9689e9de0432cc79a5e3fbfa2ffc914f` |

The reported code paths were still present: macOS used `renamex_np(RENAME_EXCL)`, Linux used `renameat2(RENAME_NOREPLACE)`, only a cross-device error selected the copy fallback, and publishing that copy used exclusive rename again. Execution stopped at the first failed move.

[Upstream issue #168](https://github.com/trembon/switch-library-manager/issues/168) remained open with 24 comments; its latest comment was November 5, 2026. The [v2.1.0 fix](https://github.com/trembon/switch-library-manager/commit/4744770099a82e9e9e07ca3b7edbb34e48d625f2) only made the macOS confirmation dialog's Yes/No button labels explicit. It did not change filesystem moves.

The later conversation contains two useful updates:

- [November 4: exclusive-rename explanation and Synology reproduction](https://github.com/trembon/switch-library-manager/issues/168#issuecomment-6631548890). The maintainer's [later statement that the app uses ordinary `os.Rename`](https://github.com/trembon/switch-library-manager/issues/168#issuecomment-6639488166) does not match the platform-specific implementation in upstream 2.1.0.
- [November 4: debug log supplied after the error dialog](https://github.com/trembon/switch-library-manager/issues/168#issuecomment-6640598757), followed by [November 5: another SMB report, without a proposed SLM fix](https://github.com/trembon/switch-library-manager/issues/168#issuecomment-6651934426).

The linked [RAWmakase Synology test](https://github.com/pch/rawmakase/issues/573#issuecomment-6335198506) measured macOS SMB 3.1.1 rejecting exclusive rename and hard links while permitting atomic exclusive creation. Its [Linux follow-up](https://github.com/pch/rawmakase/pull/676) separately measured kernel CIFS replacing another client's destination despite `RENAME_NOREPLACE`. These are supporting measurements on other systems, not a diagnosis of the reported CachyOS setup.

## Reproduced macOS boundary

Scratch tests used distinct source and unused destination names:

| Operation | Local APFS | SMB at `/Volumes/luigi_data3` |
| --- | --- | --- |
| Plain rename | Succeeded | Succeeded |
| `renamex_np(RENAME_EXCL)` | Succeeded | `Operation not supported` |
| Hard link | Succeeded | `Operation not supported` |

A same-path rename can succeed without exercising the failing operation. Finder's ability to rename a file also does not establish support for exclusive rename.

## Move behavior after this fix

Native exclusive rename remains the fast path where it is supported. If it is unsupported, SLM opens the final destination with `O_CREATE|O_EXCL` and writes through that returned handle. It does not check existence and then use plain rename. Existing destinations, including destinations created after preflight, remain untouched.

The fallback flushes and closes the complete copy, verifies the destination and rechecks the source before removing the source. The destination must still identify the file created by this move and have the expected size. Matching size and modification time cannot establish file identity. If the destination was replaced or its identity cannot be verified, the source remains, the unrecognized destination is left untouched, and the error reports both paths. SMB servers with unstable file IDs may therefore leave both copies and report an incomplete move. This conservative check adds no content hashing or full-file readback. Writes and metadata changes use the exclusively created handle, so a replacement at that pathname is not modified. Handle-based timestamps keep nanosecond precision on Linux and Windows; macOS is limited to microseconds.

For a cross-device move, SLM first renames an empty `.slm-probe-*` file in the destination folder. If that exclusive rename is unsupported, the source is copied straight to the exclusively created destination, so the data crosses the network once and no temporary copy is needed. If publication of a temporary copy is still refused, the temporary copy is removed and the original source is copied directly.

The final filename is visible while copying. If copying or verification fails, the source remains. The incomplete destination is removed when the path still identifies the file SLM created; otherwise, such as after a replacement by another client or when file identity cannot be confirmed, it is retained and its path is in the error. Inspect any retained destination before retrying; it will otherwise cause a collision. A disconnect or process termination can still leave a partial destination. This fallback costs a full file copy, even for a rename within one share.

Linux kernel CIFS/SMB filesystems deliberately select this exclusive-create path. Current [CIFS rename code](https://github.com/torvalds/linux/blob/master/fs/smb/client/inode.c) does not pass the no-replace flag through to the server operation, and [SMB2 request construction](https://github.com/torvalds/linux/blob/master/fs/smb/client/smb2inode.c) sets `ReplaceIfExists = 1`. Other Linux filesystems retain native `RENAME_NOREPLACE`; unsupported operations can select the fallback. `EINVAL` selects it only when an exclusive rename of a scratch file in the same folder also fails with `EINVAL`, so path-specific errors are reported rather than copied around. If `statfs` fails, SLM still attempts the native rename. The CIFS safeguard does not establish why Organize previously did nothing on CachyOS/KDE.

The restored template remains unchanged:

```text
{TITLE_NAME} ({DLC_NAME})[{TITLE_ID}][v{VERSION}]
```

Restoring it removed unintended whitespace-driven renames. It did not change SMB syscall support.

## Distinguish planning from moving

On macOS the application log is `~/Library/Application Support/Switch Library Manager/slm.log`. Look for:

- `Organization plan built`: includes `planned_moves`, `conflicts`, and the rename/import settings. Zero moves means no move syscall was attempted.
- `Exclusive rename unavailable; copying to an exclusively created destination`, or `Exclusive publication unavailable; copying to an exclusively created destination`: includes paths and the original OS error.
- `Moved file:`: records each completed move.
- `Organization failed`: includes planned/completed move counts and the error, including the failing operation and paths. Execution still stops at the first failure.

For CachyOS, collect the exact library and enabled intake paths from settings, then inspect both mounts:

```sh
uname -srmo
findmnt --target '/actual/library/path' --output TARGET,SOURCE,FSTYPE,OPTIONS
findmnt --target '/actual/intake/path' --output TARGET,SOURCE,FSTYPE,OPTIONS
```

`cifs`/`smb2` and a Dolphin/KIO-FUSE path are different environments. Record the filesystem type and actual local path; an `smb://` location alone is insufficient. Capture a nonzero `planned_moves` count and the full returned error before assigning a Linux cause.

## Scratch application test

The current cross-platform procedure, including the file-ID stability probe, is in [testing.md](testing.md).

From `src`, run the backend application test with existing parent directories on the desired mounts:

```sh
env SLM_ORGANIZE_TEST_ROOT='/path/on/library/mount' \
    SLM_ORGANIZE_TEST_INTAKE_ROOT='/path/on/intake/mount' \
    TMPDIR=/private/tmp \
    go test ./backend/app -run '^TestOrganizeLibraryScratchMount$' -v -count=1
```

On Linux, use its local temporary directory, such as `TMPDIR=/tmp`. Each case creates and removes only its own `.slm-organize-test-*` directories. The test exercises an in-library rename, an enabled intake on the same mount, and an intake under the second supplied parent. Choose a second filesystem to exercise a cross-device intake move.

Each successful case asserts one distinct-path move before calling `App.OrganizeLibrary`, then checks destination bytes, source removal, modification time, refreshed paths, and a zero-move rerun. A fourth case creates a destination after preflight, executes that plan, and checks the failure log and preservation of both files. Its small synthetic `.nsp` uses filename metadata fallback; it does not exercise encrypted Switch parsing or the GUI confirmation dialog. A separate GUI test with a known-good backup must also use an isolated test library and an intentionally incorrect filename.

## Verification on this checkout

The host was macOS Darwin 24.6.0, amd64, with Go 1.27.3. `smbutil statshares -m /Volumes/luigi_data3` reported SMB 3.1.1 and `FILE_IDS_SUPPORTED`.

| Environment | Result |
| --- | --- |
| Local APFS | Scanner-to-Organize rename and intake tests passed |
| `/Volumes/luigi_data3`, macOS `smbfs`, SMB 3.1.1 | In-library rename, same-share intake, APFS-to-SMB intake, and late collision passed |
| Linux amd64 and 386 | Process tests cross-compiled; no Linux runtime or CachyOS mount tested |
| Windows amd64 | Process tests cross-compiled; no Windows runtime tested |

The SMB success cases exercised both the initial unsupported rename and the unsupported temporary-copy publication. Each planned one move and then zero on rerun. The late collision returned the named rename operation with `file exists`, logged zero completed moves, and preserved both files. Scratch directories were removed. No real encrypted backups, proprietary keys, GUI interaction, or live-library organization were used for these experiments.

Full `go test ./...` and `go test -race ./...` passed, as did `go vet ./...`, `gofmt -l .`, and `git diff --check`. Tests used `TMPDIR=/private/tmp` to avoid an existing `/var` versus `/private/var` path expectation. Full-suite key-discovery tests otherwise discover the host's `.switch/prod.keys` and fail their missing-key assumptions, so those runs denied reads of the host's `.switch` directory without changing it:

```sh
sandbox-exec -p '(version 1)(allow default)(deny file-read* (subpath "/Users/todd/.switch"))' \
    env TMPDIR=/private/tmp go test ./...
```

These workarounds are no longer needed: test packages now redirect `HOME`, `USERPROFILE` and `XDG_CONFIG_HOME` to a temporary directory (`src/internal/testhome`), and the path expectation resolves symlinks.

The same isolation was used for the race and aggregate core-coverage runs. Combined core statement coverage was **92.2%**, above the required 90%. Focused regressions cover unsupported rename/publication, late collisions, existing files/symlinks/directories, copy/flush/close/metadata failures, changing sources, and replacement of the destination while its original handle remains open. Cross-compilation does not establish Linux runtime behavior; the CachyOS mount type, actual move count, and original error remain unverified.
