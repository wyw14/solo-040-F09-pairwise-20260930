package schedule

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/admission"
	"github.com/wyw14/cry-96/internal/journal"
	"github.com/wyw14/cry-96/internal/model"
)

// Planner owns convoy plans and their admission queue membership.
type Planner struct {
	mu        sync.RWMutex
	plans     map[uuid.UUID]model.ConvoyPlan
	admission *admission.Service
	recorder  *journal.Recorder
	now       func() time.Time
}

func NewPlanner(admissionService *admission.Service, recorder *journal.Recorder, now func() time.Time) *Planner {
	if now == nil {
		now = time.Now
	}
	return &Planner{
		plans: make(map[uuid.UUID]model.ConvoyPlan), admission: admissionService,
		recorder: recorder, now: now,
	}
}

// PlanOrder carries the optional scheduling basis for a convoy plan.
type PlanOrder struct {
	Priority int
	ETA      *time.Time
}

func (p *Planner) Create(ctx context.Context, chamberID string, direction model.Direction, vessels []model.Vessel, order PlanOrder) (model.ConvoyPlan, error) {
	plan, err := model.NewConvoyPlan(chamberID, direction, vessels, order.Priority, order.ETA, p.now())
	if err != nil {
		return model.ConvoyPlan{}, err
	}
	if err := p.admission.Enqueue(ctx, plan); err != nil {
		return model.ConvoyPlan{}, err
	}
	p.mu.Lock()
	p.plans[plan.ID] = plan
	p.mu.Unlock()
	if _, err := p.recorder.Record(ctx, chamberID, model.EventPlanCreated, plan.ID, 0, plan); err != nil {
		p.mu.Lock()
		delete(p.plans, plan.ID)
		p.mu.Unlock()
		p.admission.RemovePlan(chamberID, plan)
		return model.ConvoyPlan{}, err
	}
	return plan.Clone(), nil
}

// Reprioritize changes the ordering basis of a plan that has not released
// any vessel yet. The admission queue and the stored plan move together;
// plans with released vessels are rejected.
func (p *Planner) Reprioritize(ctx context.Context, planID uuid.UUID, order PlanOrder) (model.ConvoyPlan, error) {
	previous, err := p.require(planID)
	if err != nil {
		return model.ConvoyPlan{}, err
	}
	if previous.Cancelled {
		return model.ConvoyPlan{}, errors.New("cancelled plan cannot be reprioritized")
	}
	if order.Priority <= 0 {
		order.Priority = model.DefaultPriority
	}
	updated := previous
	updated.Priority = order.Priority
	if order.ETA != nil {
		utc := order.ETA.UTC()
		updated.ETA = &utc
	} else {
		updated.ETA = nil
	}
	affected, err := p.admission.Reprioritize(ctx, updated)
	if err != nil {
		return model.ConvoyPlan{}, err
	}
	p.replace(updated)
	if _, err := p.recorder.Record(ctx, updated.ChamberID, model.EventPlanReprioritized, updated.ID, 0, updated); err != nil {
		// Roll both the queue and the stored plan back to the old ordering.
		_, _ = p.admission.Reprioritize(context.WithoutCancel(ctx), previous)
		p.replace(previous)
		return model.ConvoyPlan{}, err
	}
	_ = affected
	return updated.Clone(), nil
}

// PlanReleased reports whether any of the plan's vessels has been admitted.
func (p *Planner) PlanReleased(plan model.ConvoyPlan) bool {
	return p.admission.PlanReleased(plan.ChamberID, plan.ID)
}

func (p *Planner) Get(planID uuid.UUID) (model.ConvoyPlan, bool) {
	p.mu.RLock()
	plan, ok := p.plans[planID]
	p.mu.RUnlock()
	return plan.Clone(), ok
}

func (p *Planner) replace(plan model.ConvoyPlan) {
	p.mu.Lock()
	p.plans[plan.ID] = plan.Clone()
	p.mu.Unlock()
}

// Active returns plans in the same order the admission queue serves them:
// priority ascending, then announced ETA / creation time (FCFS).
func (p *Planner) Active(chamberID string) []model.ConvoyPlan {
	p.mu.RLock()
	var result []model.ConvoyPlan
	for _, plan := range p.plans {
		if plan.ChamberID == chamberID && !plan.Cancelled {
			result = append(result, plan.Clone())
		}
	}
	p.mu.RUnlock()
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority < result[j].Priority
		}
		if !result[i].ArrivalAt().Equal(result[j].ArrivalAt()) {
			return result[i].ArrivalAt().Before(result[j].ArrivalAt())
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result
}

func (p *Planner) require(planID uuid.UUID) (model.ConvoyPlan, error) {
	plan, ok := p.Get(planID)
	if !ok {
		return model.ConvoyPlan{}, errors.New("convoy plan not found")
	}
	return plan, nil
}
