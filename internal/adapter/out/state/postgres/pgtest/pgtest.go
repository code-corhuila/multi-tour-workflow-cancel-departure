// Package pgtest gives the tests of the PostgreSQL store a database of their own, built from the DDL of db/ so the
// tests run against the real constraints. It is imported by tests only.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AppRole is the login user the service connects with in the instance (Annex J, J.4).
const AppRole = "workflow_cancel_departure_app"

// Open creates a throwaway database from TEST_DATABASE_URL, applies the DDL and returns a pool on it. The test is
// skipped when the variable is not set; the CI sets it.
func Open(t *testing.T) *pgxpool.Pool { return open(t, false) }

// OpenAsApp is Open, but every connection of the pool acts as AppRole with the grants of db/03_dcl, so a test sees
// what the service can and cannot do. The role is cluster-wide and is created if the instance lacks it.
func OpenAsApp(t *testing.T) *pgxpool.Pool { return open(t, true) }

func open(t *testing.T, asApp bool) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL is not set in the CI: the tests would pass without testing anything")
		}
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "t_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	// registered right after the creation, so that no later failure leaves the database behind; it runs before admin.Close
	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		_, _ = admin.Exec(ctx, `DROP DATABASE `+name+` WITH (FORCE)`)
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	if pool, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if asApp {
		// the role is cluster-wide: a test binary that races to create it loses harmlessly
		_, _ = admin.Exec(ctx, `DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+AppRole+`') THEN CREATE ROLE `+AppRole+` NOLOGIN; END IF; END $$`)
	}
	apply(t, pool, "01_ddl")
	if !asApp {
		return pool
	}
	apply(t, pool, "03_dcl")
	app, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	app.ConnConfig.Database = name
	app.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SET ROLE `+AppRole+`; SET search_path = workflow_cancel_departure`)
		return err
	}
	appPool, err := pgxpool.NewWithConfig(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(appPool.Close)
	return appPool
}

func apply(t *testing.T, pool *pgxpool.Pool, folder string) {
	t.Helper()
	for _, file := range sqlFiles(t, folder) {
		sql, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), string(sql)); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
	}
}

// sqlFiles lists the SQL of a folder of db/ in the order of the migrations: by subfolder, then by file name.
func sqlFiles(t *testing.T, folder string) []string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..", "..", "..", "..", "..", "..", "db", folder)
	files, err := filepath.Glob(filepath.Join(root, "*", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no SQL found under %s: %v", root, err)
	}
	sort.Strings(files)
	return files
}
