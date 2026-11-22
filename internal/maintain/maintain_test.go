// Copyright 2018 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

package maintain

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ergulcu/db-index-maintainer/internal/dialect"
)

func pct(f float64) *float64 { return &f }

func index(name string, kind dialect.Kind, fragmentation *float64, pages int64) dialect.Index {
	return dialect.Index{Schema: "dbo", Table: "orders", Name: name, Kind: kind, Fragmentation: fragmentation, Pages: pages}
}

var auto = Options{Mode: Auto, ReorganizeAt: 5, RebuildAt: 30, MinPages: 1000, RebuildUnknown: true}

func actions(steps []Step) map[string]dialect.Action {
	m := map[string]dialect.Action{}
	for _, s := range steps {
		m[s.Index.Name] = s.Action
	}
	return m
}

func TestAutoModeFollowsTheThresholds(t *testing.T) {
	indexes := []dialect.Index{
		index("low", dialect.NonClustered, pct(2), 5000),
		index("medium", dialect.NonClustered, pct(12), 5000),
		index("high", dialect.Clustered, pct(45), 5000),
		index("exactly-30", dialect.NonClustered, pct(30), 5000),
		index("tiny", dialect.NonClustered, pct(90), 8),
		index("unknown", dialect.NonClustered, nil, -1),
	}
	steps, err := Plan(dialect.SQLServer{}, indexes, auto)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]dialect.Action{"low": dialect.Skip, "medium": dialect.Reorganize, "high": dialect.Rebuild,
		"exactly-30": dialect.Rebuild, "tiny": dialect.Skip, "unknown": dialect.Rebuild}
	got := actions(steps)
	for name, action := range want {
		if got[name] != action {
			t.Errorf("%s: %s, want %s", name, got[name], action)
		}
	}
	for _, s := range steps {
		if s.Action != dialect.Skip && len(s.Statements) == 0 {
			t.Errorf("%s has no statement", s.Index.Name)
		}
		if s.Reason == "" {
			t.Errorf("%s has no reason", s.Index.Name)
		}
	}
}

func TestUnknownFragmentationCanBeSkipped(t *testing.T) {
	o := auto
	o.RebuildUnknown = false
	steps, _ := Plan(dialect.Oracle{}, []dialect.Index{index("i", dialect.NonClustered, nil, -1)}, o)
	if steps[0].Action != dialect.Skip {
		t.Errorf("action = %s, want skip", steps[0].Action)
	}
}

func TestPostgresSkipsTheReorganizeRange(t *testing.T) {
	steps, err := Plan(dialect.Postgres{}, []dialect.Index{
		index("medium", dialect.NonClustered, pct(12), 5000),
		index("high", dialect.NonClustered, pct(40), 5000),
	}, auto)
	if err != nil {
		t.Fatal(err)
	}
	if got := actions(steps); got["medium"] != dialect.Skip || got["high"] != dialect.Rebuild {
		t.Errorf("actions = %v", got)
	}
	if !strings.Contains(steps[0].Reason, "cannot reorganize") {
		t.Errorf("reason = %q", steps[0].Reason)
	}
	if _, err := Plan(dialect.Postgres{}, nil, Options{Mode: ReorganizeAll}); err == nil {
		t.Error("reorganize mode must be refused for PostgreSQL")
	}
}

func TestRebuildModeTakesEverySelectedIndex(t *testing.T) {
	clustered := dialect.Clustered
	o := Options{Mode: RebuildAll, Kind: &clustered, MinPages: 1000}
	steps, _ := Plan(dialect.SQLServer{}, []dialect.Index{
		index("pk", dialect.Clustered, pct(0), 3),
		index("ix", dialect.NonClustered, pct(80), 9000),
	}, o)
	if len(steps) != 1 || steps[0].Index.Name != "pk" || steps[0].Action != dialect.Rebuild {
		t.Errorf("steps = %+v", steps)
	}
}

func TestThresholdsMustBeOrdered(t *testing.T) {
	if _, err := Plan(dialect.SQLServer{}, nil, Options{Mode: Auto, ReorganizeAt: 50, RebuildAt: 30}); err == nil {
		t.Error("reorganize-at above rebuild-at must be refused")
	}
}

type fakeDB struct {
	executed []string
	failOn   string
	delay    time.Duration
}

func (f *fakeDB) ExecContext(_ context.Context, query string, _ ...any) (sql.Result, error) {
	time.Sleep(f.delay)
	f.executed = append(f.executed, query)
	if f.failOn != "" && strings.Contains(query, f.failOn) {
		return nil, errors.New("online rebuild needs Enterprise edition")
	}
	return nil, nil
}

func plan(t *testing.T, o Options, indexes ...dialect.Index) []Step {
	t.Helper()
	steps, err := Plan(dialect.SQLServer{}, indexes, o)
	if err != nil {
		t.Fatal(err)
	}
	return steps
}

func TestRunContinuesAfterAFailure(t *testing.T) {
	db := &fakeDB{failOn: "[ix_b]"}
	steps := plan(t, auto,
		index("ix_a", dialect.NonClustered, pct(50), 5000),
		index("ix_b", dialect.NonClustered, pct(50), 5000),
		index("ix_c", dialect.NonClustered, pct(10), 5000),
		index("ix_d", dialect.NonClustered, pct(1), 5000))

	var events []Event
	sum := Run(context.Background(), db, steps, auto, func(e Event) { events = append(events, e) })

	if sum != (Summary{Rebuilt: 1, Reorganized: 1, Failed: 1, Skipped: 1}) {
		t.Errorf("summary = %+v", sum)
	}
	if len(db.executed) != 3 || len(events) != 4 || events[1].Err == nil {
		t.Errorf("executed = %v, events = %+v", db.executed, events)
	}
}

func TestDryRunExecutesNothing(t *testing.T) {
	db := &fakeDB{}
	o := auto
	o.DryRun = true
	sum := Run(context.Background(), db, plan(t, o, index("ix", dialect.NonClustered, pct(50), 5000)), o, func(Event) {})
	if len(db.executed) != 0 || sum.Planned != 1 {
		t.Errorf("executed = %v, summary = %+v", db.executed, sum)
	}
}

func TestWindowStopsNewWork(t *testing.T) {
	db := &fakeDB{delay: 30 * time.Millisecond}
	o := auto
	o.Window = 10 * time.Millisecond
	steps := plan(t, o,
		index("ix_a", dialect.NonClustered, pct(50), 5000),
		index("ix_b", dialect.NonClustered, pct(50), 5000))

	sum := Run(context.Background(), db, steps, o, func(Event) {})

	if sum.Rebuilt != 1 || sum.NotStarted != 1 {
		t.Errorf("summary = %+v", sum)
	}
}

func TestWaitPausesAsLongAsTheLastIndexTook(t *testing.T) {
	db := &fakeDB{delay: 40 * time.Millisecond}
	o := auto
	o.Wait = true
	steps := plan(t, o,
		index("ix_a", dialect.NonClustered, pct(50), 5000),
		index("ix_b", dialect.NonClustered, pct(50), 5000))

	start := time.Now()
	Run(context.Background(), db, steps, o, func(Event) {})

	// two statements of 40 ms each, plus two pauses of about 40 ms
	if took := time.Since(start); took < 150*time.Millisecond {
		t.Errorf("took %v, the pauses are missing", took)
	}
}

func TestInterruptedRunStartsNothingMore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sum := Run(ctx, &fakeDB{}, plan(t, auto, index("ix", dialect.NonClustered, pct(50), 5000)), auto, func(Event) {})
	if sum.NotStarted != 1 {
		t.Errorf("summary = %+v", sum)
	}
}
