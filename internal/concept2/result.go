package concept2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// HeartRateSummary holds a heart rate summary, when a monitor was
// connected. It's used both on individual Splits/intervals and on the
// top-level Result. Concept2 also reports "min" here (and, for the
// per-split/interval variant only, "rest"), which we don't currently display
// and so don't capture.
type HeartRateSummary struct {
	Average  int `json:"average"`
	Max      int `json:"max"`
	Ending   int `json:"ending"`
	Recovery int `json:"recovery"`
}

// Value returns the best available single heart rate figure, falling back
// through Concept2's own fields in order — a monitor can fail to populate
// some of them for a given result/interval (confirmed against a real
// production delivery, 2026-08-12: a VariableInterval workout where every
// interval's "average" came back 0 despite "ending" holding a real reading):
//  1. Average, if non-zero
//  2. Max, if Average is 0 and Max is non-zero
//  3. Ending, if Max is 0 and Ending is non-zero
//  4. Recovery, if Ending is 0 and Recovery is non-zero
//  5. 0, if every value above is 0
func (h HeartRateSummary) Value() int {
	if h.Average != 0 {
		return h.Average
	}
	if h.Max != 0 {
		return h.Max
	}
	if h.Ending != 0 {
		return h.Ending
	}
	if h.Recovery != 0 {
		return h.Recovery
	}
	return 0
}

// Split holds data for one split or interval within a workout.
type Split struct {
	Time       int64             `json:"time"`           // tenths of a second (work time)
	Distance   float64           `json:"distance"`       // metres
	StrokeRate int               `json:"stroke_rate"`    // spm
	RestTime   int64             `json:"rest_time"`      // tenths of a second (intervals only)
	Calories   int               `json:"calories_total"` // total calories
	HeartRate  *HeartRateSummary `json:"heart_rate,omitempty"`
}

// Pace returns the pace per 500m in tenths of a second, calculated from time
// and distance. Returns 0 if distance is zero.
func (s Split) Pace() int64 {
	if s.Distance <= 0 {
		return 0
	}
	return int64(float64(s.Time) * 500 / s.Distance)
}

// Watts returns the average power output in watts, per Concept2's published
// formula (https://www.concept2.com/training/watts-calculator):
// watts = 2.80 / pace³, where pace is seconds per metre. Computed directly
// from time and distance (not via Pace()) to avoid compounding rounding
// error. Returns 0 if distance or time is zero/negative.
func (s Split) Watts() float64 {
	if s.Distance <= 0 || s.Time <= 0 {
		return 0
	}
	paceSecPerMeter := (float64(s.Time) / 10) / s.Distance
	return 2.80 / (paceSecPerMeter * paceSecPerMeter * paceSecPerMeter)
}

// Workout holds the split or interval breakdown of a result.
// The API uses "splits" for fixed-distance/time workouts and "intervals" for
// variable-interval workouts. Only one will be populated.
type Workout struct {
	Splits    []Split `json:"splits"`
	Intervals []Split `json:"intervals"`
}

// Pieces returns whichever of Intervals or Splits is populated, preferring
// Intervals. Returns nil if neither is present.
func (w Workout) Pieces() []Split {
	if len(w.Intervals) > 0 {
		return w.Intervals
	}
	return w.Splits
}

// IsIntervals reports whether the workout used intervals (vs. splits).
func (w Workout) IsIntervals() bool {
	return len(w.Intervals) > 0
}

// Result holds the data for a single Concept2 Logbook workout result.
type Result struct {
	ID            int64             `json:"id"`
	UserID        int64             `json:"user_id"`
	Date          string            `json:"date"`           // "2024-01-15 HH:MM:SS" or "2024-01-15"
	Distance      float64           `json:"distance"`       // metres
	Type          string            `json:"type"`           // "rower", "skierg", "bikeerg"
	Time          int64             `json:"time"`           // tenths of a second (work time only)
	TimeFormatted string            `json:"time_formatted"` // pre-formatted by API; includes rest for intervals
	Pace          int64             `json:"pace"`           // tenths of a second per 500m (0 on list results)
	StrokeRate    int               `json:"stroke_rate"`    // spm
	WeightClass   string            `json:"weight_class"`   // "H" or "L"
	Verified      bool              `json:"verified"`
	Ranked        bool              `json:"ranked"`
	Comments      string            `json:"comments"`
	WorkoutType   string            `json:"workout_type"` // "JustRow", "FixedDistanceSplits", etc.
	Workout       Workout           `json:"workout"`
	Calories      int               `json:"calories_total"` // total calories
	HeartRate     *HeartRateSummary `json:"heart_rate,omitempty"`
}

// AveragePace returns the result's average pace per 500m in tenths of a
// second, computed from total distance and time using the same formula as
// Split.Pace(). The API's own "pace" field is unreliable in practice (often
// zero even on single-result fetches despite docs suggesting otherwise), so
// callers displaying pace should use this instead of the raw Pace field.
// Returns 0 if distance is zero.
func (r Result) AveragePace() int64 {
	if r.Distance <= 0 {
		return 0
	}
	return int64(float64(r.Time) * 500 / r.Distance)
}

// Watts returns the result's average power output in watts, per Concept2's
// published formula (https://www.concept2.com/training/watts-calculator):
// watts = 2.80 / pace³, where pace is seconds per metre. Computed directly
// from total time and distance (not via AveragePace()) to avoid compounding
// rounding error. Returns 0 if distance or time is zero/negative.
func (r Result) Watts() float64 {
	if r.Distance <= 0 || r.Time <= 0 {
		return 0
	}
	paceSecPerMeter := (float64(r.Time) / 10) / r.Distance
	return 2.80 / (paceSecPerMeter * paceSecPerMeter * paceSecPerMeter)
}

type resultResponse struct {
	Data Result `json:"data"`
}

type resultListResponse struct {
	Data []Result `json:"data"`
}

// ListResults fetches the most recent Concept2 Logbook results for the
// authenticated user. limit controls the page size (max varies by API).
func ListResults(ctx context.Context, client *http.Client, apiBase, accessToken string, limit int) ([]Result, error) {
	if client == nil {
		client = http.DefaultClient
	}
	url := fmt.Sprintf("%s/api/users/me/results?per_page=%d", apiBase, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("concept2 list results: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("concept2 list results: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("concept2 list results: status %d: %s", resp.StatusCode, bytes.TrimSpace(bodyBytes))
	}

	var wrapper resultListResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 512*1024)).Decode(&wrapper); err != nil {
		return nil, fmt.Errorf("concept2 list results: decode: %w", err)
	}
	return wrapper.Data, nil
}

// GetResult fetches a single Concept2 Logbook result by ID. It calls
// GET {apiBase}/api/users/me/results/{resultID} with the supplied Bearer token.
// The API wraps the result in a {"data": {...}} envelope.
func GetResult(ctx context.Context, client *http.Client, apiBase, accessToken string, resultID int64) (Result, error) {
	if client == nil {
		client = http.DefaultClient
	}
	url := fmt.Sprintf("%s/api/users/me/results/%d", apiBase, resultID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{}, fmt.Errorf("concept2 get result: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("concept2 get result: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Result{}, fmt.Errorf("concept2 get result: status %d: %s", resp.StatusCode, bytes.TrimSpace(bodyBytes))
	}

	var wrapper resultResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&wrapper); err != nil {
		return Result{}, fmt.Errorf("concept2 get result: decode: %w", err)
	}
	return wrapper.Data, nil
}
