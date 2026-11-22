# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in cyoda-go, please report it privately.

**Email:** infosec@cyoda.com

**Please include:**

- A description of the vulnerability and its potential impact.
- Steps to reproduce, minimal code or configuration if possible.
- The affected version(s).
- Any suggested mitigation.

**Please do not open public GitHub issues for security reports.** GitHub's [private security advisories](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability) are also an acceptable channel.

## Response Expectations

- **Acknowledgement:** within 3 business days.
- **Initial assessment:** within 10 business days.
- **Coordinated disclosure:** we will work with you on a timeline. Typical window is 90 days from initial report to public disclosure; extended when a fix is complex or widely deployed.

## Supported Versions

Cyoda-go is pre-1.0. Only the latest minor release is supported. A security fix is made on the current `release/vX.Y.Z` branch and ships in the next release. Older versions are not maintained.

## Scope

In scope:

- Vulnerabilities in cyoda-go and its stock plugins (`plugins/memory`, `plugins/postgres`).
- Vulnerabilities in `cyoda-go-spi` that affect plugin authors or operators.
- Supply-chain issues in direct dependencies.

Out of scope (report upstream):

- Vulnerabilities in Go itself, or in third-party dependencies whose upstream project is active.
- Denial-of-service from legitimate heavy load against a single-node configuration (cyoda-go is horizontally scalable; use cluster mode and appropriate resource limits).
