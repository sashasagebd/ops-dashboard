package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// An app's database password is only ever plaintext inside these methods,
// like env var values: CreateDatabase generates and encrypts it, and the
// readers decrypt it.

// Database is an app's own Postgres, as the platform should run it.
type Database struct {
	AppID         int64
	Password      string // decrypted
	CreatedAt     time.Time
	ProvisionedAt *time.Time // when its container was first created; nil until then
}

func databaseAD(appID int64) []byte { return fmt.Appendf(nil, "app_databases:%d", appID) }

// CreateDatabase gives an app a database: a row with a new random password
// (32 bytes, as hex, so it needs no escaping in a URL). The reconciler then
// creates it. ErrConflict if the app has one already; ErrNotFound if there's
// no such app.
func (s *Store) CreateDatabase(ctx context.Context, appID int64) (Database, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Database{}, err
	}
	password := hex.EncodeToString(raw)
	sealed, err := s.box.Seal([]byte(password), databaseAD(appID))
	if err != nil {
		return Database{}, err
	}
	var created time.Time
	err = s.pool.QueryRow(ctx,
		"INSERT INTO app_databases (app_id, encrypted_password) VALUES ($1, $2) RETURNING created_at",
		appID, sealed).Scan(&created)
	if err != nil {
		return Database{}, translate(err)
	}
	return Database{AppID: appID, Password: password, CreatedAt: created}, nil
}

// Database returns an app's database, or ErrNotFound if it has none.
func (s *Store) Database(ctx context.Context, appID int64) (Database, error) {
	dbs, err := s.databases(ctx, "WHERE app_id = $1", appID)
	if err != nil {
		return Database{}, err
	}
	if len(dbs) == 0 {
		return Database{}, ErrNotFound
	}
	return dbs[0], nil
}

// Databases returns every app's database, by app ID.
func (s *Store) Databases(ctx context.Context) ([]Database, error) {
	return s.databases(ctx, "")
}

func (s *Store) databases(ctx context.Context, where string, args ...any) ([]Database, error) {
	type row struct {
		AppID         int64      `db:"app_id"`
		Sealed        []byte     `db:"encrypted_password"`
		CreatedAt     time.Time  `db:"created_at"`
		ProvisionedAt *time.Time `db:"provisioned_at"`
	}
	rows, _ := s.pool.Query(ctx,
		"SELECT app_id, encrypted_password, created_at, provisioned_at FROM app_databases "+where+" ORDER BY app_id", args...)
	sealed, err := pgx.CollectRows(rows, pgx.RowToStructByName[row])
	if err != nil {
		return nil, translate(err)
	}
	out := make([]Database, 0, len(sealed))
	for _, r := range sealed {
		password, err := s.box.Open(r.Sealed, databaseAD(r.AppID))
		if err != nil {
			return nil, fmt.Errorf("app %d's database password: %w", r.AppID, err)
		}
		out = append(out, Database{AppID: r.AppID, Password: string(password), CreatedAt: r.CreatedAt, ProvisionedAt: r.ProvisionedAt})
	}
	return out, nil
}

// MarkDatabaseProvisioned records that an app's database container has been
// created for the first time (the reconciler, once it has). Later calls keep
// the first time.
func (s *Store) MarkDatabaseProvisioned(ctx context.Context, appID int64) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE app_databases SET provisioned_at = COALESCE(provisioned_at, now()) WHERE app_id = $1", appID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
