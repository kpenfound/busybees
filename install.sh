#!/bin/sh
# Install a released `bees` binary on macOS or Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/kpenfound/busybees/main/install.sh | sh
#
# Environment:
#   BEES_VERSION      release to install, e.g. v0.2.0 (default: the latest one)
#   BEES_INSTALL_DIR  directory to install into      (default: /usr/local/bin)
#
# Run from a file rather than a pipe, the version can also be the first
# argument, which wins over BEES_VERSION:
#
#   sh install.sh v0.2.0
#
# The asset names parsed here (bees_<version>_<os>_<arch>.tar.gz and
# checksums.txt) are the release workflow's stable interface; they are
# documented in docs/releasing.md.
#
# POSIX sh. Needs curl or wget, tar, uname and a SHA-256 tool.

set -eu

REPO="kpenfound/busybees"
API_URL="https://api.github.com/repos/${REPO}/releases/latest"
DOWNLOAD_URL="https://github.com/${REPO}/releases/download"
BIN="bees"
SOURCE_HINT="build from source instead: go install github.com/${REPO}/cmd/bees@latest"

install_dir="${BEES_INSTALL_DIR:-/usr/local/bin}"

# State the exit trap cleans up. tmp_dir holds the download until it has been
# verified; staged is the copy inside install_dir between cp and the final mv,
# so a failure halfway through leaves no half-installed binary behind.
tmp_dir=""
staged=""
sudo_cmd=""
fetch_tool=""

info() { printf '%s\n' "$*"; }

die() {
	printf 'install.sh: %s\n' "$*" >&2
	exit 1
}

have() { command -v "$1" >/dev/null 2>&1; }

usage() {
	cat <<EOF
Usage: install.sh [version]

Installs a released bees binary. With no argument the latest release is
installed; BEES_VERSION selects one instead, and BEES_INSTALL_DIR (default
/usr/local/bin) says where to put it.
EOF
}

cleanup() {
	if [ -n "$staged" ]; then
		as_installer rm -f "$staged" >/dev/null 2>&1 || :
	fi
	if [ -n "$tmp_dir" ]; then
		rm -rf "$tmp_dir"
	fi
}

# ---- platform --------------------------------------------------------------

# os_name maps `uname -s` to the GOOS the release assets are named for.
os_name() {
	case "$1" in
	Darwin) echo darwin ;;
	Linux) echo linux ;;
	*) die "unsupported operating system: $1.
Released binaries exist for macOS (Darwin) and Linux only; ${SOURCE_HINT}" ;;
	esac
}

# arch_name maps `uname -m` to the GOARCH the release assets are named for.
arch_name() {
	case "$1" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) die "unsupported architecture: $1.
Released binaries exist for amd64 (x86_64) and arm64 (aarch64) only; ${SOURCE_HINT}" ;;
	esac
}

# asset_name is the tarball a release holds for one platform.
asset_name() { printf 'bees_%s_%s_%s.tar.gz\n' "$1" "$2" "$3"; }

# normalize_version accepts 0.2.0 for the tag v0.2.0: every release tag carries
# the leading v (docs/releasing.md), and dropping it is the obvious slip.
normalize_version() {
	case "$1" in
	"" | v*) printf '%s\n' "$1" ;;
	[0-9]*) printf 'v%s\n' "$1" ;;
	*) printf '%s\n' "$1" ;;
	esac
}

# ---- downloading -----------------------------------------------------------

require_fetch_tool() {
	if have curl; then
		fetch_tool=curl
	elif have wget; then
		fetch_tool=wget
	else
		die "neither curl nor wget was found on PATH.
Install one of them, or download the release by hand from
https://github.com/${REPO}/releases"
	fi
}

# fetch_to_file downloads a URL, returning non-zero on any HTTP error so the
# caller can say what the missing thing means.
#
# Both are silent, stderr included: a 404 is an expected answer at every call
# site here (no such release, no such asset), each one replaces it with a
# message naming what was being looked for, and neither tool can be made to
# report the other failures consistently anyway -- curl -S prints its own line
# above ours, GNU wget -q swallows everything, and busybox wget -q prints the
# 404 regardless. The messages name the URL so it can be retried by hand.
fetch_to_file() {
	case "$fetch_tool" in
	curl) curl -fsL -o "$2" "$1" 2>/dev/null ;;
	wget) wget -q -O "$2" "$1" 2>/dev/null ;;
	esac
}

fetch_to_stdout() {
	case "$fetch_tool" in
	curl) curl -fsL "$1" 2>/dev/null ;;
	wget) wget -q -O - "$1" 2>/dev/null ;;
	esac
}

# latest_version reads tag_name out of the GitHub releases API. The response is
# one release object, so the first tag_name in it is the answer; parsing it with
# shell expansions keeps the script free of jq, sed and python.
latest_version() {
	json=$(fetch_to_stdout "$API_URL") || die "could not read the latest release of ${REPO}.
The repository may have no published releases yet, or the network may be down.
Install a particular version with BEES_VERSION=v0.2.0, see what exists at
https://github.com/${REPO}/releases, or ${SOURCE_HINT}"
	case "$json" in
	*'"tag_name"'*) ;;
	*) die "${API_URL} did not answer with a release; ${SOURCE_HINT}" ;;
	esac
	tag=${json#*\"tag_name\"}
	tag=${tag#*:}
	tag=${tag#*\"}
	tag=${tag%%\"*}
	[ -n "$tag" ] || die "could not read a release tag from ${API_URL}.
Pass a version instead: BEES_VERSION=v0.2.0"
	printf '%s\n' "$tag"
}

# ---- checksums -------------------------------------------------------------

sha256_of() {
	if have sha256sum; then
		line=$(sha256sum "$1")
	elif have shasum; then
		line=$(shasum -a 256 "$1")
	else
		die "no SHA-256 tool found: install sha256sum (coreutils) or shasum"
	fi
	printf '%s\n' "${line%% *}"
}

# expected_checksum finds one asset's sum in a sha256sum-format checksums file.
# Returns non-zero when the release has no such asset.
expected_checksum() {
	while read -r sum name; do
		case "$name" in
		"$2" | "*$2")
			printf '%s\n' "$sum"
			return 0
			;;
		esac
	done <"$1"
	return 1
}

# ---- installing ------------------------------------------------------------

make_tmp_dir() {
	if have mktemp; then
		mktemp -d 2>/dev/null || mktemp -d -t bees-install
	else
		dir="${TMPDIR:-/tmp}/bees-install.$$"
		mkdir "$dir"
		printf '%s\n' "$dir"
	fi
}

# existing_ancestor is the closest directory that exists at or above a path, so
# writability is asked of something real when install_dir has to be created.
existing_ancestor() {
	dir=$1
	while [ ! -d "$dir" ]; do
		case "$dir" in
		*/?*) dir=${dir%/*}; [ -n "$dir" ] || dir=/ ;;
		*) dir=.; break ;;
		esac
	done
	printf '%s\n' "$dir"
}

# prepare_install_dir decides whether sudo is needed and creates install_dir.
prepare_install_dir() {
	anchor=$(existing_ancestor "$install_dir")
	if [ -w "$anchor" ]; then
		sudo_cmd=""
	elif have sudo; then
		sudo_cmd=sudo
		info "${install_dir} is not writable; using sudo (you may be asked for your password)"
	else
		die "${install_dir} is not writable and sudo was not found.
Install somewhere you can write instead, e.g.
  BEES_INSTALL_DIR=\$HOME/.local/bin
and make sure that directory is on your PATH."
	fi
	[ -d "$install_dir" ] || as_installer mkdir -p "$install_dir"
}

as_installer() {
	if [ -n "$sudo_cmd" ]; then
		sudo "$@"
	else
		"$@"
	fi
}

# install_binary copies the verified binary into install_dir under a temporary
# name and renames it into place, so an interrupted install never replaces a
# working bees with half a file.
install_binary() {
	staged="${install_dir}/.${BIN}.install.$$"
	as_installer cp "$1" "$staged" || die "could not write to ${install_dir}.
Install somewhere you can write instead: BEES_INSTALL_DIR=\$HOME/.local/bin"
	as_installer chmod 0755 "$staged"
	as_installer mv "$staged" "${install_dir}/${BIN}"
	staged=""
}

on_path() {
	case ":${PATH}:" in
	*":$1:"*) return 0 ;;
	*) return 1 ;;
	esac
}

# ---- main ------------------------------------------------------------------

main() {
	version=""
	case "${1:-}" in
	-h | --help)
		usage
		exit 0
		;;
	-*) die "unknown option: $1
$(usage)" ;;
	"") version="${BEES_VERSION:-}" ;;
	*) version="$1" ;;
	esac
	version=$(normalize_version "$version")

	require_fetch_tool
	os=$(os_name "$(uname -s)")
	arch=$(arch_name "$(uname -m)")

	if [ -z "$version" ]; then
		info "Looking up the latest release of ${REPO}..."
		version=$(latest_version)
	fi
	asset=$(asset_name "$version" "$os" "$arch")

	trap cleanup EXIT INT TERM
	tmp_dir=$(make_tmp_dir)

	info "Installing bees ${version} (${os}/${arch}) into ${install_dir}"

	# checksums.txt is fetched first: every release has one, so failing to get
	# it means the release itself is missing, while an asset absent from it
	# means this platform is not in that release.
	fetch_to_file "${DOWNLOAD_URL}/${version}/checksums.txt" "${tmp_dir}/checksums.txt" ||
		die "no release ${version} of ${REPO} (could not download its checksums.txt).
Pick one from https://github.com/${REPO}/releases"
	expected=$(expected_checksum "${tmp_dir}/checksums.txt" "$asset") ||
		die "release ${version} has no asset for ${os}/${arch} (${asset}).
See https://github.com/${REPO}/releases/tag/${version} for what it does have; ${SOURCE_HINT}"

	fetch_to_file "${DOWNLOAD_URL}/${version}/${asset}" "${tmp_dir}/${asset}" ||
		die "could not download ${asset} from ${DOWNLOAD_URL}/${version}/${asset}.
Check your network and try again; fetching that URL by hand will say why."

	actual=$(sha256_of "${tmp_dir}/${asset}")
	if [ "$actual" != "$expected" ]; then
		die "checksum mismatch for ${asset}: the download is corrupt or has been tampered with.
  expected ${expected}
  got      ${actual}
Nothing was installed. Try again; if it keeps failing, report it at
https://github.com/${REPO}/issues"
	fi

	tar -xzf "${tmp_dir}/${asset}" -C "$tmp_dir" ||
		die "could not unpack ${asset}"
	[ -f "${tmp_dir}/${BIN}" ] ||
		die "${asset} does not contain a ${BIN} binary"

	prepare_install_dir
	install_binary "${tmp_dir}/${BIN}"

	info "Installed ${install_dir}/${BIN} (${version})"
	if on_path "$install_dir"; then
		info "Run 'bees version' to check it, then 'bees init' in a project."
	else
		info "${install_dir} is not on your PATH; add it, or run ${install_dir}/${BIN} directly."
	fi
}

main "$@"
