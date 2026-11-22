# db-index-maintainer (indexapp)

Rebuilds or reorganizes the indexes of a **SQL Server**, **PostgreSQL** or **Oracle** database, picking
the action from the fragmentation of each index. A command line tool written in Go (the earlier version was
a JavaFX desktop application for SQL Server only).

## Install

```bash
go install github.com/ergulcu/db-index-maintainer/cmd/indexapp@latest
# or build it here:
go build -o indexapp ./cmd/indexapp
```

The drivers are pure Go, so one binary works on Windows, Linux and macOS without client libraries.

## Use

Always start with `--dry-run`: it lists every index with the decision and the SQL, and changes nothing.

```bash
# SQL Server: password from the environment
INDEXAPP_PASSWORD=... indexapp --driver sqlserver --host db1 --user maint --database Shop --dry-run

# PostgreSQL: online (REINDEX CONCURRENTLY), only the "sales" schema
indexapp --driver postgres --dsn "postgres://maint:secret@db2:5432/shop" --schema sales --online

# Oracle: rebuild the primary key indexes of the current schema at 02:00, within a one hour window
indexapp --driver oracle --host db3 --user app --database ORCLPDB1 --password-stdin \
         --kind clustered --mode rebuild --at "2026-11-20 02:00" --window 1h < password.txt
```

| Option | Default | |
|---|---|---|
| `--driver` | | `sqlserver`, `postgres` or `oracle` |
| `--dsn` / `INDEXAPP_DSN` | | full connection string; or `--host --port --user --database` with the password in `INDEXAPP_PASSWORD` or from `--password-stdin` |
| `--schema` | all (Oracle: current schema) | |
| `--kind` | `all` | `clustered` or `nonclustered` |
| `--mode` | `auto` | `auto` decides per index; `rebuild` / `reorganize` do every selected index (the old buttons) |
| `--reorganize-at` / `--rebuild-at` | `5` / `30` | fragmentation thresholds in percent (auto) |
| `--min-pages` | `1000` | smaller indexes are skipped (auto) |
| `--when-unknown` | `rebuild` | what auto does when the fragmentation is unknown: `rebuild` or `skip` |
| `--online` | off | keep the tables usable during rebuilds |
| `--wait` | off | after each index, pause as long as it took (as before) |
| `--at` | now | start at a local time (as the old scheduling fields) |
| `--window` | none | start no new index after this long, e.g. `2h` |
| `--dry-run` | off | print the plan only |

The exit code is 0 when everything went well, 1 when an index failed or did not start, 2 for wrong options.

## What happens on each database

| | SQL Server | PostgreSQL | Oracle |
|---|---|---|---|
| Fragmentation | `sys.dm_db_index_physical_stats` (LIMITED) of the current database | `pgstatindex` leaf fragmentation when the `pgstattuple` extension is installed (B-tree; reads the whole index), unknown otherwise | unknown (measuring would lock the table) |
| "Clustered" | clustered indexes | primary key indexes | primary key indexes |
| Rebuild | `ALTER INDEX ... REBUILD [WITH (ONLINE = ON)]` | `REINDEX INDEX [CONCURRENTLY]` | `ALTER INDEX ... REBUILD [ONLINE]`, partition by partition for partitioned indexes |
| Reorganize | `ALTER INDEX ... REORGANIZE` | not available: the 5–30 % range is skipped | `ALTER INDEX ... COALESCE` |
| Online | Enterprise / Developer edition, Azure SQL | always available (PostgreSQL 12+) | Enterprise Edition |

Only user tables are touched: views, heaps, system catalogs, disabled, invalid and temporary indexes are left out.
A failure (for example `ONLINE` on Standard edition) is reported and the next index goes on.

## Fixed since the Java version

- Fragmentation was read from **every database on the server** and joined to the indexes of the current one;
  as object ids repeat across databases, wrong indexes could be picked. Now only the current database is read.
- "Rebuild all indexes" also ran `ALTER INDEX ALL` on views, which fails. Views are left out now.
- Every index with any fragmentation above 0 % was rebuilt; now the thresholds decide, and small indexes are skipped.
- The password field showed the password in clear text; the password now comes from the environment or stdin.

## Tests

```bash
go test ./...                       # unit tests
go test -tags integration ./...     # against real SQL Server 2022, PostgreSQL 18 and Oracle 23 Free (Docker)
```

The integration tests fragment real indexes, run the tool's plan and check the result (the fragmentation
dropped, the PostgreSQL relfilenode changed, the Oracle partitions were rebuilt). The SQL Server and Oracle
images are large, so the first run takes a while. On Docker Desktop for Windows set
`TESTCONTAINERS_RYUK_DISABLED=true` if the Testcontainers reaper fails to start.

The repository was called `SqlServerIndexApp` until it learned PostgreSQL and Oracle; GitHub forwards the old name.

## License

Copyright 2018 Mehmet ERGULCU. Licensed under the [Apache License 2.0](LICENSE). Earlier versions were
published under the GPL v3.
