#!/usr/bin/env bash
# The secret scan: credentials, personal information and cloud identifiers must
# never reach this public repository. It runs three checks.
#
#   1. gitleaks, with the rules in .gitleaks.toml: credentials (gitleaks' own
#      rules), email addresses, AWS account IDs, Azure subscription and tenant
#      IDs, GCP billing accounts and projects, public IP addresses.
#   2. A denylist of personal names and addresses, kept outside the repository
#      since listing them here would publish them: the PII_DENYLIST variable
#      (one entry a line, the PII_DENYLIST secret in CI), else the file
#      ~/.config/setdrift/pii-denylist. A match names the entry by its number,
#      never by its text, as CI logs on a public repository are public.
#   3. Every commit's author and committer address must be a GitHub no-reply
#      address.
#
#   secret-scan.sh staged    what is staged, for the pre-commit hook
#   secret-scan.sh range R   the commits in a git range like origin/main..HEAD, for the pre-push hook
#   secret-scan.sh all       the whole history, every commit on every branch, for CI and make scan
#
# gitleaks comes from GITLEAKS, else the PATH (brew install gitleaks).
set -euo pipefail

cd "$(dirname "$0")/.."
mode=${1:-all}
gitleaks=${GITLEAKS:-gitleaks}
command -v "$gitleaks" >/dev/null || { echo "secret scan: gitleaks is not installed (brew install gitleaks)" >&2; exit 1; }
failed=0
fail() { printf 'secret scan: %s\n' "$*" >&2; failed=1; }

# 1. gitleaks; --redact keeps any secret it finds out of the log
case $mode in
staged) "$gitleaks" git --pre-commit --staged --config .gitleaks.toml --redact --no-banner . || fail "gitleaks found the leaks above in what is staged" ;;
range) "$gitleaks" git --log-opts="${2:?give a range like origin/main..HEAD}" --config .gitleaks.toml --redact --no-banner . || fail "gitleaks found the leaks above in $2" ;;
all) "$gitleaks" git --config .gitleaks.toml --redact --no-banner . || fail "gitleaks found the leaks above in the history" ;;
*) sed -n '2,20p' "$0" >&2; exit 2 ;;
esac

# what checks 2 and 3 read: the added lines, and the commits
case $mode in
staged) added() { git diff --cached --no-color --unified=0 | grep '^+' | grep -v '^+++' || true; }; commits="" ;;
range) added() { git log -p --no-color --unified=0 "$2" | grep '^+' | grep -v '^+++' || true; }; commits=$2 ;;
all) added() { git log -p --all --no-color --unified=0 | grep '^+' | grep -v '^+++' || true; }; commits="--all" ;;
esac

# 2. the denylist, matched without regard to case
list=${PII_DENYLIST:-}
file=${XDG_CONFIG_HOME:-$HOME/.config}/setdrift/pii-denylist
if [[ -z $list && -f $file ]]; then
  list=$(cat "$file")
fi
if [[ -n $list ]]; then
  text=$(added)
  n=0
  while IFS= read -r entry; do
    entry=${entry%$'\r'}
    [[ -z $entry || $entry == \#* ]] && continue
    n=$((n + 1))
    if grep -qiF -- "$entry" <<<"$text"; then
      fail "denylist entry $n is in what would be committed or pushed"
    fi
  done <<<"$list"
elif [[ ${CI:-} == true ]]; then
  fail "the PII_DENYLIST secret is not set, so names can't be checked"
else
  echo "secret scan: no denylist ($file), names are not checked" >&2
fi

# 3. commit addresses: GitHub no-reply only, so no personal address is in the history
if [[ -n $commits ]]; then
  bad=$(git log --format='%ae%n%ce' $commits | sort -u | grep -vE '@users\.noreply\.github\.com$|^noreply@github\.com$' || true)
  if [[ -n $bad ]]; then
    fail "$(grep -c . <<<"$bad") commit address(es) are not GitHub no-reply addresses; set git config user.email to yours, ID+username@users.noreply.github.com"
  fi
fi

if [[ $failed == 1 ]]; then
  echo "secret scan: refused. Remove what it found (from the commits too, with git commit --amend or a rebase) and try again." >&2
  exit 1
fi
echo "secret scan: clean" >&2

# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0
