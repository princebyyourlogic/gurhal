-- Accounts, moderation, news and the account audit trail.

CREATE TABLE accounts (
    gid           integer PRIMARY KEY CHECK (gid > 0),
    username      text NOT NULL,
    salt          bytea NOT NULL,
    pw_hash       bytea NOT NULL,
    reset_pending boolean NOT NULL DEFAULT false,
    created       timestamptz NOT NULL DEFAULT now(),
    last_login    timestamptz
);
CREATE UNIQUE INDEX accounts_username ON accounts (lower(username));

CREATE TABLE bans (
    gid     integer PRIMARY KEY REFERENCES accounts ON DELETE CASCADE,
    until   timestamptz NOT NULL,
    reason  text NOT NULL DEFAULT '',
    by_whom text NOT NULL DEFAULT '',
    created timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE news (
    id      bigserial PRIMARY KEY,
    body    text NOT NULL,
    by_whom text NOT NULL DEFAULT '',
    created timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE audit (
    id       bigserial PRIMARY KEY,
    at       timestamptz NOT NULL DEFAULT now(),
    op       text NOT NULL,
    gid      integer NOT NULL,
    username text NOT NULL,
    by_whom  text NOT NULL DEFAULT ''
);
CREATE INDEX audit_gid ON audit (gid, at);
