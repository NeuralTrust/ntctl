# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub:
[Report a vulnerability](https://github.com/NeuralTrust/ntctl/security/advisories/new).
Do not open a public issue.

Include the ntctl version (`ntctl version`), what you did, what happened, and
how to reproduce it. We keep you informed through the advisory until a fix is
released.

## Supported versions

Fixes go into the latest release. Upgrade with the install script, or download
the newest release and check its signature as the README describes.

## Verifying a release

Every release publishes `checksums.txt`, signed with keyless cosign by this
repository's release workflow, and an SBOM for each archive. `install.sh`
checks the checksum, and the signature when cosign is installed. Set
`NTCTL_REQUIRE_SIGNATURE=1` to make the signature check mandatory.
