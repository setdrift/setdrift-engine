#!/bin/sh
# Runs setdrift helm-postrender under whichever name SetDrift was installed as: setdrift (a release binary), or kubectl-setdrift (a kubectl plugin e.g. krew). Arguments are Helm's --post-renderer-args.
for cmd in setdrift kubectl-setdrift; do
  if command -v "$cmd" >/dev/null 2>&1; then
    exec "$cmd" helm-postrender "$@"
  fi
done
echo "setdrift-chart-repo: neither setdrift nor kubectl-setdrift is on PATH; install SetDrift first" >&2
exit 127

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
