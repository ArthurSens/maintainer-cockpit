package periodic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// State persists enough information to distinguish a new schedule target from
// a process restart without replaying cron matches missed during downtime.
type State interface {
	InitializeSchedule(context.Context, string, string, time.Time) (bool, error)
	RecordScheduleDispatch(context.Context, string, string, time.Time) error
}

// Entry describes one independently scheduled operation target.
type Entry struct {
	Operation  string
	Target     string
	Expression string
	Dispatch   func(context.Context, time.Time) error
}

// Status is the live scheduling state exposed to operators.
type Status struct {
	Operation  string    `json:"operation"`
	Collection string    `json:"collection,omitempty"`
	Expression string    `json:"expression"`
	Enabled    bool      `json:"enabled"`
	NextRun    time.Time `json:"nextRun"`
}

type scheduledEntry struct {
	Entry
	schedule cron.Schedule
	next     time.Time
}

// Coordinator dispatches cron matches into durable operation queues.
type Coordinator struct {
	mu          sync.RWMutex
	state       State
	entries     []scheduledEntry
	initialized bool
}

func New(entries []Entry, state State) (*Coordinator, error) {
	if state == nil {
		return nil, errors.New("periodic schedule state is required")
	}
	result := &Coordinator{state: state, entries: make([]scheduledEntry, 0, len(entries))}
	for _, entry := range entries {
		if entry.Operation == "" || entry.Dispatch == nil {
			return nil, errors.New("periodic schedule operation and dispatch are required")
		}
		schedule, err := Parse(entry.Expression)
		if err != nil {
			return nil, fmt.Errorf("%s schedule: %w", entry.Operation, err)
		}
		result.entries = append(result.entries, scheduledEntry{Entry: entry, schedule: schedule})
	}
	return result, nil
}

// Initialize calculates future matches and immediately bootstraps targets that
// have never had a schedule dispatch.
func (coordinator *Coordinator) Initialize(ctx context.Context, now time.Time) error {
	now = now.UTC()
	coordinator.mu.Lock()
	for index := range coordinator.entries {
		coordinator.entries[index].next = coordinator.entries[index].schedule.Next(now)
	}
	coordinator.initialized = true
	entries := append([]scheduledEntry(nil), coordinator.entries...)
	coordinator.mu.Unlock()

	for _, entry := range entries {
		first, err := coordinator.state.InitializeSchedule(
			ctx, entry.Operation, entry.Target, now,
		)
		if err != nil {
			return err
		}
		if first {
			if err := entry.Dispatch(ctx, now); err != nil {
				return fmt.Errorf("bootstrap %s schedule: %w", entry.Operation, err)
			}
		}
	}
	return nil
}

// RunDue dispatches at most once per target and advances directly beyond now,
// coalescing multiple elapsed matches.
func (coordinator *Coordinator) RunDue(ctx context.Context, now time.Time) error {
	now = now.UTC()
	var due []scheduledEntry
	coordinator.mu.Lock()
	for index := range coordinator.entries {
		entry := &coordinator.entries[index]
		if entry.next.IsZero() || entry.next.After(now) {
			continue
		}
		due = append(due, *entry)
		entry.next = entry.schedule.Next(now)
	}
	coordinator.mu.Unlock()

	for _, entry := range due {
		if err := coordinator.state.RecordScheduleDispatch(
			ctx, entry.Operation, entry.Target, now,
		); err != nil {
			return err
		}
		if err := entry.Dispatch(ctx, now); err != nil {
			return fmt.Errorf("dispatch %s schedule: %w", entry.Operation, err)
		}
	}
	return nil
}

func (coordinator *Coordinator) Status() []Status {
	coordinator.mu.RLock()
	defer coordinator.mu.RUnlock()
	result := make([]Status, 0, len(coordinator.entries))
	for _, entry := range coordinator.entries {
		result = append(result, Status{
			Operation: entry.Operation, Collection: entry.Target,
			Expression: entry.Expression, Enabled: true, NextRun: entry.next,
		})
	}
	return result
}

// Run initializes the coordinator and waits efficiently for future matches.
func (coordinator *Coordinator) Run(ctx context.Context) error {
	coordinator.mu.RLock()
	initialized := coordinator.initialized
	coordinator.mu.RUnlock()
	if !initialized {
		if err := coordinator.Initialize(ctx, time.Now().UTC()); err != nil {
			return err
		}
	}
	for {
		next := coordinator.next()
		if next.IsZero() {
			<-ctx.Done()
			return nil
		}
		delay := max(time.Until(next), 0)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case now := <-timer.C:
			if err := coordinator.RunDue(ctx, now.UTC()); err != nil {
				return err
			}
		}
	}
}

func (coordinator *Coordinator) next() time.Time {
	coordinator.mu.RLock()
	defer coordinator.mu.RUnlock()
	var next time.Time
	for _, entry := range coordinator.entries {
		if next.IsZero() || entry.next.Before(next) {
			next = entry.next
		}
	}
	return next
}
