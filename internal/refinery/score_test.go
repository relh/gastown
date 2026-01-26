package refinery

import (
	"testing"
	"time"
)

func TestScoreMR_Deterministic(t *testing.T) {
	// Same inputs should produce same output every time
	now := time.Date(2025, 12, 17, 10, 0, 0, 0, time.UTC)
	convoyCreated := time.Date(2025, 12, 16, 10, 0, 0, 0, time.UTC)

	input := ScoreInput{
		Priority:        2,
		MRCreatedAt:     time.Date(2025, 12, 17, 9, 0, 0, 0, time.UTC),
		ConvoyCreatedAt: &convoyCreated,
		RetryCount:      1,
		Now:             now,
	}
	config := DefaultScoreConfig()

	score1 := ScoreMR(input, config)
	score2 := ScoreMR(input, config)
	score3 := ScoreMR(input, config)

	if score1 != score2 || score2 != score3 {
		t.Errorf("Same inputs produced different scores: %f, %f, %f", score1, score2, score3)
	}
}

func TestScoreMR_DeterministicWithDefaults(t *testing.T) {
	// ScoreMRWithDefaults should also be deterministic
	now := time.Date(2025, 12, 17, 10, 0, 0, 0, time.UTC)

	input := ScoreInput{
		Priority:    1,
		MRCreatedAt: time.Date(2025, 12, 17, 8, 0, 0, 0, time.UTC),
		RetryCount:  0,
		Now:         now,
	}

	score1 := ScoreMRWithDefaults(input)
	score2 := ScoreMRWithDefaults(input)

	if score1 != score2 {
		t.Errorf("Same inputs produced different scores: %f != %f", score1, score2)
	}
}

func TestMRInfo_ScoreAt_Deterministic(t *testing.T) {
	// MRInfo.ScoreAt should be deterministic for same time
	convoyCreated := time.Date(2025, 12, 15, 10, 0, 0, 0, time.UTC)
	mr := &MRInfo{
		Priority:        0,
		CreatedAt:       time.Date(2025, 12, 17, 9, 30, 0, 0, time.UTC),
		ConvoyCreatedAt: &convoyCreated,
		RetryCount:      2,
	}

	now := time.Date(2025, 12, 17, 12, 0, 0, 0, time.UTC)
	score1 := mr.ScoreAt(now)
	score2 := mr.ScoreAt(now)

	if score1 != score2 {
		t.Errorf("ScoreAt with same time produced different scores: %f != %f", score1, score2)
	}
}

func TestScoreMR_DifferentInputsProduceDifferentScores(t *testing.T) {
	now := time.Date(2025, 12, 17, 10, 0, 0, 0, time.UTC)
	config := DefaultScoreConfig()

	baseInput := ScoreInput{
		Priority:    2,
		MRCreatedAt: time.Date(2025, 12, 17, 9, 0, 0, 0, time.UTC),
		RetryCount:  0,
		Now:         now,
	}

	tests := []struct {
		name   string
		modify func(ScoreInput) ScoreInput
	}{
		{
			name: "different priority",
			modify: func(i ScoreInput) ScoreInput {
				i.Priority = 0
				return i
			},
		},
		{
			name: "different MR age",
			modify: func(i ScoreInput) ScoreInput {
				i.MRCreatedAt = time.Date(2025, 12, 17, 6, 0, 0, 0, time.UTC)
				return i
			},
		},
		{
			name: "different retry count",
			modify: func(i ScoreInput) ScoreInput {
				i.RetryCount = 3
				return i
			},
		},
		{
			name: "different now time",
			modify: func(i ScoreInput) ScoreInput {
				i.Now = time.Date(2025, 12, 17, 14, 0, 0, 0, time.UTC)
				return i
			},
		},
		{
			name: "with convoy age",
			modify: func(i ScoreInput) ScoreInput {
				convoyTime := time.Date(2025, 12, 16, 0, 0, 0, 0, time.UTC)
				i.ConvoyCreatedAt = &convoyTime
				return i
			},
		},
	}

	baseScore := ScoreMR(baseInput, config)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modifiedInput := tt.modify(baseInput)
			modifiedScore := ScoreMR(modifiedInput, config)

			if baseScore == modifiedScore {
				t.Errorf("Different inputs produced same score: base=%f, modified=%f", baseScore, modifiedScore)
			}
		})
	}
}

func TestScoreMR_DifferentConfigsProduceDifferentScores(t *testing.T) {
	now := time.Date(2025, 12, 17, 10, 0, 0, 0, time.UTC)
	convoyCreated := time.Date(2025, 12, 16, 10, 0, 0, 0, time.UTC)

	input := ScoreInput{
		Priority:        2,
		MRCreatedAt:     time.Date(2025, 12, 17, 9, 0, 0, 0, time.UTC),
		ConvoyCreatedAt: &convoyCreated,
		RetryCount:      1,
		Now:             now,
	}

	config1 := DefaultScoreConfig()
	config2 := ScoreConfig{
		BaseScore:       2000.0, // Different base
		ConvoyAgeWeight: 10.0,
		PriorityWeight:  100.0,
		RetryPenalty:    50.0,
		MRAgeWeight:     1.0,
		MaxRetryPenalty: 300.0,
	}

	score1 := ScoreMR(input, config1)
	score2 := ScoreMR(input, config2)

	if score1 == score2 {
		t.Errorf("Different configs produced same score: %f", score1)
	}
}

func TestScoreMR_ReproducibleAcrossRuns(t *testing.T) {
	// This test documents that scores are reproducible across multiple test runs
	// by using fixed inputs and verifying against expected values
	now := time.Date(2025, 12, 17, 12, 0, 0, 0, time.UTC)
	convoyCreated := time.Date(2025, 12, 16, 12, 0, 0, 0, time.UTC) // 24 hours ago

	input := ScoreInput{
		Priority:        2,                                              // P2 = +200 (4-2)*100
		MRCreatedAt:     time.Date(2025, 12, 17, 10, 0, 0, 0, time.UTC), // 2 hours ago = +2
		ConvoyCreatedAt: &convoyCreated,                                 // 24 hours = +240
		RetryCount:      1,                                              // -50
		Now:             now,
	}
	config := DefaultScoreConfig()

	// Expected: 1000 (base) + 240 (convoy) + 200 (priority) - 50 (retry) + 2 (MR age) = 1392
	expected := 1392.0
	actual := ScoreMR(input, config)

	if actual != expected {
		t.Errorf("Score mismatch: got %f, want %f", actual, expected)
	}
}

func TestScoreMR_PriorityOrdering(t *testing.T) {
	// P0 should score higher than P1, which should score higher than P2, etc.
	now := time.Date(2025, 12, 17, 10, 0, 0, 0, time.UTC)
	config := DefaultScoreConfig()

	var scores [5]float64
	for priority := 0; priority <= 4; priority++ {
		input := ScoreInput{
			Priority:    priority,
			MRCreatedAt: now,
			RetryCount:  0,
			Now:         now,
		}
		scores[priority] = ScoreMR(input, config)
	}

	for i := 0; i < 4; i++ {
		if scores[i] <= scores[i+1] {
			t.Errorf("P%d score (%f) should be > P%d score (%f)", i, scores[i], i+1, scores[i+1])
		}
	}
}

func TestScoreMR_RetryPenaltyCapped(t *testing.T) {
	// Retry penalty should be capped at MaxRetryPenalty
	now := time.Date(2025, 12, 17, 10, 0, 0, 0, time.UTC)
	config := DefaultScoreConfig()

	// With default config: RetryPenalty=50, MaxRetryPenalty=300
	// At 6 retries: 6*50=300 (at cap)
	// At 10 retries: still capped at 300

	input6 := ScoreInput{
		Priority:    2,
		MRCreatedAt: now,
		RetryCount:  6,
		Now:         now,
	}
	input10 := ScoreInput{
		Priority:    2,
		MRCreatedAt: now,
		RetryCount:  10,
		Now:         now,
	}

	score6 := ScoreMR(input6, config)
	score10 := ScoreMR(input10, config)

	if score6 != score10 {
		t.Errorf("Scores should be equal when retry penalty is capped: 6 retries=%f, 10 retries=%f", score6, score10)
	}
}

func TestScoreMR_ConvoyAgeIncreasesPriority(t *testing.T) {
	// Older convoys should have higher scores (anti-starvation)
	now := time.Date(2025, 12, 17, 10, 0, 0, 0, time.UTC)
	config := DefaultScoreConfig()

	newConvoy := time.Date(2025, 12, 17, 9, 0, 0, 0, time.UTC)  // 1 hour ago
	oldConvoy := time.Date(2025, 12, 15, 10, 0, 0, 0, time.UTC) // 48 hours ago

	inputNew := ScoreInput{
		Priority:        2,
		MRCreatedAt:     now,
		ConvoyCreatedAt: &newConvoy,
		RetryCount:      0,
		Now:             now,
	}
	inputOld := ScoreInput{
		Priority:        2,
		MRCreatedAt:     now,
		ConvoyCreatedAt: &oldConvoy,
		RetryCount:      0,
		Now:             now,
	}

	scoreNew := ScoreMR(inputNew, config)
	scoreOld := ScoreMR(inputOld, config)

	if scoreOld <= scoreNew {
		t.Errorf("Older convoy (%f) should score higher than newer convoy (%f)", scoreOld, scoreNew)
	}
}

func TestScoreMR_NilConvoyHandled(t *testing.T) {
	// MRs without a convoy should still score correctly
	now := time.Date(2025, 12, 17, 10, 0, 0, 0, time.UTC)
	config := DefaultScoreConfig()

	input := ScoreInput{
		Priority:        2,
		MRCreatedAt:     time.Date(2025, 12, 17, 9, 0, 0, 0, time.UTC),
		ConvoyCreatedAt: nil, // No convoy
		RetryCount:      0,
		Now:             now,
	}

	// Should not panic and should produce a valid score
	score := ScoreMR(input, config)

	// Expected: 1000 (base) + 0 (no convoy) + 200 (P2) - 0 (no retries) + 1 (1 hour MR age) = 1201
	expected := 1201.0
	if score != expected {
		t.Errorf("Score with nil convoy: got %f, want %f", score, expected)
	}
}

func TestScoreMR_ZeroTimeUsesNow(t *testing.T) {
	// When Now is zero, should use time.Now() - verify it doesn't panic
	input := ScoreInput{
		Priority:    2,
		MRCreatedAt: time.Now().Add(-time.Hour),
		RetryCount:  0,
		Now:         time.Time{}, // Zero value
	}
	config := DefaultScoreConfig()

	// Should not panic
	score := ScoreMR(input, config)

	// Score should be reasonable (base + priority + small MR age)
	if score < config.BaseScore {
		t.Errorf("Score with zero Now should be at least BaseScore: got %f", score)
	}
}

func TestDefaultScoreConfig_Values(t *testing.T) {
	// Verify default config values are as documented
	config := DefaultScoreConfig()

	if config.BaseScore != 1000.0 {
		t.Errorf("BaseScore: got %f, want 1000.0", config.BaseScore)
	}
	if config.ConvoyAgeWeight != 10.0 {
		t.Errorf("ConvoyAgeWeight: got %f, want 10.0", config.ConvoyAgeWeight)
	}
	if config.PriorityWeight != 100.0 {
		t.Errorf("PriorityWeight: got %f, want 100.0", config.PriorityWeight)
	}
	if config.RetryPenalty != 50.0 {
		t.Errorf("RetryPenalty: got %f, want 50.0", config.RetryPenalty)
	}
	if config.MRAgeWeight != 1.0 {
		t.Errorf("MRAgeWeight: got %f, want 1.0", config.MRAgeWeight)
	}
	if config.MaxRetryPenalty != 300.0 {
		t.Errorf("MaxRetryPenalty: got %f, want 300.0", config.MaxRetryPenalty)
	}
}
