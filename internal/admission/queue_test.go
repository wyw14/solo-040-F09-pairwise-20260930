package admission

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
)

func testVessel(name string) model.Vessel {
	vessel, err := model.NewVessel(name, name+"-CS", 42)
	if err != nil {
		panic(err)
	}
	return vessel
}

func at(second int) time.Time {
	return time.Date(2026, 9, 30, 10, 0, second, 0, time.UTC)
}

func addPlan(t *testing.T, q *Queue, chamber string, priority int, arrival time.Time, hasETA bool, names ...string) uuid.UUID {
	t.Helper()
	planID := uuid.New()
	vessels := make([]model.Vessel, 0, len(names))
	for _, name := range names {
		vessels = append(vessels, testVessel(name))
	}
	if err := q.Add(chamber, planID, vessels, priority, arrival, hasETA); err != nil {
		t.Fatalf("add plan %s: %v", planID, err)
	}
	return planID
}

func TestQueueOrdersByPriorityThenArrival(t *testing.T) {
	q := NewQueue()
	chamber := "chamber-1"

	low := addPlan(t, q, chamber, 100, at(0), false, "low")
	highLate := addPlan(t, q, chamber, 50, at(30), true, "high-late")
	highEarly := addPlan(t, q, chamber, 50, at(10), true, "high-early")

	head, ok := q.Peek(chamber)
	if !ok {
		t.Fatal("expected non-empty queue")
	}
	if head.PlanID != highEarly {
		t.Fatalf("expected earliest ETA within high priority at head, got plan %s", head.PlanID)
	}
	if !head.HasETA {
		t.Fatal("head entry should report an announced ETA")
	}

	// FCFS tie-break falls back to arrival (ETA); insertion never wins over ETA.
	if _, err := q.Reprioritize(chamber, highLate, 50, at(5), true); err != nil {
		t.Fatalf("reprioritize: %v", err)
	}
	head, _ = q.Peek(chamber)
	if head.PlanID != highLate {
		t.Fatalf("expected moved-up ETA plan at head, got %s", head.PlanID)
	}

	// Raising the low-priority plan above everyone jumps it to the head.
	if _, err := q.Reprioritize(chamber, low, 10, at(0), false); err != nil {
		t.Fatalf("reprioritize: %v", err)
	}
	head, _ = q.Peek(chamber)
	if head.PlanID != low || head.HasETA {
		t.Fatalf("expected highest-priority FCFS plan at head, got %s hasETA=%v", head.PlanID, head.HasETA)
	}
}

func TestQueueReleasedPlansCannotReorder(t *testing.T) {
	q := NewQueue()
	chamber := "chamber-2"

	first := addPlan(t, q, chamber, 100, at(0), false, "first")
	second := addPlan(t, q, chamber, 100, at(10), false, "second")

	head, _ := q.Peek(chamber)
	if head.PlanID != first {
		t.Fatalf("expected FCFS head, got %s", head.PlanID)
	}
	if !q.MarkReleased(chamber, head.Vessel.ID) {
		t.Fatal("mark released failed")
	}
	if !q.PlanReleased(chamber, first) {
		t.Fatal("plan should be reported as released")
	}

	// A waiting plan may still move ahead; the released vessel stays anchored.
	if _, err := q.Reprioritize(chamber, second, 10, at(10), false); err != nil {
		t.Fatalf("reprioritize waiting plan: %v", err)
	}
	entries := q.List(chamber)
	if len(entries) != 2 || !entries[0].Released || entries[0].PlanID != first {
		t.Fatal("released vessel must remain anchored at the front")
	}

	// Peek skips the released vessel and serves the next waiting one.
	head, ok := q.Peek(chamber)
	if !ok || head.PlanID != second {
		t.Fatalf("expected next waiting plan %s, got %+v", second, head)
	}

	// The already-released plan can no longer be reordered.
	_, err := q.Reprioritize(chamber, first, 1, at(0), false)
	if !errors.Is(err, ErrPlanReleased) {
		t.Fatalf("expected ErrPlanReleased, got %v", err)
	}
}

func TestQueueVesselsOfOnePlanStayContiguous(t *testing.T) {
	q := NewQueue()
	chamber := "chamber-3"

	convoy := addPlan(t, q, chamber, 100, at(0), false, "a", "b", "c")
	addPlan(t, q, chamber, 100, at(10), false, "other")

	entries := q.List(chamber)
	var convoyPositions []int
	for i, entry := range entries {
		if entry.PlanID == convoy {
			convoyPositions = append(convoyPositions, i)
		}
	}
	if len(convoyPositions) != 3 || convoyPositions[0] != 0 || convoyPositions[1] != 1 || convoyPositions[2] != 2 {
		t.Fatalf("convoy vessels must stay contiguous and ahead of later arrival, got positions %v", convoyPositions)
	}
}
