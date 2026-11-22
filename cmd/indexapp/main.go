// Copyright 2018 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

// Command indexapp rebuilds or reorganizes the indexes of a SQL Server, PostgreSQL or Oracle database.
//
//	indexapp --driver sqlserver --host db1 --user maint --database Shop --dry-run
//	INDEXAPP_PASSWORD=... indexapp --driver postgres --host db2 --user maint --database shop --online
//	indexapp --driver oracle --dsn "oracle://maint:secret@db3:1521/ORCLPDB1" --mode rebuild --kind clustered
//
// Run "indexapp --help" for every option.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ergulcu/db-index-maintainer/internal/dialect"
	"github.com/ergulcu/db-index-maintainer/internal/maintain"
)

var version = "dev"

const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}

// settings is everything the command line says.
type settings struct {
	dialect dialect.Dialect
	dsn     string
	schema  string
	at      time.Time
	options maintain.Options
}

func run(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	s, err := parse(args, getenv, stdin, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		fmt.Fprintln(stderr, "indexapp:", err)
		return exitUsage
	}
	if s == nil { // --version
		fmt.Fprintln(stdout, "indexapp", version)
		return exitOK
	}

	if !s.at.IsZero() {
		wait := time.Until(s.at)
		if wait > 0 {
			fmt.Fprintf(stdout, "waiting until %s (%s)\n", s.at.Format("2006-01-02 15:04"), wait.Round(time.Second))
			select {
			case <-ctx.Done():
				fmt.Fprintln(stderr, "indexapp: interrupted while waiting")
				return exitFailed
			case <-time.After(wait):
			}
		}
	}

	db, err := sql.Open(s.dialect.DriverName(), s.dsn)
	if err != nil {
		fmt.Fprintln(stderr, "indexapp: open:", err)
		return exitFailed
	}
	defer db.Close()
	// One connection: indexes are handled one by one, and SQL Server/Oracle sessions keep their settings.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintln(stderr, "indexapp: cannot connect:", err)
		return exitFailed
	}

	indexes, err := s.dialect.ListIndexes(ctx, db, s.schema)
	if err != nil {
		fmt.Fprintln(stderr, "indexapp:", err)
		return exitFailed
	}
	steps, err := maintain.Plan(s.dialect, indexes, s.options)
	if err != nil {
		fmt.Fprintln(stderr, "indexapp:", err)
		return exitUsage
	}
	fmt.Fprintf(stdout, "%s: %d indexes, %d selected\n", s.dialect.Name(), len(indexes), len(steps))

	summary := maintain.Run(ctx, db, steps, s.options, func(e maintain.Event) { printEvent(stdout, e) })

	fmt.Fprintf(stdout, "done: %d rebuilt, %d reorganized, %d skipped, %d failed, %d not started",
		summary.Rebuilt, summary.Reorganized, summary.Skipped, summary.Failed, summary.NotStarted)
	if s.options.DryRun {
		fmt.Fprintf(stdout, ", %d planned (dry run)", summary.Planned)
	}
	fmt.Fprintln(stdout)
	if summary.Failed > 0 || summary.NotStarted > 0 {
		return exitFailed
	}
	return exitOK
}

func printEvent(w io.Writer, e maintain.Event) {
	name := e.Step.Index.FullName()
	switch e.Outcome {
	case maintain.Skipped:
		fmt.Fprintf(w, "  skip        %s: %s\n", name, e.Step.Reason)
	case maintain.Planned:
		fmt.Fprintf(w, "  %-11s %s: %s\n", e.Step.Action, name, e.Step.Reason)
		for _, statement := range e.Step.Statements {
			fmt.Fprintf(w, "              %s;\n", statement)
		}
	case maintain.Done:
		fmt.Fprintf(w, "  %-11s %s in %s: %s\n", e.Step.Action, name, e.Took.Round(time.Millisecond), e.Step.Reason)
	case maintain.Failed:
		fmt.Fprintf(w, "  FAILED      %s after %s: %v\n", name, e.Took.Round(time.Millisecond), e.Err)
	case maintain.NotStarted:
		fmt.Fprintf(w, "  not started %s (window over or interrupted)\n", name)
	}
}

func parse(args []string, getenv func(string) string, stdin io.Reader, stderr io.Writer) (*settings, error) {
	fs := flag.NewFlagSet("indexapp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: indexapp --driver %s [connection] [options]\n\n",
			strings.Join(dialect.Names(), "|"))
		fmt.Fprintln(stderr, "Connection: --dsn (or INDEXAPP_DSN), or --host/--port/--user/--database with the password in")
		fmt.Fprintln(stderr, "INDEXAPP_PASSWORD or read from stdin with --password-stdin.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	driver := fs.String("driver", "", "database: sqlserver, postgres or oracle")
	dsn := fs.String("dsn", "", "connection string; INDEXAPP_DSN is used when empty")
	host := fs.String("host", "", "database host")
	port := fs.Int("port", 0, "port (1433, 5432 or 1521 when 0)")
	user := fs.String("user", "", "user name")
	database := fs.String("database", "", "database name (the service name for Oracle)")
	passwordStdin := fs.Bool("password-stdin", false, "read the password from the first line of stdin")
	schema := fs.String("schema", "", "only this schema (Oracle: owner; default the current schema)")
	kind := fs.String("kind", "all", "all, clustered or nonclustered (primary keys count as clustered on PostgreSQL and Oracle)")
	mode := fs.String("mode", "auto", "auto (by fragmentation), rebuild (every index) or reorganize (every index)")
	reorganizeAt := fs.Float64("reorganize-at", 5, "auto mode: reorganize from this fragmentation percent")
	rebuildAt := fs.Float64("rebuild-at", 30, "auto mode: rebuild from this fragmentation percent")
	minPages := fs.Int64("min-pages", 1000, "auto mode: skip indexes with fewer pages")
	whenUnknown := fs.String("when-unknown", "rebuild", "auto mode, fragmentation unknown (Oracle; PostgreSQL without pgstattuple): rebuild or skip")
	online := fs.Bool("online", false, "keep tables usable while rebuilding (SQL Server Enterprise, REINDEX CONCURRENTLY, Oracle ONLINE)")
	wait := fs.Bool("wait", false, "after each index, pause as long as it took")
	window := fs.Duration("window", 0, "start no new index after this long, e.g. 2h (0: no limit)")
	at := fs.String("at", "", `start at this local time, "2006-01-02 15:04"`)
	dryRun := fs.Bool("dry-run", false, "only print what would be done")
	showVersion := fs.Bool("version", false, "print the version")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *showVersion {
		return nil, nil
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	s := &settings{schema: *schema}
	var err error
	if s.dialect, err = dialect.ByName(*driver); err != nil {
		return nil, err
	}

	s.dsn = *dsn
	if s.dsn == "" {
		s.dsn = getenv("INDEXAPP_DSN")
	}
	if s.dsn == "" {
		if *host == "" || *user == "" {
			return nil, errors.New("give --dsn, or --host and --user")
		}
		password := getenv("INDEXAPP_PASSWORD")
		if *passwordStdin {
			line, err := bufio.NewReader(stdin).ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("read password: %w", err)
			}
			password = strings.TrimRight(line, "\r\n")
		}
		s.dsn = s.dialect.DSN(*host, *port, *user, password, *database)
	}

	o := maintain.Options{ReorganizeAt: *reorganizeAt, RebuildAt: *rebuildAt, MinPages: *minPages,
		Online: *online, Wait: *wait, Window: *window, DryRun: *dryRun}
	switch *kind {
	case "all":
	case "clustered":
		k := dialect.Clustered
		o.Kind = &k
	case "nonclustered":
		k := dialect.NonClustered
		o.Kind = &k
	default:
		return nil, fmt.Errorf("--kind must be all, clustered or nonclustered")
	}
	switch *mode {
	case "auto":
		o.Mode = maintain.Auto
	case "rebuild":
		o.Mode = maintain.RebuildAll
	case "reorganize":
		o.Mode = maintain.ReorganizeAll
	default:
		return nil, fmt.Errorf("--mode must be auto, rebuild or reorganize")
	}
	switch *whenUnknown {
	case "rebuild":
		o.RebuildUnknown = true
	case "skip":
	default:
		return nil, fmt.Errorf("--when-unknown must be rebuild or skip")
	}
	if *reorganizeAt < 0 || *rebuildAt < 0 || *rebuildAt > 100 {
		return nil, errors.New("thresholds must be between 0 and 100")
	}
	if *at != "" {
		if s.at, err = time.ParseInLocation("2006-01-02 15:04", *at, time.Local); err != nil {
			return nil, fmt.Errorf(`--at must look like "2006-01-02 15:04"`)
		}
	}
	s.options = o
	return s, nil
}
