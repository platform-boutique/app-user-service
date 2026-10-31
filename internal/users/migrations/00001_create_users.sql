-- +goose Up
-- Accounts and their sessions. Passwords are stored as bcrypt hashes only; a session token is never stored,
-- only its SHA-256 (the token itself is returned once, by Register/Login).
CREATE TABLE users (
    id            uuid        PRIMARY KEY,
    email         text        NOT NULL CHECK (char_length(email) BETWEEN 3 AND 254),
    name          text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    password_hash text        NOT NULL CHECK (char_length(password_hash) BETWEEN 1 AND 100),
    created_at    timestamptz NOT NULL
);

-- One account per address, whatever the case. The unique index (not a pre-check) is what makes
-- concurrent registrations of the same address race-safe.
CREATE UNIQUE INDEX users_email_lower_key ON users (lower(email));

CREATE TABLE sessions (
    token_hash bytea       PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    CHECK (expires_at > created_at)
);

-- opportunistic clean-up of expired sessions, and "sessions of one user"
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- +goose Down
DROP TABLE sessions;
DROP TABLE users;
