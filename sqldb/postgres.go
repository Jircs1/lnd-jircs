package sqldb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"strings"
	"testing"
	"time"

	postgres_migrate "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file" // Read migrations from files. // nolint:lll
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/stretchr/testify/require"
)

var (
	// DefaultPostgresFixtureLifetime is the default maximum time a Postgres
	// test fixture is being kept alive. After that time the docker
	// container will be terminated forcefully, even if the tests aren't
	// fully executed yet. So this time needs to be chosen correctly to be
	// longer than the longest expected individual test run time.
	DefaultPostgresFixtureLifetime = 10 * time.Minute
)

// PostgresConfig holds the postgres database configuration.
//
//nolint:lll
type PostgresConfig struct {
	Dsn            string        `long:"dsn" description:"Database connection string."`
	Timeout        time.Duration `long:"timeout" description:"Database connection timeout. Set to zero to disable."`
	MaxConnections int           `long:"maxconnections" description:"The maximum number of open connections to the database. Set to zero for unlimited."`
	SkipMigrations bool          `long:"skipmigrations" description:"Skip applying migrations on startup."`
}

func (p *PostgresConfig) Validate() error {
	if p.Dsn == "" {
		return fmt.Errorf("DSN is required")
	}

	// Parse the DSN as a URL.
	_, err := url.Parse(p.Dsn)
	if err != nil {
		return fmt.Errorf("invalid DSN: %w", err)
	}

	return nil
}

// replacePasswordInDSN takes a DSN string and returns it with the password
// replaced by "***".
func replacePasswordInDSN(dsn string) (string, error) {
	// Parse the DSN as a URL
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}

	// Check if the URL has a user info part
	if u.User != nil {
		username := u.User.Username()

		// Reconstruct user info with "***" as password
		userInfo := username + ":***@"

		// Rebuild the DSN with the modified user info
		sanitizeDSN := strings.Replace(
			dsn, u.User.String()+"@", userInfo, 1,
		)

		return sanitizeDSN, nil
	}

	// Return the original DSN if no user info is present
	return dsn, nil
}

// getDatabaseNameFromDSN extracts the database name from a DSN string.
func getDatabaseNameFromDSN(dsn string) (string, error) {
	// Parse the DSN as a URL
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}

	// The database name is the last segment of the path. Trim leading slash
	// and return the last segment.
	return path.Base(u.Path), nil
}

// PostgresStore is a database store implementation that uses a Postgres
// backend.
type PostgresStore struct {
	cfg *PostgresConfig

	*BaseDB
}

// NewPostgresStore creates a new store that is backed by a Postgres database
// backend.
func NewPostgresStore(cfg *PostgresConfig) (*PostgresStore, error) {
	sanitizedDSN, err := replacePasswordInDSN(cfg.Dsn)
	if err != nil {
		return nil, err
	}
	log.Infof("Using SQL database '%s'", sanitizedDSN)

	dbName, err := getDatabaseNameFromDSN(cfg.Dsn)
	if err != nil {
		return nil, err
	}

	rawDB, err := sql.Open("pgx", cfg.Dsn)
	if err != nil {
		return nil, err
	}

	maxConns := defaultMaxConns
	if cfg.MaxConnections > 0 {
		maxConns = cfg.MaxConnections
	}

	rawDB.SetMaxOpenConns(maxConns)
	rawDB.SetMaxIdleConns(maxConns)
	rawDB.SetConnMaxLifetime(connIdleLifetime)

	if !cfg.SkipMigrations {
		// Now that the database is open, populate the database with
		// our set of schemas based on our embedded in-memory file
		// system.
		//
		// First, we'll need to open up a new migration instance for
		// our current target database: Postgres.
		driver, err := postgres_migrate.WithInstance(
			rawDB, &postgres_migrate.Config{},
		)
		if err != nil {
			return nil, err
		}

		postgresFS := newReplacerFS(sqlSchemas, map[string]string{
			"BLOB":                "BYTEA",
			"INTEGER PRIMARY KEY": "SERIAL PRIMARY KEY",
			"BIGINT PRIMARY KEY":  "BIGSERIAL PRIMARY KEY",
			"TIMESTAMP":           "TIMESTAMP WITHOUT TIME ZONE",
		})

		err = applyMigrations(
			postgresFS, driver, "sqlc/migrations", dbName,
		)
		if err != nil {
			return nil, err
		}
	}

	queries := sqlc.New(rawDB)

	return &PostgresStore{
		cfg: cfg,
		BaseDB: &BaseDB{
			DB:      rawDB,
			Queries: queries,
		},
	}, nil
}

// NewTestPostgresDB is a helper function that creates a Postgres database for
// testing using the given fixture.
func NewTestPostgresDB(t *testing.T, fixture *TestPgFixture) *PostgresStore {
	t.Helper()

	// Create random database name.
	randBytes := make([]byte, 8)
	_, err := rand.Read(randBytes)
	if err != nil {
		t.Fatal(err)
	}

	dbName := "test_" + hex.EncodeToString(randBytes)

	t.Logf("Creating new Postgres DB '%s' for testing", dbName)

	_, err = fixture.db.ExecContext(
		context.Background(), "CREATE DATABASE "+dbName,
	)
	if err != nil {
		t.Fatal(err)
	}

	cfg := fixture.GetConfig(dbName)
	store, err := NewPostgresStore(cfg)
	require.NoError(t, err)

	return store
}
