package store

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sashasagebd/ops-dashboard/backend/internal/secrets"
)

// Like the migration tests, these need a real Postgres (DATABASE_URL), since
// most of what they check is enforced by the database itself.

// testStore returns a Store on a fresh, migrated schema.
func testStore(t *testing.T) *Store {
	t.Helper()
	box, err := secrets.New(bytes.Repeat([]byte{7}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{pool: testPool(t), box: box}
	if _, err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s
}

func createTestApp(t *testing.T, s *Store, name string) App {
	t.Helper()
	app, err := s.CreateApp(t.Context(), NewApp{
		Name:     name,
		RepoURL:  "https://github.com/example/" + name,
		Hostname: name + ".example.test",
		Port:     8080,
	})
	if err != nil {
		t.Fatalf("CreateApp(%s): %v", name, err)
	}
	return app
}

const (
	sha1 = "1111111111111111111111111111111111111111"
	sha2 = "2222222222222222222222222222222222222222"
	sha3 = "3333333333333333333333333333333333333333"
)

func TestCreateAppDefaultsAndOverrides(t *testing.T) {
	s := testStore(t)

	app := createTestApp(t, s, "hello")
	if app.ID == 0 || app.CreatedAt.IsZero() {
		t.Errorf("app missing ID or CreatedAt: %+v", app)
	}
	if app.Branch != "main" || app.HealthCheckPath != "/" ||
		app.MemoryLimitBytes != 256<<20 || app.CPULimitMillicores != 500 {
		t.Errorf("defaults = branch %q, path %q, memory %d, cpu %d; want main, /, 268435456, 500",
			app.Branch, app.HealthCheckPath, app.MemoryLimitBytes, app.CPULimitMillicores)
	}

	custom, err := s.CreateApp(t.Context(), NewApp{
		Name: "custom", RepoURL: "https://github.com/example/custom", Hostname: "custom.example.test", Port: 3000,
		Branch: "release", HealthCheckPath: "/healthz", MemoryLimitBytes: 64 << 20, CPULimitMillicores: 250,
	})
	if err != nil {
		t.Fatal(err)
	}
	if custom.Branch != "release" || custom.HealthCheckPath != "/healthz" ||
		custom.MemoryLimitBytes != 64<<20 || custom.CPULimitMillicores != 250 {
		t.Errorf("overrides not kept: %+v", custom)
	}

	got, err := s.App(t.Context(), app.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Times compared with Equal: the same instant can carry a different
	// location or monotonic reading.
	gotTime, wantTime := got.CreatedAt, app.CreatedAt
	got.CreatedAt, app.CreatedAt = wantTime, wantTime
	if got != app || !gotTime.Equal(wantTime) {
		t.Errorf("App(%d) = %+v, want %+v", app.ID, got, app)
	}
	apps, err := s.Apps(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 || apps[0].Name != "custom" || apps[1].Name != "hello" {
		t.Errorf("Apps = %v, want custom then hello (by name)", apps)
	}
}

func TestAppErrors(t *testing.T) {
	s := testStore(t)
	createTestApp(t, s, "hello")

	tests := []struct {
		name string
		app  NewApp
		want error
	}{
		{"name taken", NewApp{Name: "hello", RepoURL: "r", Hostname: "other.example.test", Port: 1}, ErrConflict},
		{"hostname taken", NewApp{Name: "other", RepoURL: "r", Hostname: "hello.example.test", Port: 1}, ErrConflict},
		{"bad name", NewApp{Name: "Hello", RepoURL: "r", Hostname: "x.example.test", Port: 1}, ErrInvalid},
		{"bad port", NewApp{Name: "x", RepoURL: "r", Hostname: "x.example.test", Port: 70000}, ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.CreateApp(t.Context(), tt.app); !errors.Is(err, tt.want) {
				t.Errorf("CreateApp error = %v, want %v", err, tt.want)
			}
		})
	}

	if _, err := s.App(t.Context(), 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("App(missing) error = %v, want ErrNotFound", err)
	}
}

func TestEnvVars(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	ctx := t.Context()

	for _, kv := range [][2]string{{"TOKEN", "first"}, {"API_URL", "https://x"}, {"TOKEN", "second"}} {
		if err := s.SetEnvVar(ctx, app.ID, kv[0], kv[1]); err != nil {
			t.Fatalf("SetEnvVar(%s): %v", kv[0], err)
		}
	}

	keys, err := s.EnvVarKeys(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys, []string{"API_URL", "TOKEN"}) {
		t.Errorf("EnvVarKeys = %v, want [API_URL TOKEN]", keys)
	}
	vars, err := s.EnvVars(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) != 2 || vars["TOKEN"] != "second" || vars["API_URL"] != "https://x" {
		t.Errorf("EnvVars = %v, want TOKEN=second (overwritten) and API_URL", vars)
	}

	// Stored encrypted: the plaintext appears nowhere in the table.
	var stored []byte
	if err := s.pool.QueryRow(ctx, "SELECT encrypted_value FROM app_env_vars WHERE key = 'TOKEN'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("second")) {
		t.Error("TOKEN's value is stored in plaintext")
	}

	if err := s.DeleteEnvVar(ctx, app.ID, "API_URL"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteEnvVar(ctx, app.ID, "API_URL"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting it again: error = %v, want ErrNotFound", err)
	}
	if err := s.SetEnvVar(ctx, app.ID, "BAD-NAME", "x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("SetEnvVar(BAD-NAME) error = %v, want ErrInvalid", err)
	}
	if err := s.SetEnvVar(ctx, 999999, "TOKEN", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetEnvVar(missing app) error = %v, want ErrNotFound", err)
	}
}

func TestEnvVarCopiedToAnotherAppDoesNotDecrypt(t *testing.T) {
	s := testStore(t)
	victim := createTestApp(t, s, "victim")
	attacker := createTestApp(t, s, "attacker")
	ctx := t.Context()

	if err := s.SetEnvVar(ctx, victim.ID, "TOKEN", "victims-secret"); err != nil {
		t.Fatal(err)
	}
	// Someone with write access to the database (but not the key) copies the
	// victim's encrypted value into another app's row.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO app_env_vars (app_id, key, encrypted_value)
		SELECT $1, key, encrypted_value FROM app_env_vars WHERE app_id = $2`,
		attacker.ID, victim.ID); err != nil {
		t.Fatal(err)
	}

	vars, err := s.EnvVars(ctx, attacker.ID)
	if !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("EnvVars(attacker) = %v, %v; want ErrDecrypt", vars, err)
	}
	if strings.Contains(err.Error(), "victims-secret") {
		t.Errorf("error %q contains the value", err)
	}
}

func TestDeploymentLifecycle(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	ctx := t.Context()

	// The first deploy goes all the way through.
	first, err := s.CreateDeployment(ctx, app.ID, sha1)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != StatusQueued || first.StartedAt != nil {
		t.Errorf("new deployment = %+v, want queued and not started", first)
	}
	mustDo(t, s.MarkBuilding(ctx, first.ID))
	mustDo(t, s.MarkStarting(ctx, first.ID, "hello:"+sha1))
	mustDo(t, s.MarkHealthy(ctx, first.ID))

	current, err := s.CurrentDeployment(ctx, app.ID)
	if err != nil || current.ID != first.ID || current.ImageTag != "hello:"+sha1 ||
		current.StartedAt == nil || current.FinishedAt == nil {
		t.Fatalf("CurrentDeployment = %+v, %v; want the first, with image and timestamps", current, err)
	}

	// While a second is in progress, a third is refused.
	second, err := s.CreateDeployment(ctx, app.ID, sha2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDeployment(ctx, app.ID, sha3); !errors.Is(err, ErrDeployInProgress) {
		t.Errorf("CreateDeployment while one is in progress: error = %v, want ErrDeployInProgress", err)
	}

	// The second becoming healthy supersedes the first.
	mustDo(t, s.MarkBuilding(ctx, second.ID))
	mustDo(t, s.MarkStarting(ctx, second.ID, "hello:"+sha2))
	mustDo(t, s.MarkHealthy(ctx, second.ID))
	assertStatus(t, s, first.ID, StatusSuperseded)
	assertStatus(t, s, second.ID, StatusHealthy)

	// Only the live deployment is routed; an app with none isn't listed.
	createTestApp(t, s, "never-deployed")
	live, err := s.LiveApps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].AppName != "hello" || live[0].DeploymentID != second.ID ||
		live[0].Hostname != "hello.example.test" || live[0].Port != 8080 {
		t.Errorf("LiveApps = %+v, want only hello's second deployment", live)
	}

	// A failed third leaves the second live, and frees the app for a fourth.
	third, err := s.CreateDeployment(ctx, app.ID, sha3)
	if err != nil {
		t.Fatal(err)
	}
	mustDo(t, s.MarkBuilding(ctx, third.ID))
	mustDo(t, s.MarkFailed(ctx, third.ID, "build failed: no Dockerfile"))
	got, _ := s.Deployment(ctx, third.ID)
	if got.Status != StatusFailed || got.Error != "build failed: no Dockerfile" || got.FinishedAt == nil {
		t.Errorf("failed deployment = %+v", got)
	}
	assertStatus(t, s, second.ID, StatusHealthy)
	if _, err := s.CreateDeployment(ctx, app.ID, sha3); err != nil {
		t.Errorf("CreateDeployment after a failure: %v", err)
	}

	list, err := s.Deployments(ctx, app.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 || list[0].ID < list[3].ID {
		t.Errorf("Deployments returned %d, want 4, newest first", len(list))
	}
}

func TestDeploymentStatusOnlyMovesForward(t *testing.T) {
	s := testStore(t)
	app := createTestApp(t, s, "hello")
	ctx := t.Context()
	d, err := s.CreateDeployment(ctx, app.ID, sha1)
	if err != nil {
		t.Fatal(err)
	}

	// Skipping stages is refused, and leaves the deployment as it was.
	if err := s.MarkHealthy(ctx, d.ID); !errors.Is(err, ErrWrongStatus) {
		t.Errorf("MarkHealthy(queued) error = %v, want ErrWrongStatus", err)
	}
	if err := s.MarkStarting(ctx, d.ID, "img"); !errors.Is(err, ErrWrongStatus) {
		t.Errorf("MarkStarting(queued) error = %v, want ErrWrongStatus", err)
	}
	assertStatus(t, s, d.ID, StatusQueued)

	// Nothing comes back from failed.
	mustDo(t, s.MarkFailed(ctx, d.ID, "cancelled"))
	if err := s.MarkBuilding(ctx, d.ID); !errors.Is(err, ErrWrongStatus) {
		t.Errorf("MarkBuilding(failed) error = %v, want ErrWrongStatus", err)
	}
	if err := s.MarkFailed(ctx, d.ID, "again"); !errors.Is(err, ErrWrongStatus) {
		t.Errorf("MarkFailed(failed) error = %v, want ErrWrongStatus", err)
	}

	if err := s.MarkBuilding(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkBuilding(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateDeployment(ctx, app.ID, "main"); !errors.Is(err, ErrInvalid) {
		t.Errorf("CreateDeployment with a branch name: error = %v, want ErrInvalid", err)
	}
	if _, err := s.CreateDeployment(ctx, 999999, sha1); !errors.Is(err, ErrNotFound) {
		t.Errorf("CreateDeployment(missing app) error = %v, want ErrNotFound", err)
	}
}

func TestFailInterruptedDeployments(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	building := createTestApp(t, s, "building")
	live := createTestApp(t, s, "live")

	b, _ := s.CreateDeployment(ctx, building.ID, sha1)
	mustDo(t, s.MarkBuilding(ctx, b.ID))
	l, _ := s.CreateDeployment(ctx, live.ID, sha1)
	mustDo(t, s.MarkBuilding(ctx, l.ID))
	mustDo(t, s.MarkStarting(ctx, l.ID, "img"))
	mustDo(t, s.MarkHealthy(ctx, l.ID))

	n, err := s.FailInterruptedDeployments(ctx, "interrupted")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("failed %d deployments, want 1 (only the building one)", n)
	}
	assertStatus(t, s, b.ID, StatusFailed)
	assertStatus(t, s, l.ID, StatusHealthy)
	// The app is free to deploy again.
	if _, err := s.CreateDeployment(ctx, building.ID, sha2); err != nil {
		t.Errorf("CreateDeployment after clearing: %v", err)
	}
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertStatus(t *testing.T, s *Store, id int64, want DeploymentStatus) {
	t.Helper()
	d, err := s.Deployment(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != want {
		t.Errorf("deployment %d status = %s, want %s", id, d.Status, want)
	}
}
