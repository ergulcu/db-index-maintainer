// Copyright 2018 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ergulcu/db-index-maintainer/internal/dialect"
	"github.com/ergulcu/db-index-maintainer/internal/maintain"
)

func noEnv(string) string { return "" }

func TestParseBuildsTheConnectionFromFlags(t *testing.T) {
	env := func(name string) string {
		if name == "INDEXAPP_PASSWORD" {
			return "from-env"
		}
		return ""
	}
	s, err := parse([]string{"--driver", "postgres", "--host", "db", "--user", "maint", "--database", "shop",
		"--kind", "clustered", "--mode", "rebuild", "--online", "--wait", "--window", "90m"}, env, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if s.dialect.Name() != "postgres" || s.dsn != "postgres://maint:from-env@db:5432/shop?application_name=indexapp" {
		t.Errorf("dialect %s dsn %s", s.dialect.Name(), s.dsn)
	}
	o := s.options
	if o.Kind == nil || *o.Kind != dialect.Clustered || o.Mode != maintain.RebuildAll || !o.Online || !o.Wait ||
		o.Window != 90*time.Minute {
		t.Errorf("options = %+v", o)
	}
}

func TestParseReadsThePasswordFromStdin(t *testing.T) {
	s, err := parse([]string{"--driver", "sqlserver", "--host", "db", "--user", "sa", "--password-stdin"}, noEnv,
		strings.NewReader("s3cret\r\nignored"), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.dsn, "sa:s3cret@db:1433") {
		t.Errorf("dsn = %s", s.dsn)
	}
}

func TestParseDefaultsAndDSN(t *testing.T) {
	env := func(name string) string {
		if name == "INDEXAPP_DSN" {
			return "oracle://u:p@h:1521/SVC"
		}
		return ""
	}
	s, err := parse([]string{"--driver", "oracle", "--at", "2030-01-02 03:04"}, env, nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	o := s.options
	if s.dsn != "oracle://u:p@h:1521/SVC" || o.Mode != maintain.Auto || o.Kind != nil || o.ReorganizeAt != 5 ||
		o.RebuildAt != 30 || o.MinPages != 1000 || !o.RebuildUnknown || o.DryRun {
		t.Errorf("settings = %+v", s)
	}
	if s.at.Year() != 2030 || s.at.Hour() != 3 || s.at.Minute() != 4 {
		t.Errorf("at = %v", s.at)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	cases := map[string][]string{
		"no driver":      {"--dsn", "x"},
		"unknown driver": {"--driver", "db2", "--dsn", "x"},
		"no connection":  {"--driver", "postgres"},
		"bad kind":       {"--driver", "postgres", "--dsn", "x", "--kind", "unique"},
		"bad mode":       {"--driver", "postgres", "--dsn", "x", "--mode", "fast"},
		"bad unknown":    {"--driver", "postgres", "--dsn", "x", "--when-unknown", "ask"},
		"bad time":       {"--driver", "postgres", "--dsn", "x", "--at", "tomorrow"},
		"bad threshold":  {"--driver", "postgres", "--dsn", "x", "--rebuild-at", "150"},
		"extra argument": {"--driver", "postgres", "--dsn", "x", "now"},
		"unknown flag":   {"--driver", "postgres", "--dsn", "x", "--fast"},
	}
	for name, args := range cases {
		if _, err := parse(args, noEnv, nil, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestRunReportsUsageErrorsAndVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"--driver", "db2"}, noEnv, nil, &out, &errOut); code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if code := run(context.Background(), []string{"--version"}, noEnv, nil, &out, &errOut); code != exitOK ||
		!strings.HasPrefix(out.String(), "indexapp ") {
		t.Errorf("version: %d %q", code, out.String())
	}
}

func TestRunFailsWhenTheDatabaseIsUnreachable(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), []string{"--driver", "postgres",
		"--dsn", "postgres://u:p@127.0.0.1:1/none?connect_timeout=2"}, noEnv, nil, &out, &errOut)
	if code != exitFailed || !strings.Contains(errOut.String(), "cannot connect") {
		t.Errorf("exit %d, stderr %q", code, errOut.String())
	}
}
