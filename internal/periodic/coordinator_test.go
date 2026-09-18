package periodic

import (
	"context"
	"testing"
	"time"
)

type memoryState struct {
	initialized map[string]bool
}

func (state *memoryState) InitializeSchedule(_ context.Context, operation, target string, _ time.Time) (bool, error) {
	if state.initialized == nil {
		state.initialized = make(map[string]bool)
	}
	key := operation + "\x00" + target
	if state.initialized[key] {
		return false, nil
	}
	state.initialized[key] = true
	return true, nil
}

func (state *memoryState) RecordScheduleDispatch(_ context.Context, operation, target string, _ time.Time) error {
	if state.initialized == nil {
		state.initialized = make(map[string]bool)
	}
	state.initialized[operation+"\x00"+target] = true
	return nil
}

func TestCoordinatorBootstrapsOnceAndPublishesNextRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 9, 13, 10, 23, 0, 0, time.FixedZone("local", -3*60*60))
	state := &memoryState{}
	dispatches := 0
	entry := Entry{
		Operation: "github", Expression: "0 * * * *",
		Dispatch: func(context.Context, time.Time) error {
			dispatches++
			return nil
		},
	}
	coordinator, err := New([]Entry{entry}, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Initialize(ctx, now); err != nil {
		t.Fatal(err)
	}
	if dispatches != 1 {
		t.Fatalf("bootstrap dispatches = %d, want 1", dispatches)
	}
	status := coordinator.Status()
	wantNext := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	if len(status) != 1 || !status[0].NextRun.Equal(wantNext) {
		t.Fatalf("status = %+v, want next run %s", status, wantNext)
	}

	restarted, err := New([]Entry{entry}, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Initialize(ctx, now); err != nil {
		t.Fatal(err)
	}
	if dispatches != 1 {
		t.Fatalf("restart dispatches = %d, want bootstrap skipped", dispatches)
	}
}

func TestCoordinatorCoalescesElapsedMatches(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	started := time.Date(2026, 9, 13, 10, 1, 0, 0, time.UTC)
	state := &memoryState{initialized: map[string]bool{"correlation\x00acme": true}}
	dispatches := 0
	coordinator, err := New([]Entry{{
		Operation: "correlation", Target: "acme", Expression: "*/5 * * * *",
		Dispatch: func(context.Context, time.Time) error {
			dispatches++
			return nil
		},
	}}, state)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Initialize(ctx, started); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.RunDue(ctx, started.Add(19*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if dispatches != 1 {
		t.Fatalf("dispatches = %d, want one coalesced dispatch", dispatches)
	}
	wantNext := time.Date(2026, 9, 13, 10, 25, 0, 0, time.UTC)
	if got := coordinator.Status()[0].NextRun; !got.Equal(wantNext) {
		t.Fatalf("next run = %s, want %s", got, wantNext)
	}
}
