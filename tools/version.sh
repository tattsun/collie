#!/bin/sh
# Prints the version stamped into the binary: $COLLIE_VERSION (the release
# workflow sets it to the tag), else `git describe` (0.3.0 on a release
# tag, 0.3.0-4-gabc1234 after it, -dirty with local changes), else
# dev-<commit>. Always without the leading "v".
if [ -n "${COLLIE_VERSION:-}" ]; then
  v=$COLLIE_VERSION
elif ! v=$(git describe --tags --match 'v[0-9]*' --dirty 2>/dev/null); then
  v="dev-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
fi
echo "${v#v}"
