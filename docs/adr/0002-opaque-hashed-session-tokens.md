# 2. Opaque session tokens, stored as SHA-256

Status: accepted (reconstructed from the code and the earlier README).

## Context

The frontend needs a session it can keep in a cookie and revoke on logout. Signed stateless tokens (JWT) cannot be revoked without a store.

## Decision

A session is 32 random bytes (`crypto/rand`, base64url, 43 characters), returned once by `Register` / `Login`. The database stores only `SHA-256(token)`, `user_id`, `created_at`, `expires_at`, `revoked_at`. `GetSession` hashes the presented token and checks expiry and revocation in the query. The lifetime is fixed (`SESSION_TTL`, default 14 days), not sliding.

## Consequences

- A leaked table or backup cannot be replayed as cookies. A fast hash is enough because the token has 256 random bits.
- Logout and revocation are immediate; several sessions per user can be live.
- Every `GetSession` is a database read.
