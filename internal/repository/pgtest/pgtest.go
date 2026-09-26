// Package pgtest gives each test an isolated PostgreSQL schema.
//
// Tests need a running PostgreSQL server whose connection string is in
// TEST_DATABASE_URL, for example
//
//	postgres://postgres:postgres@localhost:55432/wallet?sslmode=disable
//
// Tests are skipped when it is unset.
package pgtest

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	// Registers the "pgx" database/sql driver.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// EnvVar names the environment variable holding the test server's DSN.
const EnvVar = "TEST_DATABASE_URL"

// DSN creates a fresh, empty schema, drops it when the test ends, and returns
// a connection string whose search_path points at it.
func DSN(t *testing.T) string {
	t.Helper()
	base := os.Getenv(EnvVar)
	if base == "" {
		t.Skipf("%s is not set; skipping PostgreSQL-backed test", EnvVar)
	}

	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	schema := "test_" + hex.EncodeToString(b)

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		defer func() { _ = admin.Close() }()
		if _, err := admin.Exec("DROP SCHEMA " + schema + " CASCADE"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})
	return withSearchPath(t, base, schema)
}

func withSearchPath(t *testing.T, dsn, schema string) string {
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return dsn + " search_path=" + schema
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvVar, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
