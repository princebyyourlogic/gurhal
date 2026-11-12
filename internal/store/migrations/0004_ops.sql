-- Mission clears (the board), the operator's event calendar, chat mutes and the help desk.

CREATE TABLE clears (
    id    bigserial PRIMARY KEY,
    at    timestamptz NOT NULL DEFAULT now(),
    game  text NOT NULL,
    uni   integer NOT NULL,
    quest integer NOT NULL,
    secs  integer NOT NULL,
    rank  smallint NOT NULL,
    score integer NOT NULL,
    kills integer NOT NULL,
    party jsonb NOT NULL -- [{"gid", "slot", "name"}]
);
CREATE INDEX clears_quest ON clears (quest, secs);
CREATE INDEX clears_at ON clears (at);

CREATE TABLE events (
    key  text PRIMARY KEY,
    spec jsonb NOT NULL
);

CREATE TABLE mutes (
    gid     integer PRIMARY KEY REFERENCES accounts ON DELETE CASCADE,
    until   timestamptz, -- NULL: until unmuted
    reason  text NOT NULL DEFAULT '',
    by_whom text NOT NULL DEFAULT ''
);

CREATE TABLE help_threads (
    id      bigserial PRIMARY KEY,
    gid     integer NOT NULL REFERENCES accounts ON DELETE CASCADE,
    name    text NOT NULL,
    subject text NOT NULL,
    created timestamptz NOT NULL DEFAULT now(),
    updated timestamptz NOT NULL DEFAULT now(),
    closed  boolean NOT NULL DEFAULT false,
    waiting boolean NOT NULL DEFAULT true
);
CREATE INDEX help_threads_gid ON help_threads (gid, id);

CREATE TABLE help_messages (
    id        bigserial PRIMARY KEY,
    thread    bigint NOT NULL REFERENCES help_threads ON DELETE CASCADE,
    sender    text NOT NULL CHECK (sender IN ('player', 'staff')),
    text      text NOT NULL,
    at        timestamptz NOT NULL DEFAULT now(),
    by_whom   text NOT NULL DEFAULT '',
    delivered boolean NOT NULL DEFAULT false
);
CREATE INDEX help_messages_thread ON help_messages (thread, id);
