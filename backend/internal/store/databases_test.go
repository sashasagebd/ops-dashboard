package store

import (
	"errors"
	"regexp"
	"testing"
)

func TestDatabases(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	hello := createTestApp(t, s, "hello")
	api := createTestApp(t, s, "api")

	if _, err := s.Database(ctx, hello.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("before creating: error = %v, want ErrNotFound", err)
	}
	created, err := s.CreateDatabase(ctx, hello.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A long random password that needs no escaping in a URL.
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(created.Password) || created.CreatedAt.IsZero() {
		t.Errorf("created = %+v", created)
	}
	got, err := s.Database(ctx, hello.ID)
	if err != nil || got.Password != created.Password {
		t.Errorf("Database = %+v, %v; want the same password back", got, err)
	}

	// One per app.
	if _, err := s.CreateDatabase(ctx, hello.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("second database: error = %v, want ErrConflict", err)
	}
	if _, err := s.CreateDatabase(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing app: error = %v, want ErrNotFound", err)
	}
	other, _ := s.CreateDatabase(ctx, api.ID)
	if other.Password == created.Password {
		t.Error("two databases got the same password")
	}

	all, err := s.Databases(ctx)
	if err != nil || len(all) != 2 || all[0].AppID != hello.ID || all[1].AppID != api.ID {
		t.Errorf("Databases = %+v, %v", all, err)
	}

	// Stored encrypted, and bound to its row: copied to another app's row,
	// it doesn't decrypt.
	var sealed []byte
	_ = s.pool.QueryRow(ctx, "SELECT encrypted_password FROM app_databases WHERE app_id = $1", hello.ID).Scan(&sealed)
	if string(sealed) == created.Password || len(sealed) == 0 {
		t.Error("the password is stored in plaintext")
	}
	if _, err := s.pool.Exec(ctx, "UPDATE app_databases SET encrypted_password = $1 WHERE app_id = $2", sealed, api.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Database(ctx, api.ID); err == nil {
		t.Error("a password copied to another app's row decrypted")
	}

	// Not created until the reconciler says so; the first time is kept.
	if got.ProvisionedAt != nil {
		t.Errorf("new database provisioned at %v, want nil", got.ProvisionedAt)
	}
	mustDo(t, s.MarkDatabaseProvisioned(ctx, hello.ID))
	first, _ := s.Database(ctx, hello.ID)
	mustDo(t, s.MarkDatabaseProvisioned(ctx, hello.ID))
	again, _ := s.Database(ctx, hello.ID)
	if first.ProvisionedAt == nil || !again.ProvisionedAt.Equal(*first.ProvisionedAt) {
		t.Errorf("provisioned at %v then %v, want set once", first.ProvisionedAt, again.ProvisionedAt)
	}
	if err := s.MarkDatabaseProvisioned(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing app: error = %v, want ErrNotFound", err)
	}

	// The reconciler's new event kind is allowed.
	if err := s.AddEvent(ctx, NewEvent{AppID: hello.ID, Kind: EventDatabaseCreated, Message: "created"}); err != nil {
		t.Errorf("database_created event: %v", err)
	}
}
