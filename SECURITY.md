# Security Policy

## Reporting a vulnerability

Please report vulnerabilities privately to the maintainers.
Do not open public issues for security-sensitive reports.

## Security baseline

- Agent startup enforces model manifest verification.
- Runtime model artifacts are treated as opaque and checksum-validated.
- Secrets must not be committed to repository history.
