#!/bin/sh

set -eu

repository="levmv/skot"
releases_api="https://api.github.com/repos/${repository}/releases?per_page=100"
release_base="https://github.com/${repository}/releases/download"

die() {
	printf 'sk installer: %s\n' "$*" >&2
	exit 1
}

command -v curl >/dev/null 2>&1 || die "curl is required"

case "$(uname -s)" in
	Linux) os="linux" ;;
	Darwin) os="darwin" ;;
	*) die "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
	x86_64 | amd64) arch="amd64" ;;
	aarch64 | arm64) arch="arm64" ;;
	*) die "unsupported architecture: $(uname -m)" ;;
esac

requested_version=${SK_VERSION:-latest}
case "$requested_version" in
	"" | latest)
		releases=$(curl -fsSL \
			-H "Accept: application/vnd.github+json" \
			-H "X-GitHub-Api-Version: 2022-11-28" \
			"$releases_api") || die "could not list GitHub releases"
		tag=$(printf '%s\n' "$releases" |
			grep -o '"tag_name"[[:space:]]*:[[:space:]]*"skot-v[^"]*"' |
			sed 's/.*"\(skot-v[^"]*\)"$/\1/' |
			head -n 1)
		[ -n "$tag" ] || die "no skot-v* release was found"
		;;
	skot-v*) tag="$requested_version" ;;
	v*) tag="skot-${requested_version}" ;;
	*) tag="skot-v${requested_version}" ;;
esac

asset="sk-${os}-${arch}"
asset_url="${release_base}/${tag}/${asset}"
checksums_url="${release_base}/${tag}/checksums.txt"

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/sk-install.XXXXXX") || die "could not create a temporary directory"
staged=""
cleanup() {
	if [ -n "$staged" ]; then
		rm -f "$staged"
	fi
	rm -rf "$tmp_dir"
}
trap cleanup 0 HUP INT TERM

curl -fL --retry 3 --connect-timeout 10 -o "${tmp_dir}/${asset}" "$asset_url" ||
	die "could not download ${tag}/${asset}"
curl -fL --retry 3 --connect-timeout 10 -o "${tmp_dir}/checksums.txt" "$checksums_url" ||
	die "could not download ${tag}/checksums.txt"

expected=$(awk -v asset="$asset" '$2 == asset { print $1; exit }' "${tmp_dir}/checksums.txt")
[ -n "$expected" ] || die "${asset} is missing from checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "${tmp_dir}/${asset}")
	actual=${actual%% *}
elif command -v shasum >/dev/null 2>&1; then
	actual=$(shasum -a 256 "${tmp_dir}/${asset}")
	actual=${actual%% *}
else
	die "sha256sum or shasum is required"
fi
[ "$actual" = "$expected" ] || die "checksum verification failed for ${asset}"

if [ -n "${SK_INSTALL_DIR:-}" ]; then
	install_dir=$SK_INSTALL_DIR
elif [ -n "${HOME:-}" ]; then
	install_dir="${HOME}/.local/bin"
else
	die "HOME is not set; provide SK_INSTALL_DIR"
fi

mkdir -p "$install_dir" || die "could not create ${install_dir}"
staged="${install_dir}/.sk.install.$$"
install -m 0755 "${tmp_dir}/${asset}" "$staged" || die "could not write to ${install_dir}"
mv -f "$staged" "${install_dir}/sk" || die "could not install sk to ${install_dir}"
staged=""

printf 'Installed Skot %s to %s/sk\n' "${tag#skot-}" "$install_dir"
case ":${PATH:-}:" in
	*":${install_dir}:"*) ;;
	*) printf 'Add %s to PATH to run sk.\n' "$install_dir" ;;
esac
