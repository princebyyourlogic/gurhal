// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func (p *Postgres) AccountByGID(ctx context.Context, gid uint32) (Account, error) {
	var name string
	err := p.pool.QueryRow(ctx, `SELECT username FROM accounts WHERE gid = $1`, int32(gid)).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, err
	}
	return p.Account(ctx, name)
}

func (p *Postgres) FindGID(ctx context.Context, name string) (uint32, error) {
	var gid int32
	err := p.pool.QueryRow(ctx, `SELECT gid FROM (
		SELECT gid, 0 AS o FROM accounts WHERE lower(username) = lower($1)
		UNION ALL SELECT gid, 1 FROM characters WHERE lower(name) = lower($1)) x ORDER BY o LIMIT 1`, name).Scan(&gid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return uint32(gid), err
}

func (p *Postgres) CountAccounts(ctx context.Context) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx, `SELECT count(*) FROM accounts`).Scan(&n)
	return n, err
}

func (p *Postgres) AllCharacters(ctx context.Context) ([]Owned, error) {
	rows, err := p.pool.Query(ctx, `SELECT gid, slot, name, raw, state FROM characters`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanOwned)
}

func scanOwned(row pgx.CollectableRow) (Owned, error) {
	var o Owned
	var gid int32
	var slot int16
	err := row.Scan(&gid, &slot, &o.Name, &o.Raw, &o.State)
	o.GID, o.Slot = uint32(gid), int(slot)
	return o, err
}

func (p *Postgres) BannedGIDs(ctx context.Context, now time.Time) ([]uint32, error) {
	rows, err := p.pool.Query(ctx, `SELECT gid FROM bans WHERE until > $1`, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (uint32, error) {
		var g int32
		err := row.Scan(&g)
		return uint32(g), err
	})
}

func (p *Postgres) RecordClear(ctx context.Context, c Clear) error {
	party, err := json.Marshal(c.Party)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO clears (at, game, uni, quest, secs, rank, score, kills, party)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, c.At, c.Game, int32(c.Uni), int32(c.Quest), c.Secs,
		int16(c.Rank), c.Score, c.Kills, string(party))
	return err
}

func (p *Postgres) Clears(ctx context.Context) ([]Clear, error) {
	rows, err := p.pool.Query(ctx, `SELECT at, game, uni, quest, secs, rank, score, kills, party FROM clears ORDER BY at, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Clear, error) {
		var c Clear
		var uni, quest int32
		var rank int16
		var party []byte
		if err := row.Scan(&c.At, &c.Game, &uni, &quest, &c.Secs, &rank, &c.Score, &c.Kills, &party); err != nil {
			return c, err
		}
		c.Uni, c.Quest, c.Rank = uint32(uni), uint32(quest), int(rank)
		return c, json.Unmarshal(party, &c.Party)
	})
}

func (p *Postgres) Events(ctx context.Context) ([]Event, error) {
	rows, err := p.pool.Query(ctx, `SELECT key, spec FROM events ORDER BY key`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		err := row.Scan(&e.Key, &e.Spec)
		return e, err
	})
}

func (p *Postgres) SetEvent(ctx context.Context, key string, spec []byte) error {
	if spec == nil {
		_, err := p.pool.Exec(ctx, `DELETE FROM events WHERE key = $1`, key)
		return err
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO events (key, spec) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET spec = excluded.spec`, key, string(spec))
	return err
}

func (p *Postgres) Mutes(ctx context.Context) ([]Mute, error) {
	rows, err := p.pool.Query(ctx, `SELECT gid, until, reason, by_whom FROM mutes`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Mute, error) {
		var m Mute
		var gid int32
		var until *time.Time
		err := row.Scan(&gid, &until, &m.Reason, &m.By)
		m.GID = uint32(gid)
		if until != nil {
			m.Until = *until
		}
		return m, err
	})
}

func (p *Postgres) SetMute(ctx context.Context, m Mute) error {
	var until any
	if !m.Until.IsZero() {
		until = m.Until
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO mutes (gid, until, reason, by_whom) VALUES ($1, $2, $3, $4)
		ON CONFLICT (gid) DO UPDATE SET until = excluded.until, reason = excluded.reason, by_whom = excluded.by_whom`,
		int32(m.GID), until, m.Reason, m.By)
	return err
}

func (p *Postgres) Unmute(ctx context.Context, gid uint32) (bool, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM mutes WHERE gid = $1`, int32(gid))
	return tag.RowsAffected() > 0, err
}

func (p *Postgres) HelpOpen(ctx context.Context, gid uint32) (int64, bool, error) {
	var id int64
	err := p.pool.QueryRow(ctx, `SELECT id FROM help_threads WHERE gid = $1 AND NOT closed ORDER BY id DESC LIMIT 1`,
		int32(gid)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

func (p *Postgres) HelpSent(ctx context.Context, gid uint32, since time.Time) (int, error) {
	var n int
	err := p.pool.QueryRow(ctx, `SELECT count(*) FROM help_messages m JOIN help_threads t ON t.id = m.thread
		WHERE t.gid = $1 AND m.sender = 'player' AND m.at > $2`, int32(gid), since).Scan(&n)
	return n, err
}

func (p *Postgres) HelpAsk(ctx context.Context, gid uint32, name, subject, text string, at time.Time) (int64, error) {
	var id int64
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE help_threads SET updated = $2, waiting = true, name = $3
			WHERE id = (SELECT id FROM help_threads WHERE gid = $1 AND NOT closed ORDER BY id DESC LIMIT 1)
			RETURNING id`, int32(gid), at, name).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `INSERT INTO help_threads (gid, name, subject, created, updated)
				VALUES ($1, $2, $3, $4, $4) RETURNING id`, int32(gid), name, subject, at).Scan(&id)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO help_messages (thread, sender, text, at, delivered)
			VALUES ($1, 'player', $2, $3, true)`, id, text, at)
		return err
	})
	return id, err
}

func (p *Postgres) HelpLatestAnswer(ctx context.Context, thread int64) (string, bool, error) {
	var text string
	err := p.pool.QueryRow(ctx, `SELECT text FROM help_messages WHERE thread = $1 AND sender = 'staff'
		ORDER BY id DESC LIMIT 1`, thread).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return text, err == nil, err
}

func (p *Postgres) HelpUndelivered(ctx context.Context, gid uint32) ([]HelpMessage, error) {
	rows, err := p.pool.Query(ctx, `SELECT m.id, m.thread, m.text, m.at FROM help_messages m
		JOIN help_threads t ON t.id = m.thread
		WHERE t.gid = $1 AND m.sender = 'staff' AND NOT m.delivered ORDER BY m.id`, int32(gid))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (HelpMessage, error) {
		var h HelpMessage
		err := row.Scan(&h.ID, &h.Thread, &h.Text, &h.At)
		return h, err
	})
}

func (p *Postgres) HelpDelivered(ctx context.Context, id int64) error {
	_, err := p.pool.Exec(ctx, `UPDATE help_messages SET delivered = true WHERE id = $1`, id)
	return err
}

func (p *Postgres) HelpReply(ctx context.Context, thread int64, text, by string, closeIt bool, at time.Time) (uint32, error) {
	var gid int32
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE help_threads SET updated = $2, waiting = false, closed = closed OR $3
			WHERE id = $1 RETURNING gid`, thread, at, closeIt).Scan(&gid)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO help_messages (thread, sender, text, at, by_whom)
			VALUES ($1, 'staff', $2, $3, $4)`, thread, text, at, by)
		return err
	})
	return uint32(gid), err
}

func (p *Postgres) HelpClose(ctx context.Context, thread int64, at time.Time) (uint32, bool, error) {
	var gid int32
	var was bool
	err := p.pool.QueryRow(ctx, `UPDATE help_threads t SET closed = true, waiting = false, updated = $2
		FROM (SELECT id, closed FROM help_threads WHERE id = $1 FOR UPDATE) old
		WHERE t.id = old.id RETURNING t.gid, old.closed`, thread, at).Scan(&gid, &was)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, ErrNotFound
	}
	return uint32(gid), was, err
}
