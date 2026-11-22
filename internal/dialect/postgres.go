// Copyright 2018 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

package dialect

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
)

// Postgres is PostgreSQL 12 or newer (REINDEX CONCURRENTLY).
//
// PostgreSQL has no clustered indexes in the SQL Server sense; the primary key indexes count as "clustered".
// It also has nothing like REORGANIZE, so only rebuilds (REINDEX) are done. Fragmentation is known when the
// pgstattuple extension is installed (leaf_fragmentation of pgstatindex, B-tree indexes only); note that
// measuring reads the whole index.
type Postgres struct{}

func (Postgres) Name() string        { return "postgres" }
func (Postgres) DriverName() string  { return "pgx" }
func (Postgres) CanReorganize() bool { return false }

func (Postgres) DSN(host string, port int, user, password, database string) string {
	if port == 0 {
		port = 5432
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: host + ":" + strconv.Itoa(port),
		Path: "/" + database}
	q := url.Values{}
	q.Set("application_name", "indexapp")
	u.RawQuery = q.Encode()
	return u.String()
}

const postgresIndexes = `
SELECT n.nspname, t.relname, i.relname, ix.indisprimary, am.amname, i.relpages
FROM pg_index ix
JOIN pg_class i ON i.oid = ix.indexrelid
JOIN pg_class t ON t.oid = ix.indrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
JOIN pg_am am ON am.oid = i.relam
WHERE t.relkind IN ('r', 'm')
  AND i.relkind = 'i'           -- partitioned parent indexes ('I') have no storage of their own
  AND ix.indisvalid
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND n.nspname NOT LIKE 'pg\_toast%'
  AND ($1 = '' OR n.nspname = $1)
ORDER BY n.nspname, t.relname, i.relname`

func (Postgres) ListIndexes(ctx context.Context, db *sql.DB, schema string) ([]Index, error) {
	var hasPgstattuple bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pgstattuple')").
		Scan(&hasPgstattuple); err != nil {
		return nil, fmt.Errorf("check pgstattuple: %w", err)
	}

	rows, err := db.QueryContext(ctx, postgresIndexes, schema)
	if err != nil {
		return nil, fmt.Errorf("list indexes: %w", err)
	}
	type found struct {
		Index
		method string
	}
	var list []found
	for rows.Next() {
		var f found
		var primary bool
		if err := rows.Scan(&f.Schema, &f.Table, &f.Name, &primary, &f.method, &f.Pages); err != nil {
			rows.Close()
			return nil, err
		}
		if primary {
			f.Kind = Clustered
		}
		list = append(list, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	indexes := make([]Index, 0, len(list))
	for _, f := range list {
		if hasPgstattuple && f.method == "btree" {
			var fragmentation sql.NullFloat64
			name := quote(f.Schema, `"`, `"`) + "." + quote(f.Name, `"`, `"`)
			err := db.QueryRowContext(ctx, "SELECT leaf_fragmentation FROM pgstatindex($1::regclass)", name).
				Scan(&fragmentation)
			if err != nil {
				return nil, fmt.Errorf("measure %s: %w", name, err)
			}
			// pgstatindex reports NaN for indexes without leaf pages.
			if fragmentation.Valid && fragmentation.Float64 == fragmentation.Float64 {
				f.Fragmentation = float(fragmentation)
			}
		}
		indexes = append(indexes, f.Index)
	}
	return indexes, nil
}

func (Postgres) Statements(idx Index, action Action, online bool) ([]string, error) {
	if action == Skip {
		return nil, nil
	}
	if action == Reorganize {
		return nil, fmt.Errorf("PostgreSQL has no reorganize")
	}
	name := quote(idx.Schema, `"`, `"`) + "." + quote(idx.Name, `"`, `"`)
	if online {
		return []string{"REINDEX INDEX CONCURRENTLY " + name}, nil
	}
	return []string{"REINDEX INDEX " + name}, nil
}
