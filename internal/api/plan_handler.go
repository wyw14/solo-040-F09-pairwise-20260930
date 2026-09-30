package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/wyw14/cry-96/internal/admission"
	"github.com/wyw14/cry-96/internal/model"
	"github.com/wyw14/cry-96/internal/schedule"
	"github.com/wyw14/cry-96/internal/signal"
)

type vesselInput struct {
	Name     string  `json:"name"`
	CallSign string  `json:"call_sign"`
	LengthM  float64 `json:"length_m"`
}

type planInput struct {
	ChamberID string          `json:"chamber_id"`
	Direction model.Direction `json:"direction"`
	Vessels   []vesselInput   `json:"vessels"`
	Priority  int             `json:"priority,omitempty"`
	ETA       string          `json:"eta,omitempty"`
}

// planOrderInput updates the ordering basis. Nil fields keep the current
// value; an explicit empty ETA string clears a previously announced ETA.
type planOrderInput struct {
	Priority *int    `json:"priority"`
	ETA      *string `json:"eta"`
}

func parseETA(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	eta, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, errors.New("eta must be an RFC3339 timestamp")
	}
	return &eta, nil
}

func createPlanHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input planInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		vessels := make([]model.Vessel, 0, len(input.Vessels))
		for _, item := range input.Vessels {
			vessel, err := model.NewVessel(item.Name, item.CallSign, item.LengthM)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			vessels = append(vessels, vessel)
		}
		eta, err := parseETA(input.ETA)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		order := schedule.PlanOrder{Priority: input.Priority, ETA: eta}
		plan, err := runtime.Schedule.Create(r.Context(), input.ChamberID, input.Direction, vessels, order)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		cycleValue, err := runtime.CycleService.Start(r.Context(), plan)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"plan": plan, "cycle": cycleValue})
	}
}

func plansHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result := make(map[string]any, len(runtime.Chambers))
		for _, chamberID := range runtime.Chambers {
			plans := runtime.Schedule.Active(chamberID)
			items := make([]map[string]any, 0, len(plans))
			for position, plan := range plans {
				items = append(items, map[string]any{
					"plan": plan,
					// Queue positions start at one and follow the same
					// priority/arrival ordering the admission controller serves.
					"queue_position": position + 1,
					"released":       runtime.Schedule.PlanReleased(plan),
				})
			}
			result[chamberID] = items
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func publishSignalsHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		planID, err := uuid.Parse(chi.URLParam(r, "planID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		plan, ok := runtime.Planner.Get(planID)
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("plan not found"))
			return
		}
		cycleValue, ok := runtime.Cycles.Current(plan.ChamberID)
		if !ok {
			writeError(w, http.StatusConflict, errors.New("active transit not found"))
			return
		}
		publication, err := runtime.Schedule.Publish(r.Context(), planID, cycleValue)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusAccepted, publication)
	}
}

func cancelPlanHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		planID, err := uuid.Parse(chi.URLParam(r, "planID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		plan, ok := runtime.Planner.Get(planID)
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("plan not found"))
			return
		}
		cycleValue, ok := runtime.Cycles.Current(plan.ChamberID)
		publication := signal.Publication{
			CycleID: cycleValue.ID, Generation: cycleValue.Generation,
			Previous: make(map[uuid.UUID]model.SignalState),
		}
		if ok {
			for _, vessel := range plan.Vessels {
				if _, exists := runtime.SignalStore.Get(vessel.ID); exists {
					publication.Published = append(publication.Published, vessel.ID)
				}
			}
		}
		result, err := runtime.Schedule.Cancel(r.Context(), planID, "operator cancellation", publication)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		if ok {
			_, _ = runtime.Cycles.Cancel(r.Context(), plan.ChamberID, "operator cancellation")
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// adjustPlanHandler changes the ordering basis (priority and/or ETA) of a
// plan that has not released any vessel. Released plans are rejected so the
// already-committed admission order cannot be disturbed.
func adjustPlanHandler(runtime *Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		planID, err := uuid.Parse(chi.URLParam(r, "planID"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		plan, ok := runtime.Planner.Get(planID)
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("plan not found"))
			return
		}
		if plan.Cancelled {
			writeError(w, http.StatusConflict, errors.New("cancelled plan cannot be reordered"))
			return
		}
		if runtime.Schedule.PlanReleased(plan) {
			writeError(w, http.StatusConflict, admission.ErrPlanReleased)
			return
		}
		var input planOrderInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		priority := plan.Priority
		if input.Priority != nil {
			priority = *input.Priority
		}
		var eta *time.Time
		if input.ETA != nil {
			eta, err = parseETA(*input.ETA)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
		} else {
			eta = plan.ETA
		}
		updated, err := runtime.Schedule.Reprioritize(r.Context(), planID, priority, eta)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	}
}
