// Copyright 2018 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

package dialect

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	goora "github.com/sijms/go-ora/v2" // pure Go driver, registers "oracle"
)

// Oracle is Oracle Database 12c or newer.
//
// Oracle has no cheap fragmentation figure (ANALYZE INDEX ... VALIDATE STRUCTURE locks the table), so the
// fragmentation is unknown and the --when-unknown flag decides. Primary key indexes count as "clustered".
// REORGANIZE is done with COALESCE. Partitioned indexes are rebuilt partition by partition.
type Oracle struct{}

func (Oracle) Name() string        { return "oracle" }
func (Oracle) DriverName() string  { return "oracle" }
func (Oracle) CanReorganize() bool { return true }

// DSN uses the database flag as the service name, e.g. FREEPDB1 or ORCLPDB1.
func (Oracle) DSN(host string, port int, user, password, database string) string {
	if port == 0 {
		port = 1521
	}
	return goora.BuildUrl(host, port, database, user, password, nil)
}

const oracleIndexes = `
SELECT i.owner, i.table_name, i.index_name,
       CASE WHEN EXISTS (SELECT 1 FROM all_constraints c
                         WHERE c.owner = i.owner AND c.index_name = i.index_name AND c.constraint_type = 'P')
            THEN 1 ELSE 0 END,
       NVL(i.leaf_blocks, -1), i.partitioned
FROM all_indexes i
WHERE i.owner = NVL(:1, SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA'))
  AND i.table_type = 'TABLE'
  AND i.index_type IN ('NORMAL', 'NORMAL/REV', 'BITMAP', 'FUNCTION-BASED NORMAL', 'FUNCTION-BASED BITMAP')
  AND i.temporary = 'N'
  AND i.status IN ('VALID', 'N/A')
  AND i.dropped = 'NO'
ORDER BY i.table_name, i.index_name`

const oraclePartitions = `
SELECT partition_name FROM all_ind_partitions
WHERE index_owner = :1 AND index_name = :2
ORDER BY partition_position`

func (Oracle) ListIndexes(ctx context.Context, db *sql.DB, schema string) ([]Index, error) {
	var owner any
	if schema != "" {
		owner = strings.ToUpper(schema)
	}
	rows, err := db.QueryContext(ctx, oracleIndexes, owner)
	if err != nil {
		return nil, fmt.Errorf("list indexes: %w", err)
	}
	type found struct {
		Index
		partitioned bool
	}
	var list []found
	for rows.Next() {
		var f found
		var primary int
		var partitioned string
		if err := rows.Scan(&f.Schema, &f.Table, &f.Name, &primary, &f.Pages, &partitioned); err != nil {
			rows.Close()
			return nil, err
		}
		if primary == 1 {
			f.Kind = Clustered
		}
		f.partitioned = partitioned == "YES"
		list = append(list, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	indexes := make([]Index, 0, len(list))
	for _, f := range list {
		if f.partitioned {
			partitions, err := oracleIndexPartitions(ctx, db, f.Schema, f.Name)
			if err != nil {
				return nil, err
			}
			f.Partitions = partitions
		}
		indexes = append(indexes, f.Index)
	}
	return indexes, nil
}

func oracleIndexPartitions(ctx context.Context, db *sql.DB, owner, index string) ([]string, error) {
	rows, err := db.QueryContext(ctx, oraclePartitions, owner, index)
	if err != nil {
		return nil, fmt.Errorf("list partitions of %s.%s: %w", owner, index, err)
	}
	defer rows.Close()
	var partitions []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		partitions = append(partitions, name)
	}
	return partitions, rows.Err()
}

func (Oracle) Statements(idx Index, action Action, online bool) ([]string, error) {
	if action == Skip {
		return nil, nil
	}
	name := "ALTER INDEX " + quote(idx.Schema, `"`, `"`) + "." + quote(idx.Name, `"`, `"`)
	suffix := ""
	if online && action == Rebuild {
		suffix = " ONLINE"
	}
	if len(idx.Partitions) == 0 {
		if action == Reorganize {
			return []string{name + " COALESCE"}, nil
		}
		return []string{name + " REBUILD" + suffix}, nil
	}
	statements := make([]string, 0, len(idx.Partitions))
	for _, partition := range idx.Partitions {
		p := quote(partition, `"`, `"`)
		if action == Reorganize {
			statements = append(statements, name+" MODIFY PARTITION "+p+" COALESCE")
		} else {
			statements = append(statements, name+" REBUILD PARTITION "+p+suffix)
		}
	}
	return statements, nil
}
