# Security policy

Albert runs package-manager commands on your Mac, so security reports matter to us.

## Reporting a vulnerability

Please **do not open a public issue**. Use GitHub's private reporting instead:
**Security → Report a vulnerability** on https://github.com/kamllistos/albert/security/advisories/new

Include what you found, how to reproduce it, and which version (`albert version`) you tested. You'll get a reply within 7 days.

## Supported versions

Only the latest release receives security fixes.

## Design notes

How Albert protects its local agent (localhost-only listener, per-launch token, one-time sign-in code, Host/Origin checks, no secret in the GUI container) is described in the Security section of the [README](README.md#security).
