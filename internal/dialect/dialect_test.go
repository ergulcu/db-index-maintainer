// Copyright 2018 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

package dialect

import (
	"reflect"
	"strings"
	"testing"
)

func TestByName(t *testing.T) {
	for name, want := range map[string]string{"sqlserver": "sqlserver", "MSSQL": "sqlserver", "postgres": "postgres",
		"postgresql": "postgres", "oracle": "oracle"} {
		d, err := ByName(name)
		if err != nil || d.Name() != want {
			t.Errorf("ByName(%q) = %v, %v", name, d, err)
		}
	}
	if _, err := ByName("db2"); err == nil {
		t.Error("db2 is not supported")
	}
}

func statements(t *testing.T, d Dialect, idx Index, action Action, online bool) []string {
	t.Helper()
	s, err := d.Statements(idx, action, online)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSQLServerStatements(t *testing.T) {
	idx := Index{Schema: "sales", Table: "order items", Name: "IX_odd]name"}
	d := SQLServer{}
	cases := []struct {
		action Action
		online bool
		want   string
	}{
		{Rebuild, false, "ALTER INDEX [IX_odd]]name] ON [sales].[order items] REBUILD"},
		{Rebuild, true, "ALTER INDEX [IX_odd]]name] ON [sales].[order items] REBUILD WITH (ONLINE = ON)"},
		{Reorganize, true, "ALTER INDEX [IX_odd]]name] ON [sales].[order items] REORGANIZE"},
	}
	for _, c := range cases {
		got := statements(t, d, idx, c.action, c.online)
		if !reflect.DeepEqual(got, []string{c.want}) {
			t.Errorf("%s online=%v: %q, want %q", c.action, c.online, got, c.want)
		}
	}
	if got := statements(t, d, idx, Skip, false); len(got) != 0 {
		t.Errorf("skip produced %q", got)
	}
}

func TestPostgresStatements(t *testing.T) {
	idx := Index{Schema: "public", Table: "orders", Name: `orders"pkey`}
	d := Postgres{}
	if got := statements(t, d, idx, Rebuild, true); got[0] != `REINDEX INDEX CONCURRENTLY "public"."orders""pkey"` {
		t.Errorf("online = %q", got)
	}
	if got := statements(t, d, idx, Rebuild, false); got[0] != `REINDEX INDEX "public"."orders""pkey"` {
		t.Errorf("offline = %q", got)
	}
	if _, err := d.Statements(idx, Reorganize, false); err == nil {
		t.Error("PostgreSQL has no reorganize")
	}
}

func TestOracleStatements(t *testing.T) {
	d := Oracle{}
	idx := Index{Schema: "APP", Table: "ORDERS", Name: "ORDERS_IX"}
	if got := statements(t, d, idx, Rebuild, true); got[0] != `ALTER INDEX "APP"."ORDERS_IX" REBUILD ONLINE` {
		t.Errorf("rebuild = %q", got)
	}
	if got := statements(t, d, idx, Reorganize, true); got[0] != `ALTER INDEX "APP"."ORDERS_IX" COALESCE` {
		t.Errorf("reorganize = %q", got)
	}
	idx.Partitions = []string{"P2025", "P2026"}
	want := []string{`ALTER INDEX "APP"."ORDERS_IX" REBUILD PARTITION "P2025"`,
		`ALTER INDEX "APP"."ORDERS_IX" REBUILD PARTITION "P2026"`}
	if got := statements(t, d, idx, Rebuild, false); !reflect.DeepEqual(got, want) {
		t.Errorf("partitions = %q", got)
	}
	if got := statements(t, d, idx, Reorganize, false); got[1] != `ALTER INDEX "APP"."ORDERS_IX" MODIFY PARTITION "P2026" COALESCE` {
		t.Errorf("partition coalesce = %q", got)
	}
}

func TestDSN(t *testing.T) {
	if got := (SQLServer{}).DSN("db.local", 0, "sa", "p@ss:w/rd", "Shop"); got !=
		"sqlserver://sa:p%40ss%3Aw%2Frd@db.local:1433?app+name=indexapp&database=Shop" {
		t.Errorf("sqlserver = %s", got)
	}
	if got := (Postgres{}).DSN("db.local", 6543, "app", "secret", "shop"); got !=
		"postgres://app:secret@db.local:6543/shop?application_name=indexapp" {
		t.Errorf("postgres = %s", got)
	}
	if got := (Oracle{}).DSN("db.local", 0, "app", "secret", "FREEPDB1"); !strings.HasPrefix(got, "oracle://app:secret@db.local:1521/FREEPDB1") {
		t.Errorf("oracle = %s", got)
	}
}
