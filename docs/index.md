# user-service

A gRPC service for the shop's accounts: register, log in, resolve a session token, log out.

- Passwords are stored as bcrypt hashes; a session is an opaque random token of which only the SHA-256 is stored.
- It owns its own Postgres database and creates the schema with embedded migrations at start-up.
- Demo grade: no email verification, no password reset, no roles. See [security notes](security.md).

| | |
|---|---|
| Runtime | Go 1.26, static binary, image `platform-demo/user-service:local` (built locally only) |
| Ports | gRPC `8086`, HTTP `9090` (`/metrics`, scraped by Alloy) |
| State | its own Postgres (`users`, `sessions`) |
| Called by | frontend |
| Status | not deployed anywhere yet; no pipeline, no Helm chart |

Pages: [architecture](architecture.md), [API](api.md), [configuration](configuration.md), [observability](observability.md), [operations](operations.md), [development](development.md), [security](security.md), [decisions](adr/0001-one-postgres-per-service.md).
