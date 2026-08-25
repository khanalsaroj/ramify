# Security Policy

## Supported versions

Only the latest release on the `main` branch is currently supported with security
fixes.

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability. Report it privately to
the repository maintainers through GitHub's private security advisory workflow.
Include:

- the affected version or commit;
- the affected component and configuration;
- impact and realistic attack prerequisites;
- a minimal defensive reproduction or test case, if available.

Please do not access systems or data that do not belong to you while validating a
report. Maintainers will acknowledge reports within seven days and will coordinate
disclosure after a fix or mitigation is available.

## Security expectations for deployments

- Keep the control API on the Unix socket unless remote access is required.
- Set explicit socket ownership and permissions.
- Use a strong TCP token and TLS/mTLS when exposing the TCP API.
- Configure SSH known-hosts verification.
- Scope GitHub and Cloudflare credentials to the minimum required resources.
- Keep the SQLite database, certificate storage, SSH key, and configuration file
  readable only by the daemon account.
