package internal

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentDialect(t *testing.T) {
	t.Setenv("COLD_CLI_DATABASE_URL", "")
	if got := CurrentDialect(); got != DialectSQLite {
		t.Fatalf("expected sqlite dialect, got %q", got)
	}

	t.Setenv("COLD_CLI_DATABASE_URL", "postgres://user:secret@localhost:5432/cold_cli")
	if got := CurrentDialect(); got != DialectPostgres {
		t.Fatalf("expected postgres dialect, got %q", got)
	}
}

func TestOpenStore_SQLiteByDefault(t *testing.T) {
	t.Setenv("COLD_CLI_DATABASE_URL", "")

	var driverName, dataSourceName string
	sqliteBootstrapCalled := false
	postgresBootstrapCalled := false

	store, err := openStore(storeOpenConfig{
		sqlitePath: ":memory:",
		openDB: func(driver, dsn string) (*sql.DB, error) {
			driverName = driver
			dataSourceName = dsn
			return sql.Open("sqlite", sqliteDSN(":memory:"))
		},
		bootstrapSQLite: func(db *sql.DB) error {
			sqliteBootstrapCalled = true
			return nil
		},
		bootstrapPostgres: func(db *sql.DB) error {
			postgresBootstrapCalled = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("opening sqlite store: %v", err)
	}
	defer store.Close()

	if store.Dialect != DialectSQLite {
		t.Fatalf("expected sqlite dialect, got %q", store.Dialect)
	}
	if driverName != "sqlite" {
		t.Fatalf("expected sqlite driver, got %q", driverName)
	}
	if !strings.Contains(dataSourceName, "busy_timeout") {
		t.Fatalf("expected sqlite DSN pragmas, got %q", dataSourceName)
	}
	if !sqliteBootstrapCalled {
		t.Fatal("expected sqlite bootstrap to run")
	}
	if postgresBootstrapCalled {
		t.Fatal("did not expect postgres bootstrap to run")
	}
}

func TestOpenStore_PostgresFromEnv(t *testing.T) {
	t.Setenv("COLD_CLI_DATABASE_URL", "postgres://user:secret@db.example.com:5432/cold_cli")

	var driverName, dataSourceName string
	sqliteBootstrapCalled := false
	postgresBootstrapCalled := false

	cfg := storeOpenConfigFromEnv()
	cfg.openDB = func(driver, dsn string) (*sql.DB, error) {
		driverName = driver
		dataSourceName = dsn
		return sql.Open("sqlite", sqliteDSN(":memory:"))
	}
	cfg.bootstrapSQLite = func(db *sql.DB) error {
		sqliteBootstrapCalled = true
		return nil
	}
	cfg.bootstrapPostgres = func(db *sql.DB) error {
		postgresBootstrapCalled = true
		return nil
	}

	store, err := openStore(cfg)
	if err != nil {
		t.Fatalf("opening postgres store: %v", err)
	}
	defer store.Close()

	if store.Dialect != DialectPostgres {
		t.Fatalf("expected postgres dialect, got %q", store.Dialect)
	}
	if driverName != "pgx" {
		t.Fatalf("expected pgx driver, got %q", driverName)
	}
	if dataSourceName != "postgres://user:secret@db.example.com:5432/cold_cli" {
		t.Fatalf("unexpected postgres DSN: %q", dataSourceName)
	}
	if sqliteBootstrapCalled {
		t.Fatal("did not expect sqlite bootstrap to run")
	}
	if !postgresBootstrapCalled {
		t.Fatal("expected postgres bootstrap to run")
	}
	if store.DisplayTarget() != "postgres://user:xxxxx@db.example.com:5432/cold_cli" {
		t.Fatalf("unexpected display target: %q", store.DisplayTarget())
	}
}

func TestStoreAcquireTickLock_SQLite(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "tick.lock")

	storeA := &Store{
		Dialect:               DialectSQLite,
		tickLockPath:          lockPath,
		acquireSQLiteTickLock: acquireSQLiteTickLock,
	}
	storeB := &Store{
		Dialect:               DialectSQLite,
		tickLockPath:          lockPath,
		acquireSQLiteTickLock: acquireSQLiteTickLock,
	}

	lockA, err := storeA.AcquireTickLock(context.Background())
	if err != nil {
		t.Fatalf("acquiring first sqlite tick lock: %v", err)
	}

	if _, err := storeB.AcquireTickLock(context.Background()); err == nil {
		t.Fatal("expected second sqlite tick lock acquisition to fail")
	}

	if err := lockA.Close(); err != nil {
		t.Fatalf("releasing sqlite tick lock: %v", err)
	}

	lockB, err := storeB.AcquireTickLock(context.Background())
	if err != nil {
		t.Fatalf("re-acquiring sqlite tick lock: %v", err)
	}
	defer lockB.Close()
}

func TestStoreAcquireTickLock_PostgresSelection(t *testing.T) {
	called := false
	lock := &fakeTickLock{}
	store := &Store{
		DB:          new(sql.DB),
		Dialect:     DialectPostgres,
		tickLockKey: 12345,
		acquirePostgresTickLock: func(ctx context.Context, db *sql.DB, key int64) (TickLock, error) {
			called = true
			if db == nil {
				t.Fatal("expected db handle")
			}
			if key != 12345 {
				t.Fatalf("expected tick lock key 12345, got %d", key)
			}
			return lock, nil
		},
	}

	got, err := store.AcquireTickLock(context.Background())
	if err != nil {
		t.Fatalf("acquiring postgres tick lock: %v", err)
	}
	if !called {
		t.Fatal("expected postgres tick lock path to be used")
	}
	if got != lock {
		t.Fatal("expected injected tick lock to be returned")
	}
}

func TestAcquirePostgresTickLockWithLocker(t *testing.T) {
	locker := &fakePostgresAdvisoryLocker{tryLockResult: true}

	lock, err := acquirePostgresTickLockWithLocker(context.Background(), locker, 42)
	if err != nil {
		t.Fatalf("acquiring advisory lock: %v", err)
	}

	if !locker.tryLockCalled {
		t.Fatal("expected advisory TryLock to be called")
	}

	if err := lock.Close(); err != nil {
		t.Fatalf("closing advisory lock: %v", err)
	}
	if !locker.unlockCalled {
		t.Fatal("expected advisory Unlock to be called")
	}
	if !locker.closeCalled {
		t.Fatal("expected advisory connection close to be called")
	}
}

type fakeTickLock struct {
	closed bool
}

func (l *fakeTickLock) Close() error {
	l.closed = true
	return nil
}

type fakePostgresAdvisoryLocker struct {
	tryLockCalled bool
	tryLockResult bool
	unlockCalled  bool
	closeCalled   bool
}

func (l *fakePostgresAdvisoryLocker) TryLock(ctx context.Context, key int64) (bool, error) {
	l.tryLockCalled = true
	return l.tryLockResult, nil
}

func (l *fakePostgresAdvisoryLocker) Unlock(ctx context.Context, key int64) error {
	l.unlockCalled = true
	return nil
}

func (l *fakePostgresAdvisoryLocker) Close() error {
	l.closeCalled = true
	return nil
}
