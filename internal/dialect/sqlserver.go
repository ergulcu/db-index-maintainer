// Copyright 2018 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

package dialect

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"

	_ "github.com/microsoft/go-mssqldb" // registers "sqlserver"
)

// SQLServer is Microsoft SQL Server and Azure SQL.
type SQLServer struct{}

func (SQLServer) Name() string        { return "sqlserver" }
func (SQLServer) DriverName() string  { return "sqlserver" }
func (SQLServer) CanReorganize() bool { return true }

func (SQLServer) DSN(host string, port int, user, password, database string) string {
	if port == 0 {
		port = 1433
	}
	u := url.URL{Scheme: "sqlserver", User: url.UserPassword(user, password), Host: host + ":" + strconv.Itoa(port)}
	q := url.Values{}
	if database != "" {
		q.Set("database", database)
	}
	q.Set("app name", "indexapp")
	u.RawQuery = q.Encode()
	return u.String()
}

// The Java version passed NULL as the database to sys.dm_db_index_physical_stats, which scans every database
// of the server, and then joined the result to sys.indexes of the current database only. Object ids repeat
// across databases, so it could pick wrong indexes. DB_ID() limits the scan to the current database.
// LIMITED is the cheapest mode; partitions of one index are summed up.
const sqlServerIndexes = `
SELECT s.name, o.name, i.name, i.type,
       MAX(ps.avg_fragmentation_in_percent), SUM(ps.page_count)
FROM sys.dm_db_index_physical_stats(DB_ID(), NULL, NULL, NULL, 'LIMITED') ps
JOIN sys.indexes i ON i.object_id = ps.object_id AND i.index_id = ps.index_id
JOIN sys.objects o ON o.object_id = i.object_id
JOIN sys.schemas s ON s.schema_id = o.schema_id
WHERE o.type = 'U'
  AND i.type IN (1, 2)          -- clustered and nonclustered rowstore indexes; heaps have no index to rebuild
  AND i.is_disabled = 0
  AND i.is_hypothetical = 0
  AND ps.alloc_unit_type_desc = 'IN_ROW_DATA'
  AND (@p1 = '' OR s.name = @p1)
GROUP BY s.name, o.name, i.name, i.type, i.index_id
ORDER BY s.name, o.name, i.index_id`

func (SQLServer) ListIndexes(ctx context.Context, db *sql.DB, schema string) ([]Index, error) {
	rows, err := db.QueryContext(ctx, sqlServerIndexes, schema)
	if err != nil {
		return nil, fmt.Errorf("list indexes: %w", err)
	}
	defer rows.Close()
	var list []Index
	for rows.Next() {
		var idx Index
		var indexType int
		var fragmentation sql.NullFloat64
		var pages sql.NullInt64
		if err := rows.Scan(&idx.Schema, &idx.Table, &idx.Name, &indexType, &fragmentation, &pages); err != nil {
			return nil, err
		}
		if indexType == 1 {
			idx.Kind = Clustered
		}
		idx.Fragmentation = float(fragmentation)
		idx.Pages = -1
		if pages.Valid {
			idx.Pages = pages.Int64
		}
		list = append(list, idx)
	}
	return list, rows.Err()
}

func (SQLServer) Statements(idx Index, action Action, online bool) ([]string, error) {
	target := "ALTER INDEX " + quote(idx.Name, "[", "]") + " ON " + quote(idx.Schema, "[", "]") + "." +
		quote(idx.Table, "[", "]")
	switch action {
	case Rebuild:
		if online {
			// ONLINE needs Enterprise or Developer edition (or Azure SQL); other editions report an error.
			return []string{target + " REBUILD WITH (ONLINE = ON)"}, nil
		}
		return []string{target + " REBUILD"}, nil
	case Reorganize:
		// REORGANIZE is always online.
		return []string{target + " REORGANIZE"}, nil
	}
	return nil, nil
}
