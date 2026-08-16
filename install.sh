#!/bin/sh

set -eu

repository="levmv/skot"
latest_release_api="https://api.github.com/repos/${repository}/releases/latest"
release_base="https://github.com/${repository}/releases/download"

die() {
	printf 'sk installer: %s\n' "$*" >&2
	exit 1
}

default_path_profile() {
	shell_name=${SHELL:-}
	shell_name=${shell_name##*/}
	case "$shell_name" in
	zsh) printf '%s\n' "${ZDOTDIR:-$HOME}/.zshrc" ;;
	bash)
		if [ "$os" = "darwin" ]; then
			printf '%s\n' "${HOME}/.bash_profile"
		else
			printf '%s\n' "${HOME}/.bashrc"
		fi
		;;
	sh | dash | ksh) printf '%s\n' "${HOME}/.profile" ;;
	*) return 1 ;;
	esac
}

print_path_activation() {
	printf 'Open a new terminal, or run:\n  export PATH="$HOME/.local/bin:$PATH"\n'
}

offer_default_path_update() {
	profile=$(default_path_profile) || return 1

	path_line='export PATH="$HOME/.local/bin:$PATH"'
	if [ -f "$profile" ] && grep -Fqx "$path_line" "$profile"; then
		printf '\n%s is configured in %s but is not active in this shell.\n' "$install_dir" "$profile"
		print_path_activation
		return 0
	fi

	if [ -n "${CI:-}" ]; then
		return 1
	fi
	if ! ( : </dev/tty ) 2>/dev/null; then
		return 1
	fi
	printf '\n%s is not on PATH.\nAdd it to %s? [Y/n] ' "$install_dir" "$profile" >/dev/tty
	answer=
	if ! IFS= read -r answer </dev/tty; then
		return 1
	fi
	case "$answer" in
	"" | y | Y | yes | Yes | YES) ;;
	*) return 1 ;;
	esac

	{
		if [ -s "$profile" ]; then
			printf '\n'
		fi
		printf '# Skot\n%s\n' "$path_line"
	} >>"$profile" || return 1
	printf 'Added %s to PATH in %s.\n' "$install_dir" "$profile"
	print_path_activation
}

print_manual_path_setup() {
	printf '\n%s is not on PATH.\n' "$install_dir"
	if [ -n "${HOME:-}" ] && [ "$install_dir" = "${HOME}/.local/bin" ]; then
		shell_name=${SHELL:-}
		shell_name=${shell_name##*/}
		case "$shell_name" in
		fish) printf 'Run:\n  fish_add_path "$HOME/.local/bin"\n' ;;
		*)
			if profile=$(default_path_profile); then
				printf 'Add this line to %s:\n' "$profile"
			else
				printf 'Add this line to your shell profile:\n'
			fi
			printf '  export PATH="$HOME/.local/bin:$PATH"\n'
			;;
		esac
	else
		printf 'Add this directory to your shell PATH.\n'
	fi
	printf 'Until then, run Skot with:\n  "%s/sk"\n' "$install_dir"
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
		# The latest endpoint reports one release and never a draft or a
		# prerelease.
		release=$(curl -fsSL \
			-H "Accept: application/vnd.github+json" \
			-H "X-GitHub-Api-Version: 2022-11-28" \
			"$latest_release_api") || die "could not read the latest GitHub release"
		tag=$(printf '%s\n' "$release" |
			grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' |
			sed 's/.*"\([^"]*\)"$/\1/' |
			head -n 1)
		[ -n "$tag" ] || die "no released version was found"
		;;
	v*) tag="$requested_version" ;;
	*) tag="v${requested_version}" ;;
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

printf 'Installed Skot %s to %s/sk\n' "$tag" "$install_dir"
case ":${PATH:-}:" in
	*":${install_dir}:"*) ;;
	*)
		if [ -n "${HOME:-}" ] && [ "$install_dir" = "${HOME}/.local/bin" ] && offer_default_path_update; then
			:
		else
			print_manual_path_setup
		fi
		;;
esac
