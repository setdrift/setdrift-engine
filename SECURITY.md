# Security Policy

## Reporting a vulnerability

Please report security issues privately, through [GitHub's private vulnerability reporting](https://github.com/setdrift/setdrift-engine/security/advisories/new), and not in a public issue or pull request. Only the maintainers see the report.

A useful report says what is affected (the command, flag or package, and the version from `setdrift version`), how to reproduce it, and what an attacker could do with it.

What happens next.

- **Acknowledgement -** Within a week, with a first assessment.
- **A fix -** In a patch release, as soon as one is ready, with a GitHub security advisory that credits you unless you'd rather not be named.
- **Disclosure -** Coordinated with you, once the fix is released.

There is no bug bounty.

## What counts

SetDrift reads clusters and writes what it finds to files, so these matter most.

- **A secret value leaving the cluster -** SetDrift must never store, save, print or move a secret value. Any way one reaches an export, a snapshot, a report, a log or the terminal is the most serious kind of report.
- **A change to a cluster -** Every command only reads, except `verify`'s server-side dry-run, which changes nothing. Any way SetDrift changes a cluster or a cloud account is a vulnerability.
- **Untrusted input -** A crafted manifest, snapshot, export or kubeconfig that makes SetDrift run code, read or write outside the paths it was given, or reach the network where it shouldn't.
- **The release -** The binaries, their checksums and attestations, the install scripts and the Homebrew formula.

The website, setdrift.io, is reported the same way.

## Supported versions

Fixes go into the latest release. SetDrift is at v0.x, so upgrading to the latest release is how to get them.
