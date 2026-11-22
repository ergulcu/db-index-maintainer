// Copyright 2018 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// These tests start real databases with Testcontainers: go test -tags integration ./...
// Each one is skipped when Docker is not available. The SQL Server and Oracle images are large;
// the first run takes a while to pull them.
package dialect_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mssql"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ergulcu/db-index-maintainer/internal/dialect"
	"github.com/ergulcu/db-index-maintainer/internal/maintain"
)

func open(t *testing.T, d dialect.Dialect, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open(d.DriverName(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for {
		if err = db.PingContext(ctx); err == nil {
			return db
		}
		select {
		case <-ctx.Done():
			t.Fatalf("database not reachable: %v", err)
		case <-time.After(2 * time.Second):
		}
	}
}

func exec(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, s := range statements {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func find(indexes []dialect.Index, name string) *dialect.Index {
	for i := range indexes {
		if strings.EqualFold(indexes[i].Name, name) {
			return &indexes[i]
		}
	}
	return nil
}

func runAll(t *testing.T, d dialect.Dialect, db *sql.DB, indexes []dialect.Index, o maintain.Options) maintain.Summary {
	t.Helper()
	steps, err := maintain.Plan(d, indexes, o)
	if err != nil {
		t.Fatal(err)
	}
	return maintain.Run(context.Background(), db, steps, o, func(e maintain.Event) {
		if e.Err != nil {
			t.Errorf("%s: %v (%q)", e.Step.Index.FullName(), e.Err, e.Step.Statements)
		}
		t.Logf("%v %s %s: %s", e.Outcome, e.Step.Action, e.Step.Index.FullName(), e.Step.Reason)
	})
}

func TestPostgres(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	container, err := tcpostgres.Run(ctx, "postgres:18-alpine", tcpostgres.WithDatabase("shop"),
		tcpostgres.WithUsername("maint"), tcpostgres.WithPassword("maint"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })
	dsn, _ := container.ConnectionString(ctx, "sslmode=disable")
	d := dialect.Postgres{}
	db := open(t, d, dsn)

	exec(t, db,
		"CREATE EXTENSION pgstattuple",
		"CREATE SCHEMA sales",
		"CREATE TABLE sales.orders (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), note text)",
		"CREATE INDEX orders_note ON sales.orders (note)",
		"INSERT INTO sales.orders (note) SELECT md5(random()::text) FROM generate_series(1, 30000)",
		"DELETE FROM sales.orders WHERE random() < 0.5",
		"CREATE VIEW sales.order_view AS SELECT * FROM sales.orders",
		"CREATE TABLE public.other (id int PRIMARY KEY)",
	)

	indexes, err := d.ListIndexes(ctx, db, "sales")
	if err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 2 {
		t.Fatalf("indexes of sales = %+v", indexes)
	}
	pk, note := find(indexes, "orders_pkey"), find(indexes, "orders_note")
	if pk == nil || note == nil || pk.Kind != dialect.Clustered || note.Kind != dialect.NonClustered {
		t.Fatalf("indexes = %+v", indexes)
	}
	if pk.Fragmentation == nil || note.Fragmentation == nil || pk.Pages <= 0 {
		t.Fatalf("fragmentation not measured: %+v", indexes)
	}

	before := relfilenodes(t, db)
	sum := runAll(t, d, db, indexes, maintain.Options{Mode: maintain.RebuildAll, Online: true})
	if sum.Rebuilt != 2 || sum.Failed != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	after := relfilenodes(t, db)
	for name, node := range before {
		if after[name] == node {
			t.Errorf("%s was not rebuilt (relfilenode %d unchanged)", name, node)
		}
	}
}

func relfilenodes(t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	rows, err := db.Query(`SELECT relname, relfilenode FROM pg_class WHERE relname IN ('orders_pkey', 'orders_note')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	m := map[string]int64{}
	for rows.Next() {
		var name string
		var node int64
		rows.Scan(&name, &node) //nolint:errcheck
		m[name] = node
	}
	return m
}

func TestSQLServer(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	// New on every run; SQL Server demands upper case, lower case, a digit and a symbol.
	password := "Aa1!" + rand.Text()
	container, err := mssql.Run(ctx, "mcr.microsoft.com/mssql/server:2022-latest", mssql.WithAcceptEULA(),
		mssql.WithPassword(password))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })
	host, _ := container.Host(ctx)
	port, _ := container.MappedPort(ctx, "1433/tcp")
	d := dialect.SQLServer{}
	master := open(t, d, d.DSN(host, int(port.Num()), "sa", password, "master"))
	exec(t, master, "CREATE DATABASE Shop", "CREATE DATABASE Other")

	// A fragmented index in another database must not show up: the Java version scanned all databases.
	other := open(t, d, d.DSN(host, int(port.Num()), "sa", password, "Other"))
	exec(t, other,
		"CREATE TABLE dbo.elsewhere (id UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID() CONSTRAINT PK_elsewhere PRIMARY KEY CLUSTERED, pad CHAR(400) NOT NULL DEFAULT 'x')",
		"INSERT INTO dbo.elsewhere (pad) SELECT TOP (5000) 'x' FROM sys.all_objects a CROSS JOIN sys.all_objects b")

	db := open(t, d, d.DSN(host, int(port.Num()), "sa", password, "Shop"))
	exec(t, db,
		"CREATE SCHEMA sales",
		// Random GUID keys split pages all the time, which fragments the clustered index.
		"CREATE TABLE sales.orders (id UNIQUEIDENTIFIER NOT NULL DEFAULT NEWID() CONSTRAINT PK_orders PRIMARY KEY CLUSTERED, customer INT NOT NULL, pad CHAR(400) NOT NULL DEFAULT 'x')",
		"CREATE NONCLUSTERED INDEX IX_orders_customer ON sales.orders (customer)",
		// Small batches: one big INSERT ... SELECT would be sorted by key first and cause no page splits.
		`DECLARE @i INT = 0;
		 WHILE @i < 400 BEGIN
		   INSERT INTO sales.orders (customer) SELECT TOP (50) ABS(CHECKSUM(NEWID())) % 1000 FROM sys.all_objects;
		   SET @i += 1;
		 END`,
		"CREATE TABLE sales.heap (id INT)",
		"EXEC('CREATE VIEW sales.order_view AS SELECT id FROM sales.orders')",
	)

	indexes, err := d.ListIndexes(ctx, db, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 2 || find(indexes, "PK_elsewhere") != nil {
		t.Fatalf("indexes = %+v", indexes)
	}
	pk := find(indexes, "PK_orders")
	if pk == nil || pk.Kind != dialect.Clustered || pk.Fragmentation == nil || *pk.Fragmentation < 30 || pk.Pages < 100 {
		t.Fatalf("PK_orders = %+v (fragmentation %v)", pk, deref(pk.Fragmentation))
	}

	// Developer edition supports ONLINE rebuilds.
	sum := runAll(t, d, db, indexes, maintain.Options{Mode: maintain.Auto, ReorganizeAt: 5, RebuildAt: 30,
		MinPages: 10, RebuildUnknown: true, Online: true})
	if sum.Failed != 0 || sum.Rebuilt < 1 {
		t.Fatalf("summary = %+v", sum)
	}
	again, _ := d.ListIndexes(ctx, db, "sales")
	if f := find(again, "PK_orders").Fragmentation; f == nil || *f >= 5 {
		t.Errorf("fragmentation after rebuild = %v", deref(f))
	}
}

func TestOracle(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	// Letters and digits only, so that the shell scripts of the image cope.
	adminPassword, appPassword := "Pw"+rand.Text(), "Pw"+rand.Text()
	container, err := testcontainers.Run(ctx, "gvenzl/oracle-free:23-slim-faststart",
		testcontainers.WithExposedPorts("1521/tcp"),
		testcontainers.WithEnv(map[string]string{"ORACLE_PASSWORD": adminPassword, "APP_USER": "app",
			"APP_USER_PASSWORD": appPassword}),
		testcontainers.WithWaitStrategy(wait.ForLog("DATABASE IS READY TO USE!").WithStartupTimeout(10*time.Minute)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })
	host, _ := container.Host(ctx)
	port, _ := container.MappedPort(ctx, "1521/tcp")
	d := dialect.Oracle{}
	db := open(t, d, d.DSN(host, int(port.Num()), "app", appPassword, "FREEPDB1"))

	exec(t, db,
		"CREATE TABLE orders (id NUMBER CONSTRAINT orders_pk PRIMARY KEY, customer NUMBER, created DATE)",
		"CREATE INDEX orders_customer ON orders (customer)",
		"INSERT INTO orders SELECT level, MOD(level, 97), SYSDATE - level FROM dual CONNECT BY level <= 5000",
		`CREATE TABLE events (id NUMBER, happened DATE)
		   PARTITION BY RANGE (happened) (
		     PARTITION p2025 VALUES LESS THAN (DATE '2026-01-01'),
		     PARTITION pmax VALUES LESS THAN (MAXVALUE))`,
		"CREATE INDEX events_happened ON events (happened) LOCAL",
		"INSERT INTO events SELECT level, DATE '2025-06-01' + level FROM dual CONNECT BY level <= 1000",
		"CREATE VIEW order_view AS SELECT id FROM orders",
	)

	indexes, err := d.ListIndexes(ctx, db, "")
	if err != nil {
		t.Fatal(err)
	}
	pk, customer, events := find(indexes, "ORDERS_PK"), find(indexes, "ORDERS_CUSTOMER"), find(indexes, "EVENTS_HAPPENED")
	if len(indexes) != 3 || pk == nil || customer == nil || events == nil || pk.Kind != dialect.Clustered ||
		customer.Kind != dialect.NonClustered || pk.Fragmentation != nil {
		t.Fatalf("indexes = %+v", indexes)
	}
	if fmt.Sprint(events.Partitions) != "[P2025 PMAX]" {
		t.Fatalf("partitions = %v", events.Partitions)
	}

	sum := runAll(t, d, db, indexes, maintain.Options{Mode: maintain.Auto, RebuildAt: 30, RebuildUnknown: true})
	if sum.Rebuilt != 3 || sum.Failed != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	reorganized := runAll(t, d, db, indexes, maintain.Options{Mode: maintain.ReorganizeAll})
	if reorganized.Reorganized != 3 || reorganized.Failed != 0 {
		t.Fatalf("coalesce summary = %+v", reorganized)
	}
	var unusable int
	if err := db.QueryRow(`SELECT COUNT(*) FROM user_indexes WHERE status = 'UNUSABLE'`).Scan(&unusable); err != nil || unusable != 0 {
		t.Errorf("unusable indexes after the run: %d (%v)", unusable, err)
	}
}

func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}
