#!/usr/bin/env bash
# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0

# Fails if a source or build file lacks the Apache-2.0 license header in its
# first lines. Prints the files that do.
#
# Why: every file in this repo is Apache-2.0, and the header carries that with
# a file that is copied elsewhere. It also lets scanners identify the license
# per file. Checking it keeps the open/closed boundary visible: a file without
# the header (e.g. copied in from the private repo) fails CI.
#
# Why this way: git lists the files, tracked or not, so ignored ones (bin/,
# out/) are skipped without a separate exclude list. Generated
# code, tool-owned files and test data are excluded below.
set -euo pipefail

cd "$(dirname "$0")/.."
missing=0
while IFS= read -r f; do
  case "$f" in
    *.go | *.proto | *.sh | *.yaml | *.yml | Makefile) ;;
    *) continue ;;
  esac
  case "$f" in
    testdata/* | *.pb.go) continue ;;
  esac
  if ! head -n 5 "$f" | grep -q 'SPDX-License-Identifier: Apache-2.0'; then
    echo "$f"
    missing=1
  fi
done < <(git ls-files --cached --others --exclude-standard)

if [ "$missing" -ne 0 ]; then
  echo "error: the files above lack the license header. Add, in the file's comment syntax:" >&2
  echo "  Copyright The SetDrift Authors" >&2
  echo "  SPDX-License-Identifier: Apache-2.0" >&2
  exit 1
fi
