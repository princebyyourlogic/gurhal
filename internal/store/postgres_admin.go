// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (p *Postgres) Accounts(ctx context.Context, pattern string, limit int) ([]AccountRow, error) {
	rows, err := p.pool.Query(ctx, `SELECT a.gid, a.username, a.reset_pending, a.created, a.last_login,
			coalesce(array(SELECT name FROM characters c WHERE c.gid = a.gid ORDER BY slot), '{}'),
			b.until, coalesce(b.reason, '')
		FROM accounts a LEFT JOIN bans b ON b.gid = a.gid
		WHERE $1 = '' OR strpos(lower(a.username), lower($1)) > 0
			OR EXISTS (SELECT 1 FROM characters c WHERE c.gid = a.gid AND strpos(lower(c.name), lower($1)) > 0)
		ORDER BY lower(a.username) LIMIT $2`, pattern, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (AccountRow, error) {
		var r AccountRow
		var gid int32
		var last, until *time.Time
		err := row.Scan(&gid, &r.Username, &r.ResetPending, &r.Created, &last, &r.Characters, &until, &r.BanReason)
		r.GID = uint32(gid)
		if last != nil {
			r.LastLogin = *last
		}
		if until != nil {
			r.BannedUntil = *until
		}
		return r, err
	})
}

func (p *Postgres) RenameAccount(ctx context.Context, gid uint32, name string) error {
	tag, err := p.pool.Exec(ctx, `UPDATE accounts SET username = $2 WHERE gid = $1`, int32(gid), name)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return ErrNameTaken
	}
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (p *Postgres) DeleteAccount(ctx context.Context, gid uint32) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM accounts WHERE gid = $1`, int32(gid)) // the rest cascades
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (p *Postgres) NoticeList(ctx context.Context) ([]Notice, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, body, by_whom, created FROM news ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Notice, error) {
		var n Notice
		err := row.Scan(&n.ID, &n.Text, &n.By, &n.Created)
		return n, err
	})
}

func (p *Postgres) DeleteNotice(ctx context.Context, id int64) (bool, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM news WHERE id = $1`, id)
	return tag.RowsAffected() > 0, err
}

const helpThreadCols = `t.id, t.gid, t.name, t.subject, t.created, t.updated, t.closed, t.waiting AND NOT t.closed,
	(SELECT count(*) FROM help_messages m WHERE m.thread = t.id)`

func scanThread(row pgx.Row) (HelpThread, error) {
	var t HelpThread
	var gid int32
	err := row.Scan(&t.ID, &gid, &t.Name, &t.Subject, &t.Created, &t.Updated, &t.Closed, &t.Waiting, &t.Messages)
	t.GID = uint32(gid)
	return t, err
}

func (p *Postgres) HelpThreads(ctx context.Context, openOnly bool, limit int) ([]HelpThread, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+helpThreadCols+` FROM help_threads t
		WHERE NOT ($1 AND t.closed) ORDER BY t.updated DESC, t.id DESC LIMIT $2`, openOnly, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (HelpThread, error) { return scanThread(row) })
}

func (p *Postgres) HelpThread(ctx context.Context, id int64) (HelpThread, []HelpEntry, error) {
	t, err := scanThread(p.pool.QueryRow(ctx, `SELECT `+helpThreadCols+` FROM help_threads t WHERE t.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return HelpThread{}, nil, ErrNotFound
	}
	if err != nil {
		return t, nil, err
	}
	rows, err := p.pool.Query(ctx, `SELECT sender = 'staff', text, by_whom, at, delivered FROM help_messages
		WHERE thread = $1 ORDER BY id`, id)
	if err != nil {
		return t, nil, err
	}
	msgs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (HelpEntry, error) {
		var e HelpEntry
		err := row.Scan(&e.Staff, &e.Text, &e.By, &e.At, &e.Delivered)
		return e, err
	})
	return t, msgs, err
}

func (p *Postgres) AuditTail(ctx context.Context, n int) ([]AuditEntry, error) {
	rows, err := p.pool.Query(ctx, `SELECT at, op, gid, username, by_whom FROM audit ORDER BY id DESC LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (AuditEntry, error) {
		var e AuditEntry
		var gid int32
		err := row.Scan(&e.At, &e.Op, &gid, &e.Username, &e.By)
		e.GID = uint32(gid)
		return e, err
	})
}
