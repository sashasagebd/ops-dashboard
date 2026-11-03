package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Env var values are only ever plaintext inside these methods: SetEnvVar
// encrypts before writing and EnvVars decrypts after reading, so nothing
// else can store a value unencrypted by mistake.

// envVarAD is the associated data binding an encrypted value to its row.
// Key names can't contain ':' (see the migration), so this is unambiguous.
func envVarAD(appID int64, key string) []byte {
	return fmt.Appendf(nil, "app_env_vars:%d:%s", appID, key)
}

// SetEnvVar sets (or replaces) one of an app's env vars. An unknown app is
// ErrNotFound; an invalid key name is ErrInvalid.
func (s *Store) SetEnvVar(ctx context.Context, appID int64, key, value string) error {
	sealed, err := s.box.Seal([]byte(value), envVarAD(appID, key))
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO app_env_vars (app_id, key, encrypted_value) VALUES ($1, $2, $3)
		ON CONFLICT (app_id, key)
		DO UPDATE SET encrypted_value = EXCLUDED.encrypted_value, updated_at = now()`,
		appID, key, sealed)
	return translate(err)
}

// DeleteEnvVar removes one of an app's env vars, or returns ErrNotFound.
func (s *Store) DeleteEnvVar(ctx context.Context, appID int64, key string) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM app_env_vars WHERE app_id = $1 AND key = $2", appID, key)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// EnvVarKeys returns the names of an app's env vars, sorted, without
// decrypting anything. It's what the API shows: values are write-only.
func (s *Store) EnvVarKeys(ctx context.Context, appID int64) ([]string, error) {
	rows, _ := s.pool.Query(ctx, "SELECT key FROM app_env_vars WHERE app_id = $1 ORDER BY key", appID)
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	return keys, translate(err)
}

// EnvVars returns an app's env vars, decrypted, for starting its container.
// A value that fails to decrypt (wrong PLATFORM_SECRET_KEY, or a tampered
// row) fails the whole call, naming the key but never the value: starting
// an app with an env var silently missing would be worse.
func (s *Store) EnvVars(ctx context.Context, appID int64) (map[string]string, error) {
	type row struct {
		Key   string `db:"key"`
		Value []byte `db:"encrypted_value"`
	}
	rows, _ := s.pool.Query(ctx, "SELECT key, encrypted_value FROM app_env_vars WHERE app_id = $1", appID)
	sealed, err := pgx.CollectRows(rows, pgx.RowToStructByName[row])
	if err != nil {
		return nil, translate(err)
	}

	vars := make(map[string]string, len(sealed))
	for _, r := range sealed {
		value, err := s.box.Open(r.Value, envVarAD(appID, r.Key))
		if err != nil {
			return nil, fmt.Errorf("env var %s: %w", r.Key, err)
		}
		vars[r.Key] = string(value)
	}
	return vars, nil
}
