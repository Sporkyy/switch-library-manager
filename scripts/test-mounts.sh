#!/usr/bin/env bash
# Runs the whole testing.md verification on macOS or Linux and prints one
# pass/fail line per check:
#   - gofmt, go vet, go test and go test -race
#   - per SMB share: the file-ID stability probe and the scratch Organize test,
#     with the intake folder on local disk
#   - across servers: the scratch Organize test with the library on one server
#     and the intake on another, in both directions
#   - per SMB share: a check that no .slm-* scratch entries were left behind
#
# Usage: scripts/test-mounts.sh [--no-race] [--list] [SHARE_PATH ...]
#   --no-race  skip go test -race (faster; it needs cgo)
#   --list     print the shares and servers that would be tested, then exit
# Without share paths, every mounted SMB share is tested (cifs/smb3 on Linux,
# smbfs on macOS). The tests only create, move and remove synthetic files
# inside uniquely named scratch folders; nothing else on a share is touched.
# Full output for every check is kept in a log folder printed at the end.

set -o pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$script_dir/.." && pwd)"

race=true
list_only=false
requested=()
for argument in "$@"; do
	case "$argument" in
	--no-race) race=false ;;
	--list) list_only=true ;;
	-h | --help)
		sed -n '2,18p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	-*)
		echo "unknown option: $argument" >&2
		exit 2
		;;
	/) requested+=("/") ;;
	*) requested+=("${argument%/}") ;;
	esac
done

for tool in go gofmt; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "$tool is not on PATH" >&2
		exit 2
	fi
done

temp_root="${TMPDIR:-/tmp}"
temp_root="${temp_root%/}"
log_dir="$(mktemp -d "$temp_root/slm-test-mounts.XXXXXX")"
intake_dir="$(mktemp -d "$temp_root/slm-local-intake.XXXXXX")"
trap 'rm -rf "$intake_dir"' EXIT

# Mounted SMB shares as parallel arrays of mount point and server host.
share_paths=()
share_hosts=()

add_share() {
	local target="$1" source="$2" host
	host="${source#//}"
	host="${host#*@}"
	host="${host%%/*}"
	# macOS lists one server as both NAME._smb._tcp.local and NAME.local.
	host="${host%._smb._tcp.local}"
	host="${host%.local}"
	share_paths+=("$target")
	share_hosts+=("$(printf '%s' "$host" | tr '[:upper:]' '[:lower:]')")
}

case "$(uname -s)" in
Linux)
	if command -v findmnt >/dev/null 2>&1; then
		while read -r target source; do
			# findmnt -r escapes spaces and other special characters as \xNN.
			add_share "$(printf '%b' "$target")" "$(printf '%b' "$source")"
		done < <(findmnt -rn -t cifs,smb3 -o TARGET,SOURCE)
	fi
	;;
Darwin)
	# //user@SERVER._smb._tcp.local/share on /Volumes/share (smbfs, ...)
	while IFS= read -r line; do
		source="${line%% on *}"
		target="${line#* on }"
		target="${target% (smbfs*}"
		add_share "$target" "$source"
	done < <(mount -t smbfs)
	;;
*)
	echo "unsupported OS $(uname -s); follow docs/testing.md by hand" >&2
	exit 2
	;;
esac

if ((${#requested[@]} > 0)); then
	selected_paths=()
	selected_hosts=()
	for path in "${requested[@]}"; do
		host="unknown:$path"
		for i in "${!share_paths[@]}"; do
			if [[ "${share_paths[$i]}" == "$path" ]]; then
				host="${share_hosts[$i]}"
			fi
		done
		if [[ ! -d "$path" ]]; then
			echo "not a directory: $path" >&2
			exit 2
		fi
		selected_paths+=("$path")
		selected_hosts+=("$host")
	done
	share_paths=("${selected_paths[@]}")
	share_hosts=("${selected_hosts[@]}")
fi

results=()
failures=0
step_number=0

# run_check NAME COMMAND...: runs the command from src, keeps its output in the
# log folder and records PASS or FAIL.
run_check() {
	local name="$1" log status
	shift
	step_number=$((step_number + 1))
	log="$log_dir/$(printf '%02d' "$step_number")-$(printf '%s' "$name" | tr -c 'A-Za-z0-9._-' '_').log"
	printf '%-70s ' "$name"
	(cd "$repo_root/src" && "$@") >"$log" 2>&1
	status=$?
	local note=""
	if grep -q 'summary for' "$log"; then
		note="$(grep -o 'handle vs path after close [0-9]*/[0-9]*.*' "$log" | head -1)"
	fi
	if grep -q 'Could not lock SMB destination' "$log"; then
		note="lock refused on this mount; record its options"
	fi
	if ((status == 0)); then
		echo "PASS"
		results+=("PASS  $name${note:+  ($note)}")
	else
		echo "FAIL"
		results+=("FAIL  $name  -> $log")
		failures=$((failures + 1))
		show_failure_excerpt "$log"
	fi
}

# show_failure_excerpt LOG prints, for each failed test, the last message it
# logged (a failing test stops at its t.Fatal), or the first lines of a
# failing non-test check.
show_failure_excerpt() {
	local excerpt
	excerpt="$(awk '
		/^=== (RUN|CONT)/ { current = $3; next }
		/_test\.go:[0-9]+: / { sub(/^[ \t]+/, ""); last[current] = $0; next }
		/--- FAIL: / {
			name = $3
			if ((name in last) && !(name in shown)) { print name ": " last[name]; shown[name] = 1 }
		}
	' "$1" | head -6)"
	if [[ -z "$excerpt" ]]; then
		excerpt="$(grep -v -E '^(ok|PASS|FAIL)?[[:space:]]' "$1" | head -6)"
	fi
	printf '%s\n' "$excerpt" | cut -c1-220 | sed 's/^/      /'
}

check_gofmt() {
	local unformatted
	unformatted="$(gofmt -l .)"
	if [[ -n "$unformatted" ]]; then
		printf 'unformatted files:\n%s\n' "$unformatted"
		return 1
	fi
}

check_no_leftovers() {
	local share="$1" leftovers
	leftovers="$(find "$share" -mindepth 1 -maxdepth 1 -name '.slm-*' 2>&1)"
	if [[ -n "$leftovers" ]]; then
		printf 'scratch entries left in %s:\n%s\n' "$share" "$leftovers"
		return 1
	fi
}

organize_test() {
	SLM_ORGANIZE_TEST_ROOT="$1" SLM_ORGANIZE_TEST_INTAKE_ROOT="$2" \
		go test ./backend/app -run '^TestOrganizeLibraryScratchMount$' -v -count=1
}

probe_test() {
	SLM_FILE_ID_TEST_ROOTS="$1" \
		go test ./backend/process -run '^TestFileIdentityStabilityOnMount$' -v -count=1
}

echo "OS: $(uname -srm); Go: $(go env GOVERSION)"
echo "Shares: ${#share_paths[@]}"
for i in "${!share_paths[@]}"; do
	echo "  ${share_paths[$i]}  (server ${share_hosts[$i]})"
done
echo
if $list_only; then
	exit 0
fi

run_check "gofmt -l ." check_gofmt
run_check "go vet ./..." go vet ./...
run_check "go test ./..." go test -count=1 ./...
if $race; then
	run_check "go test -race ./..." go test -race -count=1 ./...
fi

for share in "${share_paths[@]}"; do
	run_check "file-ID probe: $share" probe_test "$share"
	run_check "organize: library $share, intake local disk" organize_test "$share" "$intake_dir"
done

# One share per distinct server, then each ordered pair of servers.
server_paths=()
server_hosts=()
for i in "${!share_paths[@]}"; do
	seen=false
	for host in "${server_hosts[@]}"; do
		[[ "$host" == "${share_hosts[$i]}" ]] && seen=true
	done
	if ! $seen && [[ "${share_hosts[$i]}" != unknown:* ]]; then
		server_paths+=("${share_paths[$i]}")
		server_hosts+=("${share_hosts[$i]}")
	fi
done
for i in "${!server_paths[@]}"; do
	for j in "${!server_paths[@]}"; do
		if ((i != j)); then
			run_check "organize: library ${server_paths[$i]}, intake ${server_paths[$j]}" \
				organize_test "${server_paths[$i]}" "${server_paths[$j]}"
		fi
	done
done

for share in "${share_paths[@]}"; do
	run_check "no scratch leftovers: $share" check_no_leftovers "$share"
done

echo
echo "Summary"
printf '  %s\n' "${results[@]}"
if ((${#share_paths[@]} == 0)); then
	echo "  NOTE  no SMB shares mounted; only the local checks ran"
elif ((${#server_paths[@]} < 2)); then
	echo "  NOTE  fewer than two servers found; cross-server checks did not run"
fi
echo "Logs: $log_dir"
if ((failures > 0)); then
	echo "$failures check(s) failed"
	exit 1
fi
echo "All checks passed"
