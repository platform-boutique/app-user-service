# 1. Accounts live in a Postgres owned by this service

Status: accepted (reconstructed from the code and the earlier README; no dated decision record existed).

## Context

Accounts and sessions must survive restarts and be checked on every signed-in page. The shop's other data services (product catalog, reviews, orders, inventory) each keep their own database.

## Decision

user-service owns one Postgres database (`users`, `sessions`). No other service reads it. The schema is created by embedded goose migrations run by the service at start-up, forward-only, under goose's advisory lock.

## Consequences

- Other services need the gRPC API; there are no cross-database joins. Order ownership is a `user_id` string stored by order-service, not a foreign key.
- The service is not ready (`NOT_SERVING`) until the database is migrated and reachable.
