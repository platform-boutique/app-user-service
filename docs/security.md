# Security notes

What it does:

- **Passwords**: bcrypt at `BCRYPT_COST`, random salt per hash, never logged, never returned. Only the hash is in the database.
- **Sessions**: the token is 256 random bits; the database stores only `SHA-256(token)` (plus `user_id`, `created_at`, `expires_at`, `revoked_at`), so a leaked table or backup cannot be replayed as cookies. A fast hash is enough because there is nothing to brute-force. `GetSession` checks expiry and revocation in the query; a token that is not 43 base64url characters is refused without a database call.
- **No user enumeration at login**: an unknown address, a wrong password, a malformed address and a password over 72 bytes all take the same path and give the same code and message. For an unknown address the service compares against a dummy bcrypt hash made at the configured cost, so a failed login costs exactly one bcrypt comparison either way (a unit test counts the comparisons for every failure case; response times were not measured). If `BCRYPT_COST` is changed later, accounts hashed at the old cost still compare at the old cost.
- **Login rate limit**: sliding window per normalised e-mail address (default 5 per minute), in the memory of one process (same style as the review-service limiter), applied to known and unknown addresses alike. The attempt is counted before the password is checked, so parallel guesses cannot all slip through while bcrypt runs. A success resets the count. Refused attempts are not counted, so a block ends one window after the last counted attempt: someone who keeps failing for an address cannot lock its owner out for good, only for up to one window at a time.
- **Race-safe registration**: uniqueness is the unique index on `lower(email)`, not a pre-check. A violation maps to `ALREADY_EXISTS`; of N concurrent registrations of one address exactly one wins (tested with 30 goroutines on Postgres, mixed case).
- **Expired sessions** are deleted opportunistically: a login or registration starts a background `DELETE ... WHERE expires_at <= now`, at most once a minute per process. No cron. Revoked sessions stay until they expire.
- **Errors** never contain SQL text, the e-mail or the password; logs contain user ids, never e-mail addresses, passwords, tokens, hashes (a unit test and an integration test fail if a log line contains any of them, with a passing control that proves they were looking at real output).

What it does not do (accepted for the demo, say so before using it for anything real):

- **No email verification and no password reset.** A lost password means a lost account.
- **User enumeration on `Register` is accepted**: `ALREADY_EXISTS` tells the caller that an address has an account. The frontend contract needs that message. Only `Login` is hardened.
- The limiters are **per process**: with N replicas the effective limit is N times higher and a restart resets them. There is no per-client or per-IP limit (the frontend is the only caller and the service sees one address). The register limit is one global bucket, so it protects the CPU and the table from floods but also lets a flood block other registrations for a minute. When 50,000 distinct keys are inside their window a new key is refused (fail closed), which would block logins of addresses not seen before; a flood of 50,000 distinct addresses in one minute is the price.
- bcrypt work is not capped: many concurrent logins use many cores.
- No multi-factor authentication, no account deletion, no listing or revoking of other sessions, no change of password or email.
- Plain gRPC without TLS or authentication of the caller: anyone who can reach the port can call it (in the local stack it is on the internal network only).
- A failed `Register` after the bcrypt step leaves nothing behind (the account and its first session are one transaction); `Login` after a successful password check and a failed session insert returns an error and the user logs in again.
