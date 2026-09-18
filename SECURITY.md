# Security policy

## Supported versions

Maintainer Cockpit is pre-1.0. Only the most recent published release receives
security fixes. Upgrade guidance accompanies every release.

## Report a vulnerability

Do not open a public issue for a suspected vulnerability. Use the repository's
**Security → Report a vulnerability** form to create a private security
advisory:

https://github.com/ArthurSens/maintainer-cockpit/security/advisories/new

Include affected versions, impact, reproduction steps, and any suggested
mitigation. The maintainer will acknowledge a complete report within seven
days and coordinate disclosure after a fix is available. Please avoid accessing
other people's deployments or data while investigating.

## Security boundaries

Maintainer Cockpit reads public GitHub repositories and stores credentials,
sessions, private maintainer state, and retained model payloads. Treat its
SQLite volume and secret sources as sensitive. Deploy it behind HTTPS, use
least-privileged GitHub credentials, and do not expose the administrator reload
secret.
