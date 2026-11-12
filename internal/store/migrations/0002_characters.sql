-- Characters: the client's own 0x0D02 body, and the progression/inventory state the server owns.

CREATE TABLE characters (
    gid     integer NOT NULL REFERENCES accounts ON DELETE CASCADE,
    slot    smallint NOT NULL CHECK (slot BETWEEN 0 AND 3),
    name    text NOT NULL,
    raw     bytea NOT NULL,
    state   jsonb,
    created timestamptz NOT NULL DEFAULT now(),
    updated timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (gid, slot)
);
CREATE INDEX characters_name ON characters (lower(name));
