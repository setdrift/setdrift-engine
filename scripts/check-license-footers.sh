#!/usr/bin/env bash
# this checks that every source and build file ends with the apache-2.0 license footer, and prints the ones that dont
# every file in this repo is apache-2.0, the footer carries that along when a file gets copied somewhere else
# license scanners read the SPDX line wherever it is in the file, so the footer still identifies the license per file
# it also keeps the open/closed boundary visible, a file copied in from the private repo without the footer fails ci
# git lists the files, tracked or not, so ignored stuff like bin/ and out/ gets skipped without a separate exclude list
set -euo pipefail

# running from the repo root no matter where the script was called from
cd "$(dirname "$0")/.."
missing=0
while IFS= read -r f; do
  # checking if its a kind of file that carries the footer, everything else is skipped
  case "$f" in
    *.go | *.proto | *.sh | *.yaml | *.yml | *.ps1 | *.cmd | Makefile) ;;
    *) continue ;;
  esac
  # checking if its generated code or test data, those are left alone
  case "$f" in
    testdata/* | */testdata/* | *.pb.go) continue ;;
  esac
  # checking the last few lines for the SPDX line, if its not there the file gets listed
  if ! tail -n 5 "$f" | grep -q 'SPDX-License-Identifier: Apache-2.0'; then
    echo "$f"
    missing=1
  fi
done < <(git ls-files --cached --others --exclude-standard)

# checking if any file was missing it, if so print what to add and fail
if [ "$missing" -ne 0 ]; then
  echo "error: the files above lack the license footer, add these as the last lines in the file's comment syntax" >&2
  echo "  Copyright The SetDrift Authors" >&2
  echo "  SPDX-License-Identifier: Apache-2.0" >&2
  exit 1
fi

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
