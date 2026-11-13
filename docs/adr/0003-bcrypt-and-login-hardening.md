# 3. bcrypt with a configurable cost and a hardened login path

Status: accepted (reconstructed from the code and the earlier README).

## Context

Passwords must be hashed with a slow algorithm; login must not reveal which addresses have accounts; there is no other brute-force protection in front of the service.

## Decision

Passwords use bcrypt at `BCRYPT_COST` (default 11, 4-16, invalid values stop the process). bcrypt ignores bytes after 72, so longer passwords are refused at registration. Login gives one code and message for every credential failure and compares against a dummy hash for unknown addresses. A sliding-window limiter per normalised e-mail address (default 5 per minute) counts the attempt before the password is checked; a global registration limiter (60 per minute) brakes floods.

## Consequences

- Both limiters live in the memory of one process: restart resets them, N replicas allow N times the limit.
- `Register` still reveals existing addresses (`ALREADY_EXISTS`), because the frontend contract needs it.
