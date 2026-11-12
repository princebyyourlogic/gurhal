// SPDX-License-Identifier: GPL-3.0-or-later

package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrationLock serializes schema changes when several psud processes start at once.
const migrationLock = 0x50535532 // "PSU2"

// Postgres stores everything in PostgreSQL. GIDs are stored as integer (they fit in 31 bits).
type Postgres struct {
	pool *pgxpool.Pool
}

// OpenPostgres connects and applies any migrations the database has not seen.
func OpenPostgres(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &Postgres{pool: pool}, nil
}

// migrate applies migrations/NNNN_*.sql in order, each once, in one transaction.
func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version integer PRIMARY KEY, applied timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		for _, f := range files {
			name := strings.TrimPrefix(f, "migrations/")
			version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
			if err != nil {
				return fmt.Errorf("%s: name must start with a number", name)
			}
			var done bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`,
				version).Scan(&done); err != nil {
				return err
			}
			if done {
				continue
			}
			sql, err := migrations.ReadFile(f)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) Account(ctx context.Context, username string) (Account, error) {
	var a Account
	var gid int32
	var last *time.Time
	err := p.pool.QueryRow(ctx, `SELECT gid, username, salt, pw_hash, reset_pending, created, last_login
		FROM accounts WHERE lower(username) = lower($1)`, username).
		Scan(&gid, &a.Username, &a.Salt, &a.Hash, &a.ResetPending, &a.Created, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	a.GID = uint32(gid)
	if last != nil {
		a.LastLogin = *last
	}
	return a, err
}

func (p *Postgres) CreateAccount(ctx context.Context, a Account) error {
	if a.Created.IsZero() {
		a.Created = time.Now()
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO accounts (gid, username, salt, pw_hash, reset_pending, created)
		VALUES ($1, $2, $3, $4, $5, $6)`, int32(a.GID), a.Username, a.Salt, a.Hash, a.ResetPending, a.Created)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		if pgErr.ConstraintName == "accounts_pkey" {
			return ErrGIDTaken
		}
		return ErrNameTaken
	}
	return err
}

func (p *Postgres) SetPassword(ctx context.Context, gid uint32, salt, hash []byte, resetPending bool) error {
	return p.execOne(ctx, `UPDATE accounts SET salt = $2, pw_hash = $3, reset_pending = $4 WHERE gid = $1`,
		int32(gid), salt, hash, resetPending)
}

func (p *Postgres) TouchLogin(ctx context.Context, gid uint32, at time.Time) error {
	return p.execOne(ctx, `UPDATE accounts SET last_login = $2 WHERE gid = $1`, int32(gid), at)
}

func (p *Postgres) Characters(ctx context.Context, gid uint32) ([]Character, error) {
	rows, err := p.pool.Query(ctx, `SELECT slot, name, raw, state FROM characters WHERE gid = $1 ORDER BY slot`, int32(gid))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Character, error) {
		var c Character
		var slot int16
		err := row.Scan(&slot, &c.Name, &c.Raw, &c.State)
		c.Slot = int(slot)
		return c, err
	})
}

func (p *Postgres) OpenShops(ctx context.Context) ([]Owned, error) {
	rows, err := p.pool.Query(ctx, `SELECT gid, slot, name, raw, state FROM characters
		WHERE (state->'shop'->>'open')::boolean`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanOwned)
}

func (p *Postgres) CreateCharacter(ctx context.Context, gid uint32, c Character) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO characters (gid, slot, name, raw, state) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (gid, slot) DO UPDATE SET name = excluded.name, raw = excluded.raw, state = excluded.state,
		created = now(), updated = now()`, int32(gid), int16(c.Slot), c.Name, c.Raw, nullJSON(c.State))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
		return ErrNotFound
	}
	return err
}

func (p *Postgres) SaveState(ctx context.Context, gid uint32, slot int, state []byte) error {
	return p.execOne(ctx, `UPDATE characters SET state = $3, updated = now() WHERE gid = $1 AND slot = $2`,
		int32(gid), int16(slot), nullJSON(state))
}

func (p *Postgres) SetRaw(ctx context.Context, gid uint32, slot int, raw []byte) error {
	return p.execOne(ctx, `UPDATE characters SET raw = $3, updated = now() WHERE gid = $1 AND slot = $2`,
		int32(gid), int16(slot), raw)
}

func (p *Postgres) Cards(ctx context.Context, gid uint32) (map[int][]byte, error) {
	rows, err := p.pool.Query(ctx, `SELECT slot, rec FROM cards WHERE gid = $1`, int32(gid))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int][]byte{}
	for rows.Next() {
		var slot int16
		var rec []byte
		if err := rows.Scan(&slot, &rec); err != nil {
			return nil, err
		}
		out[int(slot)] = rec
	}
	return out, rows.Err()
}

func (p *Postgres) SetCard(ctx context.Context, gid uint32, slot int, rec []byte) error {
	if rec == nil {
		_, err := p.pool.Exec(ctx, `DELETE FROM cards WHERE gid = $1 AND slot = $2`, int32(gid), int16(slot))
		return err
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO cards (gid, slot, rec) VALUES ($1, $2, $3)
		ON CONFLICT (gid, slot) DO UPDATE SET rec = excluded.rec`, int32(gid), int16(slot), rec)
	return err
}

func (p *Postgres) Mail(ctx context.Context, gid uint32, limit int) ([]MailRec, error) {
	rows, err := p.pool.Query(ctx, `SELECT id, rec FROM (SELECT id, rec FROM mail WHERE gid = $1 ORDER BY id DESC LIMIT $2) m
		ORDER BY id`, int32(gid), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (MailRec, error) {
		var m MailRec
		return m, row.Scan(&m.ID, &m.Rec)
	})
}

func (p *Postgres) AddMail(ctx context.Context, gid uint32, rec []byte) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO mail (gid, rec) VALUES ($1, $2)`, int32(gid), rec)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return ErrNotFound
	}
	return err
}

func (p *Postgres) SetMail(ctx context.Context, id int64, rec []byte) error {
	return p.execOne(ctx, `UPDATE mail SET rec = $2 WHERE id = $1`, id, rec)
}

func (p *Postgres) Blacklist(ctx context.Context, gid uint32) ([][]byte, error) {
	rows, err := p.pool.Query(ctx, `SELECT entry FROM blacklist WHERE gid = $1 ORDER BY added`, int32(gid))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[[]byte])
}

func (p *Postgres) SetBlacklist(ctx context.Context, gid, who uint32, entry []byte) error {
	if entry == nil {
		_, err := p.pool.Exec(ctx, `DELETE FROM blacklist WHERE gid = $1 AND who = $2`, int32(gid), int32(who))
		return err
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO blacklist (gid, who, entry) VALUES ($1, $2, $3)
		ON CONFLICT (gid, who) DO UPDATE SET entry = excluded.entry`, int32(gid), int32(who), entry)
	return err
}

// nullJSON stores an empty state as NULL rather than an invalid jsonb.
func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func (p *Postgres) ActiveBan(ctx context.Context, gid uint32, now time.Time) (Ban, bool, error) {
	var b Ban
	err := p.pool.QueryRow(ctx, `SELECT until, reason, by_whom FROM bans WHERE gid = $1 AND until > $2`,
		int32(gid), now).Scan(&b.Until, &b.Reason, &b.By)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ban{}, false, nil
	}
	return b, err == nil, err
}

func (p *Postgres) SetBan(ctx context.Context, gid uint32, b Ban) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO bans (gid, until, reason, by_whom) VALUES ($1, $2, $3, $4)
		ON CONFLICT (gid) DO UPDATE SET until = excluded.until, reason = excluded.reason,
		by_whom = excluded.by_whom, created = now()`, int32(gid), b.Until, b.Reason, b.By)
	return err
}

func (p *Postgres) Notices(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT body FROM news ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (p *Postgres) AddNotice(ctx context.Context, text, by string) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO news (body, by_whom) VALUES ($1, $2)`, text, by)
	return err
}

func (p *Postgres) Audit(ctx context.Context, op string, gid uint32, username, by string) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO audit (op, gid, username, by_whom) VALUES ($1, $2, $3, $4)`,
		op, int32(gid), username, by)
	return err
}

func (p *Postgres) execOne(ctx context.Context, sql string, args ...any) error {
	tag, err := p.pool.Exec(ctx, sql, args...)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}
