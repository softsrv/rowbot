package concept2

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetResult_ParsesIntervalHeartRate guards the raw-body-logging change in
// GetResult: it switched from streaming json.NewDecoder(reader).Decode to
// io.ReadAll + json.Unmarshal so the raw body could be captured for a Debug
// log line before decoding. This confirms that restructuring didn't change
// what actually gets parsed — including nested per-interval heart rate, the
// exact field this was added to help diagnose reports of.
func TestGetResult_ParsesIntervalHeartRate(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"id": 42,
				"user_id": 7,
				"workout": {
					"intervals": [
						{"time": 1000, "distance": 250, "heart_rate": {"average": 150}}
					]
				}
			}
		}`))
	}))
	defer srv.Close()

	result, err := GetResult(context.Background(), srv.Client(), srv.URL, "fake-token", 42)
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if result.ID != 42 {
		t.Errorf("ID = %d, want 42", result.ID)
	}
	pieces := result.Workout.Pieces()
	if len(pieces) != 1 {
		t.Fatalf("got %d pieces, want 1", len(pieces))
	}
	if pieces[0].HeartRate == nil {
		t.Fatal("expected interval HeartRate to be non-nil")
	}
	if pieces[0].HeartRate.Average != 150 {
		t.Errorf("interval HeartRate.Average = %d, want 150", pieces[0].HeartRate.Average)
	}
}

// TestGetResult_IntervalHeartRateFallsBackToEnding is a distilled version of
// a real production delivery (2026-08-12, VariableInterval workout):
// Concept2 sent "average":0 for every interval despite a real reading being
// available in "ending". Confirms the fallback resolves through the full
// GetResult path, not just in an isolated Value() call.
func TestGetResult_IntervalHeartRateFallsBackToEnding(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"id": 119434929,
				"user_id": 1890109,
				"heart_rate": {"average": 120, "min": 0, "max": 0, "ending": 120, "recovery": 0},
				"workout": {
					"intervals": [
						{"time": 2479, "distance": 1000, "heart_rate": {"average": 0, "min": 0, "max": 0, "ending": 86, "recovery": 0}}
					]
				}
			}
		}`))
	}))
	defer srv.Close()

	result, err := GetResult(context.Background(), srv.Client(), srv.URL, "fake-token", 119434929)
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}

	if got := result.HeartRate.Value(); got != 120 {
		t.Errorf("top-level HeartRate.Value() = %d, want 120 (from average)", got)
	}

	pieces := result.Workout.Pieces()
	if len(pieces) != 1 {
		t.Fatalf("got %d pieces, want 1", len(pieces))
	}
	if got := pieces[0].HeartRate.Value(); got != 86 {
		t.Errorf("interval HeartRate.Value() = %d, want 86 (average was 0, should fall back to ending)", got)
	}
}

func TestHeartRateSummary_Value(t *testing.T) {
	tests := []struct {
		name string
		hr   HeartRateSummary
		want int
	}{
		{"average wins when non-zero", HeartRateSummary{Average: 145, Max: 160, Ending: 130, Recovery: 90}, 145},
		{"falls back to max when average is zero", HeartRateSummary{Average: 0, Max: 160, Ending: 130, Recovery: 90}, 160},
		{"falls back to ending when average and max are zero", HeartRateSummary{Average: 0, Max: 0, Ending: 130, Recovery: 90}, 130},
		{"falls back to recovery when average, max, and ending are zero", HeartRateSummary{Average: 0, Max: 0, Ending: 0, Recovery: 90}, 90},
		{"zero when everything is zero", HeartRateSummary{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.hr.Value(); got != tt.want {
				t.Errorf("Value() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResultAveragePace(t *testing.T) {
	tests := []struct {
		name     string
		distance float64
		time     int64
		want     int64
	}{
		{
			name:     "normal case",
			distance: 5000,
			time:     13477,
			want:     1347, // int64(13477 * 500 / 5000) = int64(1347.7)
		},
		{
			name:     "zero distance",
			distance: 0,
			time:     13477,
			want:     0,
		},
		{
			name:     "negative distance",
			distance: -100,
			time:     13477,
			want:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Result{Distance: tt.distance, Time: tt.time}
			if got := r.AveragePace(); got != tt.want {
				t.Errorf("AveragePace() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestResultWatts validates against Concept2's own worked example
// (https://www.concept2.com/training/watts-calculator): a 2:05/500m split
// (125 seconds/500m, a 0.25 sec/m pace) yields (2.80/0.25³) = 179.2 watts.
func TestResultWatts(t *testing.T) {
	tests := []struct {
		name     string
		distance float64
		time     int64
		want     float64
	}{
		{
			name:     "concept2 worked example: 2:05/500m",
			distance: 500,
			time:     1250, // tenths of a second = 125.0 seconds
			want:     179.2,
		},
		{
			name:     "zero distance",
			distance: 0,
			time:     1250,
			want:     0,
		},
		{
			name:     "zero time",
			distance: 500,
			time:     0,
			want:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Result{Distance: tt.distance, Time: tt.time}
			if got := r.Watts(); math.Abs(got-tt.want) > 0.1 {
				t.Errorf("Watts() = %f, want %f", got, tt.want)
			}
		})
	}
}

// TestSplitWatts mirrors TestResultWatts for Split.Watts(), using the same
// Concept2 worked example.
func TestSplitWatts(t *testing.T) {
	tests := []struct {
		name     string
		distance float64
		time     int64
		want     float64
	}{
		{
			name:     "concept2 worked example: 2:05/500m",
			distance: 500,
			time:     1250, // tenths of a second = 125.0 seconds
			want:     179.2,
		},
		{
			name:     "zero distance",
			distance: 0,
			time:     1250,
			want:     0,
		},
		{
			name:     "zero time",
			distance: 500,
			time:     0,
			want:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Split{Distance: tt.distance, Time: tt.time}
			if got := s.Watts(); math.Abs(got-tt.want) > 0.1 {
				t.Errorf("Watts() = %f, want %f", got, tt.want)
			}
		})
	}
}
