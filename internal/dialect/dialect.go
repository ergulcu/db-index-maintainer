// Copyright 2018 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

// Package dialect knows, for each database, how to list indexes with their fragmentation and how to
// rebuild or reorganize them.
package dialect

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Kind separates the "clustered" indexes from the others. SQL Server has real clustered indexes;
// in PostgreSQL and Oracle the primary key indexes play that part (see the README).
type Kind int

const (
	NonClustered Kind = iota
	Clustered
)

func (k Kind) String() string {
	if k == Clustered {
		return "clustered"
	}
	return "nonclustered"
}

// Action is what to do with an index.
type Action int

const (
	Skip Action = iota
	Reorganize
	Rebuild
)

func (a Action) String() string {
	switch a {
	case Reorganize:
		return "reorganize"
	case Rebuild:
		return "rebuild"
	default:
		return "skip"
	}
}

// Index is one index found in the database.
type Index struct {
	Schema string
	Table  string
	Name   string
	Kind   Kind
	// Fragmentation is in percent; nil when the database cannot tell cheaply.
	Fragmentation *float64
	// Pages is the size in pages or blocks; -1 when unknown.
	Pages int64
	// Partitions are rebuilt one by one where the database needs that (Oracle).
	Partitions []string
}

// FullName is schema.table.index.
func (i Index) FullName() string { return i.Schema + "." + i.Table + "." + i.Name }

// Dialect is one database product.
type Dialect interface {
	// Name is the value of the --driver flag.
	Name() string
	// DriverName is the database/sql driver.
	DriverName() string
	// DSN builds a connection string from the separate connection flags.
	DSN(host string, port int, user, password, database string) string
	// ListIndexes returns the user indexes of the current database, of one schema when schema is not empty.
	ListIndexes(ctx context.Context, db *sql.DB, schema string) ([]Index, error)
	// Statements returns the SQL that performs the action; more than one for partitioned indexes.
	Statements(idx Index, action Action, online bool) ([]string, error)
	// CanReorganize reports whether the database has a lighter alternative to a rebuild.
	CanReorganize() bool
}

// ByName returns the dialect for the --driver flag.
func ByName(name string) (Dialect, error) {
	switch strings.ToLower(name) {
	case "sqlserver", "mssql":
		return SQLServer{}, nil
	case "postgres", "postgresql", "pgx":
		return Postgres{}, nil
	case "oracle":
		return Oracle{}, nil
	}
	return nil, fmt.Errorf("unknown driver %q: use sqlserver, postgres or oracle", name)
}

// Names lists the supported --driver values.
func Names() []string { return []string{"sqlserver", "postgres", "oracle"} }

// quote wraps an identifier in the given quote characters, doubling the closing one inside it.
func quote(name, open, close string) string {
	return open + strings.ReplaceAll(name, close, close+close) + close
}

func float(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}
