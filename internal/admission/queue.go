package admission

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/model"
)

type QueueEntry struct {
	Vessel    model.Vessel `json:"vessel"`
	PlanID    uuid.UUID    `json:"plan_id"`
	Priority  int          `json:"priority"`
	ArrivalAt time.Time    `json:"arrival_at"`
	HasETA    bool         `json:"has_eta"`
	Released  bool         `json:"released"`
	seq       uint64
}

// ErrPlanReleased reports that a plan already has vessels past the signal,
// so its waiting order can no longer be changed.
var ErrPlanReleased = errors.New("plan has released vessels and cannot be reordered")

// Queue orders each chamber's waiting vessels. Ordering within a chamber is:
// released vessels stay anchored at the front (they may never be reordered),
// then priority ascending, then arrival time ascending (ETA when announced,
// otherwise creation time — first come first served within one priority).
type Queue struct {
	mu      sync.RWMutex
	entries map[string][]QueueEntry
	seq     map[string]uint64
}

func NewQueue() *Queue {
	return &Queue{entries: make(map[string][]QueueEntry), seq: make(map[string]uint64)}
}

func (q *Queue) Add(chamberID string, planID uuid.UUID, vessels []model.Vessel, priority int, arrival time.Time, hasETA bool) error {
	if chamberID == "" || planID == uuid.Nil || len(vessels) == 0 {
		return errors.New("queue addition requires chamber, plan and vessels")
	}
	if priority <= 0 {
		priority = model.DefaultPriority
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	arrival = arrival.UTC()
	for _, vessel := range vessels {
		q.seq[chamberID]++
		q.entries[chamberID] = append(q.entries[chamberID], QueueEntry{
			Vessel: vessel, PlanID: planID, Priority: priority,
			ArrivalAt: arrival, HasETA: hasETA, seq: q.seq[chamberID],
		})
	}
	q.resortLocked(chamberID)
	return nil
}

// Peek returns the next vessel that is still waiting for release.
func (q *Queue) Peek(chamberID string) (QueueEntry, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	for _, entry := range q.entries[chamberID] {
		if !entry.Released {
			return entry, true
		}
	}
	return QueueEntry{}, false
}

func (q *Queue) MarkReleased(chamberID string, vesselID uuid.UUID) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for index := range q.entries[chamberID] {
		if q.entries[chamberID][index].Vessel.ID == vesselID {
			q.entries[chamberID][index].Released = true
			return true
		}
	}
	return false
}

// PlanReleased reports whether any vessel of the plan has passed the signal.
func (q *Queue) PlanReleased(chamberID string, planID uuid.UUID) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	for _, entry := range q.entries[chamberID] {
		if entry.PlanID == planID && entry.Released {
			return true
		}
	}
	return false
}

// Reprioritize reorders the still-waiting vessels of a plan. Plans that
// already have released vessels are refused: their order is committed.
func (q *Queue) Reprioritize(chamberID string, planID uuid.UUID, priority int, arrival time.Time, hasETA bool) (int, error) {
	if priority <= 0 {
		priority = model.DefaultPriority
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	entries := q.entries[chamberID]
	matched := 0
	for index := range entries {
		if entries[index].PlanID != planID {
			continue
		}
		if entries[index].Released {
			return 0, ErrPlanReleased
		}
		entries[index].Priority = priority
		entries[index].ArrivalAt = arrival.UTC()
		entries[index].HasETA = hasETA
		matched++
	}
	if matched == 0 {
		return 0, errors.New("no waiting queue entries for plan")
	}
	q.resortLocked(chamberID)
	return matched, nil
}

func (q *Queue) RemovePlan(chamberID string, planID uuid.UUID) []QueueEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	entries := q.entries[chamberID]
	kept := entries[:0]
	var removed []QueueEntry
	for _, entry := range entries {
		if entry.PlanID == planID {
			removed = append(removed, entry)
		} else {
			kept = append(kept, entry)
		}
	}
	q.entries[chamberID] = append([]QueueEntry(nil), kept...)
	return removed
}

// Restore reinserts entries (for example after a failed cancellation) back
// into the waiting area; released vessels of other plans stay anchored ahead.
func (q *Queue) Restore(chamberID string, entries []QueueEntry) {
	q.mu.Lock()
	defer q.mu.Unlock()
	current := q.entries[chamberID]
	restored := make([]QueueEntry, 0, len(entries)+len(current))
	for _, entry := range entries {
		entry.Released = false
		if entry.Priority <= 0 {
			entry.Priority = model.DefaultPriority
		}
		restored = append(restored, entry)
	}
	q.entries[chamberID] = append(restored, current...)
	q.resortLocked(chamberID)
}

func (q *Queue) List(chamberID string) []QueueEntry {
	q.mu.RLock()
	result := append([]QueueEntry(nil), q.entries[chamberID]...)
	q.mu.RUnlock()
	return result
}

// resortLocked keeps the released prefix untouched and stable-sorts every
// waiting vessel by priority, then arrival (FCFS), then insertion sequence.
func (q *Queue) resortLocked(chamberID string) {
	entries := q.entries[chamberID]
	firstWaiting := 0
	for firstWaiting < len(entries) && entries[firstWaiting].Released {
		firstWaiting++
	}
	waiting := entries[firstWaiting:]
	sort.SliceStable(waiting, func(i, j int) bool {
		if waiting[i].Priority != waiting[j].Priority {
			return waiting[i].Priority < waiting[j].Priority
		}
		if !waiting[i].ArrivalAt.Equal(waiting[j].ArrivalAt) {
			return waiting[i].ArrivalAt.Before(waiting[j].ArrivalAt)
		}
		return waiting[i].seq < waiting[j].seq
	})
}
