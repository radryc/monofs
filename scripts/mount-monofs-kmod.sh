#!/usr/bin/env bash
set -euo pipefail

usage() {
	cat <<'EOF'
Usage:
  scripts/mount-monofs-kmod.sh --mount=/path [--gateway=host:port] [--seed-paths=a/b,c/d] [--cluster-version=N] [--debug]

Mounts the native monofs kernel-module scaffold as the lower filesystem.
This script expects the `monofs` kernel module to already be built and loaded.

Options:
  --mount=PATH           Target mount point (required)
  --gateway=HOST:PORT    Router/gateway address to record in mount options
  --seed-paths=LIST      Comma-separated synthetic display paths to expose
  --cluster-version=N    Initial topology/version hint for the mount
  --debug                Enable scaffold debug logging inside the kmod
  -h, --help             Show this help text
EOF
}

mount_point=""
gateway=""
seed_paths=""
cluster_version=""
debug=0

for arg in "$@"; do
	case "$arg" in
	--mount=*)
		mount_point="${arg#*=}"
		;;
	--gateway=*)
		gateway="${arg#*=}"
		;;
	--seed-paths=*)
		seed_paths="${arg#*=}"
		;;
	--cluster-version=*)
		cluster_version="${arg#*=}"
		;;
	--debug)
		debug=1
		;;
	-h|--help)
		usage
		exit 0
		;;
	*)
		echo "Unknown argument: $arg" >&2
		usage >&2
		exit 1
		;;
	esac
done

if [[ -z "$mount_point" ]]; then
	echo "Error: --mount is required." >&2
	usage >&2
	exit 1
fi

mkdir -p "$mount_point"

mount_opts=("overlay_writes")

if [[ -n "$gateway" ]]; then
	mount_opts+=("gateway=$gateway")
fi

if [[ -n "$seed_paths" ]]; then
	mount_opts+=("seed_paths=$seed_paths")
fi

if [[ -n "$cluster_version" ]]; then
	mount_opts+=("cluster_version=$cluster_version")
fi

if [[ "$debug" -eq 1 ]]; then
	mount_opts+=("debug")
fi

opt_string="$(IFS=,; echo "${mount_opts[*]}")"

echo "Mounting monofs kmod scaffold at $mount_point"
mount -t monofs -o "$opt_string" monofs "$mount_point"
