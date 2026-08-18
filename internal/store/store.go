// Package store implements the PostgreSQL persistence layer.
package store

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"aihub/internal/cryptobox"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("store: not found")

// ErrConflict is returned when a unique constraint rejects a write.
var ErrConflict = errors.New("store: conflict")

// ErrUnauthorized is returned when a credential exists but may not be used: it
// was revoked, it expired, or the account behind it is not active. It is kept
// distinct from ErrNotFound so the HTTP layer can answer 401 rather than 500
// without having to treat every database failure as an authentication failure.
var ErrUnauthorized = errors.New("store: not authorized")

// Store is the entry point to every repository. It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
	box  *cryptobox.Box
}

// New builds a Store over an existing pool.
func New(pool *pgxpool.Pool, box *cryptobox.Box) *Store {
	return &Store{pool: pool, box: box}
}

// Pool exposes the underlying pool for health checks.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// mapErr normalises pgx errors into the sentinels above.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return err
}
