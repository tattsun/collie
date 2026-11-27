#!/bin/sh
# Prints the -ldflags for building collie: the version (tools/version.sh)
# and, when $COLLIE_BASE_IMAGE is set (the release workflow sets it to the
# agent base image it published, pinned by digest), that base image.
set -e
dir=$(dirname "$0")
flags="-s -w -X github.com/tattsun/collie/internal/version.Version=$("$dir/version.sh")"
if [ -n "${COLLIE_BASE_IMAGE:-}" ]; then
  case "$COLLIE_BASE_IMAGE" in
    *@sha256:*) ;;
    *) echo "COLLIE_BASE_IMAGE must be pinned by digest (…@sha256:…): $COLLIE_BASE_IMAGE" >&2; exit 1 ;;
  esac
  flags="$flags -X github.com/tattsun/collie/internal/sandbox.BaseImage=$COLLIE_BASE_IMAGE"
fi
echo "$flags"
