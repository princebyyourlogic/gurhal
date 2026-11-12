-- Partner cards, mail and the blacklist: records as the client sends them.

CREATE TABLE cards (
    gid  integer NOT NULL REFERENCES accounts ON DELETE CASCADE,
    slot smallint NOT NULL CHECK (slot BETWEEN 0 AND 199),
    rec  bytea NOT NULL,
    PRIMARY KEY (gid, slot)
);

CREATE TABLE mail (
    id  bigserial PRIMARY KEY,
    gid integer NOT NULL REFERENCES accounts ON DELETE CASCADE,
    rec bytea NOT NULL
);
CREATE INDEX mail_gid ON mail (gid, id);

CREATE TABLE blacklist (
    gid     integer NOT NULL REFERENCES accounts ON DELETE CASCADE,
    who     integer NOT NULL,
    entry   bytea NOT NULL,
    added   bigserial,
    PRIMARY KEY (gid, who)
);
