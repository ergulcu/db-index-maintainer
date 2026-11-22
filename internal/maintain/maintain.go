// Copyright 2018 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

// Package maintain decides what to do with each index and does it.
package maintain

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ergulcu/db-index-maintainer/internal/dialect"
)

// Mode is how actions are chosen.
type Mode int

const (
	// Auto reorganizes or rebuilds depending on the fragmentation (the recommended way).
	Auto Mode = iota
	// RebuildAll rebuilds every selected index, as the "rebuild" buttons of the Java version did.
	RebuildAll
	// ReorganizeAll reorganizes every selected index.
	ReorganizeAll
)

// Options select and tune the work.
type Options struct {
	// Kind limits the work to clustered or nonclustered indexes; nil means all.
	Kind *dialect.Kind
	Mode Mode
	// ReorganizeAt and RebuildAt are fragmentation thresholds in percent (Auto mode).
	ReorganizeAt float64
	RebuildAt    float64
	// MinPages skips smaller indexes in Auto mode; their fragmentation says little.
	MinPages int64
	// RebuildUnknown rebuilds indexes whose fragmentation is unknown (Auto mode); otherwise they are skipped.
	RebuildUnknown bool
	// Online keeps the tables usable during rebuilds where the database supports it.
	Online bool
	// Wait pauses after each index as long as that index took, to leave room for the regular load.
	Wait bool
	// Window stops starting new indexes once it has passed; 0 means no limit.
	Window time.Duration
	// DryRun only reports the statements.
	DryRun bool
}

// Step is the decision for one index.
type Step struct {
	Index      dialect.Index
	Action     dialect.Action
	Reason     string
	Statements []string
}

// Plan decides the action for each index.
func Plan(d dialect.Dialect, indexes []dialect.Index, o Options) ([]Step, error) {
	if o.Mode == ReorganizeAll && !d.CanReorganize() {
		return nil, fmt.Errorf("%s cannot reorganize indexes; use --mode rebuild or --mode auto", d.Name())
	}
	if o.Mode == Auto && o.ReorganizeAt > o.RebuildAt {
		return nil, fmt.Errorf("--reorganize-at (%.1f) must not be above --rebuild-at (%.1f)", o.ReorganizeAt, o.RebuildAt)
	}
	var steps []Step
	for _, idx := range indexes {
		if o.Kind != nil && idx.Kind != *o.Kind {
			continue
		}
		action, reason := decide(d, idx, o)
		statements, err := d.Statements(idx, action, o.Online)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", idx.FullName(), err)
		}
		steps = append(steps, Step{Index: idx, Action: action, Reason: reason, Statements: statements})
	}
	return steps, nil
}

func decide(d dialect.Dialect, idx dialect.Index, o Options) (dialect.Action, string) {
	switch o.Mode {
	case RebuildAll:
		return dialect.Rebuild, "--mode rebuild"
	case ReorganizeAll:
		return dialect.Reorganize, "--mode reorganize"
	}
	if idx.Pages >= 0 && idx.Pages < o.MinPages {
		return dialect.Skip, fmt.Sprintf("only %d pages (below %d)", idx.Pages, o.MinPages)
	}
	if idx.Fragmentation == nil {
		if o.RebuildUnknown {
			return dialect.Rebuild, "fragmentation unknown"
		}
		return dialect.Skip, "fragmentation unknown"
	}
	f := *idx.Fragmentation
	switch {
	case f >= o.RebuildAt:
		return dialect.Rebuild, fmt.Sprintf("fragmentation %.1f%% (rebuild from %.0f%%)", f, o.RebuildAt)
	case f >= o.ReorganizeAt && d.CanReorganize():
		return dialect.Reorganize, fmt.Sprintf("fragmentation %.1f%% (reorganize from %.0f%%)", f, o.ReorganizeAt)
	case f >= o.ReorganizeAt:
		return dialect.Skip, fmt.Sprintf("fragmentation %.1f%%; %s cannot reorganize and rebuilds from %.0f%%",
			f, d.Name(), o.RebuildAt)
	default:
		return dialect.Skip, fmt.Sprintf("fragmentation %.1f%% (below %.0f%%)", f, o.ReorganizeAt)
	}
}

// Execer runs a statement; *sql.DB is one.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Outcome of a step.
type Outcome int

const (
	Skipped Outcome = iota
	Done
	Failed
	NotStarted // the window was over, or the run was interrupted
	Planned    // dry run
)

func (o Outcome) String() string {
	switch o {
	case Done:
		return "done"
	case Failed:
		return "failed"
	case NotStarted:
		return "not started"
	case Planned:
		return "planned"
	default:
		return "skipped"
	}
}

// Event reports the progress of one step.
type Event struct {
	Step    Step
	Outcome Outcome
	Took    time.Duration
	Err     error
}

// Summary counts the outcomes.
type Summary struct {
	Rebuilt, Reorganized, Skipped, Failed, NotStarted, Planned int
}

// Run carries out the steps one after the other. A failing index does not stop the others.
func Run(ctx context.Context, db Execer, steps []Step, o Options, report func(Event)) Summary {
	var sum Summary
	var deadline time.Time
	if o.Window > 0 {
		deadline = time.Now().Add(o.Window)
	}
	for _, step := range steps {
		ev := Event{Step: step}
		switch {
		case step.Action == dialect.Skip:
			ev.Outcome = Skipped
			sum.Skipped++
		case o.DryRun:
			ev.Outcome = Planned
			sum.Planned++
		case ctx.Err() != nil || (!deadline.IsZero() && time.Now().After(deadline)):
			ev.Outcome = NotStarted
			sum.NotStarted++
		default:
			start := time.Now()
			for _, statement := range step.Statements {
				if _, err := db.ExecContext(ctx, statement); err != nil {
					ev.Err = err
					break
				}
			}
			ev.Took = time.Since(start)
			if ev.Err != nil {
				ev.Outcome = Failed
				sum.Failed++
			} else {
				ev.Outcome = Done
				if step.Action == dialect.Rebuild {
					sum.Rebuilt++
				} else {
					sum.Reorganized++
				}
			}
		}
		report(ev)
		if ev.Outcome == Done && o.Wait && ev.Took > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(ev.Took):
			}
		}
	}
	return sum
}
