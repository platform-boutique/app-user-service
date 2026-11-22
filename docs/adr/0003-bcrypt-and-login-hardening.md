# 3. bcrypt with a configurable cost and a hardened login path

Status: accepted (reconstructed from the code and the earlier README).

## Context

Passwords must be hashed with a slow algorithm; login must not reveal which addresses have accounts; there is no other brute-force protection in front of the service.

## Decision

Passwords use bcrypt at `BCRYPT_COST` (default 11, 4-16, invalid values stop the process). bcrypt ignores bytes after 72, so longer passwords are refused at registration. Login gives one code and message for every credential failure and compares against a dummy hash for unknown addresses. A sliding-window limiter per normalised e-mail address (default 5 per minute) counts the attempt before the password is checked; a global registration limiter (60 per minute) brakes floods.

## Consequences

- Both limiters live in the memory of one process: restart resets them, N replicas allow N times the limit.
- `Register` still reveals existing addresses (`ALREADY_EXISTS`), because the frontend contract needs it.
- bcrypt cannot be interrupted, so the 5 s `REQUEST_TIMEOUT` bounds only the database calls around it. Measured on the development host on 2026-11-22 (one hash each): cost 11 0.18 s, cost 14 1.5 s, cost 16 5.9 s. At cost 16 a registration or login can run past the timeout and then fail at its next database call. Documented theoretical case, accepted 2026-11-22: the default is 11, the upper limit 16 stays an operator's choice.
