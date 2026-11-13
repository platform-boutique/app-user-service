# API

Package `hipstershop.users`, service `UserService` ([`proto/users.proto`](https://github.com/platform-boutique/app-user-service/blob/main/proto/users.proto), copied verbatim from the slice-5 contract of the shop design). The package is a sub-package of `hipstershop` on purpose, like `hipstershop.orders`: a client that also compiles `demo.proto` gets no name clashes.

| Method | Request | Behaviour | Error codes |
|---|---|---|---|
| `Register` | `email`, `password`, `name` | Creates the account and signs it in (same response as `Login`: `user`, `token`, `expires_at`). | `INVALID_ARGUMENT` (message starts with the field name, e.g. `password must be 8-72 bytes (got 5)`), `ALREADY_EXISTS` (`an account with this email already exists`), `RESOURCE_EXHAUSTED`, `UNAVAILABLE`, `INTERNAL` |
| `Login` | `email`, `password` | Checks the credentials and opens a new session. Several sessions per user can be live at once (one per browser). | `UNAUTHENTICATED` with the ONE message `wrong email or password` for an unknown email, a wrong password and a password that cannot be right (over 72 bytes); `RESOURCE_EXHAUSTED` (`too many attempts, try again in N seconds`); `INVALID_ARGUMENT` only when email or password is empty; `UNAVAILABLE`, `INTERNAL` |
| `GetSession` | `token` | Returns the `User` of a live session. | `UNAUTHENTICATED` (`invalid or expired session`) for an unknown, malformed, expired or logged-out token; `UNAVAILABLE`, `INTERNAL` |
| `Logout` | `token` | Revokes that one session. Idempotent: an unknown, malformed, empty or already revoked token is `OK` and changes nothing (a second call does not move `revoked_at`). | `UNAVAILABLE`, `INTERNAL` |

All handlers answer `UNAVAILABLE` (`database not ready`) until the migrations have run; a database outage later gives `UNAVAILABLE` (`database unavailable`) without SQL details (those are logged); a handler panic becomes `INTERNAL` through a recovery interceptor.

Field rules:

- `email`: trimmed and lower-cased by the service; 3-254 characters; a plain address (`net/mail`, no display name, no `<...>`, no control characters). Unique ignoring case.
- `password`: 8-72 **bytes** (bcrypt ignores everything after byte 72, so a longer password is refused instead of silently truncated); not trimmed, not changed. At `Login` a password over 72 bytes is just a failed login.
- `name`: trimmed; 1-100 characters; no control characters.
- `id` is a UUID v4; `token` is 32 random bytes from `crypto/rand` as base64url without padding (43 characters), returned only by `Register`/`Login`; `expires_at` is login time plus `SESSION_TTL` (default 14 days, fixed, not sliding).

Examples (against a started container or the compose stack):

```bash
grpcurl -plaintext -d '{"email":"ada@example.com","password":"correct horse","name":"Ada"}' localhost:8086 hipstershop.users.UserService/Register
grpcurl -plaintext -d '{"email":"ADA@example.com","password":"correct horse"}' localhost:8086 hipstershop.users.UserService/Login
grpcurl -plaintext -d '{"token":"<token>"}' localhost:8086 hipstershop.users.UserService/GetSession
grpcurl -plaintext -d '{"token":"<token>"}' localhost:8086 hipstershop.users.UserService/Logout
```
