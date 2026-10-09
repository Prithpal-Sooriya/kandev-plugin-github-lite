#!/bin/sh
set -eu

fail() {
	printf 'package verification failed: %s\n' "$1" >&2
	exit 1
}

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
	fail 'usage: verify-package.sh PACKAGE_DIR full | host PLATFORM'
fi

package_dir=$1
mode=$2
host_platform=${3-}
[ -d "$package_dir" ] || fail "package directory not found: $package_dir"

for required in manifest.yaml ui/bundle.js checksums.txt; do
	[ -f "$package_dir/$required" ] || fail "missing required file: $required"
done

manifest_executables=$(awk '
	$0 == "runtime:" { in_runtime = 1; next }
	in_runtime && $0 == "  executables:" { in_executables = 1; next }
	in_executables && $0 !~ /^    / { exit }
	in_executables && /^    [[:alnum:]_-]+: "[^\"]+"$/ {
		platform = $1
		sub(/:$/, "", platform)
		path = $2
		gsub(/"/, "", path)
		print platform " " path
	}
' "$package_dir/manifest.yaml" | LC_ALL=C sort)

# Categories are validated against kandev's install-time enum (kandev apps/
# backend internal/plugins/manifest/validate.go: validCategories). The 0.1.0
# package shipped category "integrations", which the host rejected with 400
# `unknown category "integrations"` — plugin-pack only parses the manifest,
# it does not run full Validate, so this check closes that gap for the one
# rule that bit. When present, categories must use the inline
# `categories: ["a", "b"]` spelling this parser understands.
if grep -qE '^categories:' "$package_dir/manifest.yaml"; then
	manifest_categories=$(sed -nE 's/^categories: *\[(.*)\]$/\1/p' "$package_dir/manifest.yaml")
	[ -n "$manifest_categories" ] || fail 'categories: must use the inline ["a", "b"] spelling'
	for category in $(printf '%s\n' "$manifest_categories" | tr ',' ' ' | tr -d '"'); do
		case "$category" in
			connector | automation | tools | analytics | canvas) ;;
			*) fail "unknown manifest category: $category" ;;
		esac
	done
fi
expected_executables=$(printf '%s\n' \
	'darwin-amd64 server/plugin-darwin-amd64' \
	'darwin-arm64 server/plugin-darwin-arm64' \
	'linux-amd64 server/plugin-linux-amd64' \
	'linux-arm64 server/plugin-linux-arm64' \
	'windows-amd64 server/plugin-windows-amd64.exe' | LC_ALL=C sort)
[ "$manifest_executables" = "$expected_executables" ] || fail 'manifest runtime.executables does not match the supported platform set'

case "$mode" in
	full)
		executable_paths=$(printf '%s\n' "$manifest_executables" | awk '{ print $2 }')
		;;
	host)
		[ -n "$host_platform" ] || fail 'host mode requires a platform name'
		executable_paths=$(printf '%s\n' "$manifest_executables" | awk -v platform="$host_platform" '$1 == platform { print $2 }')
		[ -n "$executable_paths" ] || fail "host platform is not declared: $host_platform"
		;;
	*)
		fail "unknown verification mode: $mode"
		;;
esac

for executable in $executable_paths; do
	[ -f "$package_dir/$executable" ] || fail "missing declared executable: $executable"
done

forbidden_path=$(find "$package_dir" \
	\( -name recipes -o -name scripts -o -name node_modules \
		-o -name package.json -o -name package-lock.json \
		-o -name tsconfig.recipes.json -o -name '*.test.js' \) \
	-print -quit)
[ -z "$forbidden_path" ] || fail "forbidden recipe or development path: ${forbidden_path#"$package_dir"/}"

expected_files=$(printf '%s\n' manifest.yaml ui/bundle.js checksums.txt $executable_paths | LC_ALL=C sort)
actual_files=$(cd "$package_dir" && find . -type f -print | sed 's#^\./##' | LC_ALL=C sort)
[ "$actual_files" = "$expected_files" ] || {
	printf 'unexpected package file inventory\nexpected:\n%s\nfound:\n%s\n' "$expected_files" "$actual_files" >&2
	exit 1
}

checksum_entries=$(awk '
	NF != 2 { invalid = 1; next }
	{ print $2 }
	END { if (NR == 0 || invalid) exit 1 }
' "$package_dir/checksums.txt") || fail 'checksums.txt has invalid lines'
checksum_files=$(printf '%s\n' "$checksum_entries" | LC_ALL=C sort)
expected_checksum_files=$(printf '%s\n' manifest.yaml ui/bundle.js $executable_paths | LC_ALL=C sort)
[ "$checksum_files" = "$expected_checksum_files" ] || fail 'checksums.txt does not list every package file exactly once'

if command -v sha256sum >/dev/null 2>&1; then
	(cd "$package_dir" && sha256sum -c checksums.txt) || fail 'checksum verification failed'
elif command -v shasum >/dev/null 2>&1; then
	(cd "$package_dir" && shasum -a 256 -c checksums.txt) || fail 'checksum verification failed'
else
	fail 'sha256sum or shasum is required'
fi
