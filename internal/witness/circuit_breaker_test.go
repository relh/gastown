package witness

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestExtractPolecatName(t *testing.T) {
	tests := []struct {
		name     string
		beadID   string
		expected string
	}{
		{
			name:     "standard format",
			beadID:   "gs-gastown-polecat-nux",
			expected: "nux",
		},
		{
			name:     "with hyphen in name",
			beadID:   "gs-gastown-polecat-test-agent",
			expected: "test-agent",
		},
		{
			name:     "different prefix",
			beadID:   "bd-beads-polecat-worker",
			expected: "worker",
		},
		{
			name:     "no marker found",
			beadID:   "some-random-id",
			expected: "id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractPolecatName(tt.beadID)
			if result != tt.expected {
				t.Errorf("extractPolecatName(%q) = %q, want %q", tt.beadID, result, tt.expected)
			}
		})
	}
}

func TestDefaultCircuitBreakerConfig(t *testing.T) {
	config := DefaultCircuitBreakerConfig()

	if config.MaxFailures != DefaultMaxFailures {
		t.Errorf("MaxFailures = %d, want %d", config.MaxFailures, DefaultMaxFailures)
	}

	if config.CooldownPeriod != DefaultCooldownPeriod {
		t.Errorf("CooldownPeriod = %v, want %v", config.CooldownPeriod, DefaultCooldownPeriod)
	}
}

func TestCircuitBreakerConstants(t *testing.T) {
	// Verify circuit breaker constants match beads package
	if beads.CircuitClosed != "closed" {
		t.Errorf("CircuitClosed = %q, want %q", beads.CircuitClosed, "closed")
	}
	if beads.CircuitOpen != "open" {
		t.Errorf("CircuitOpen = %q, want %q", beads.CircuitOpen, "open")
	}
	if beads.CircuitHalfOpen != "half_open" {
		t.Errorf("CircuitHalfOpen = %q, want %q", beads.CircuitHalfOpen, "half_open")
	}
}

func TestParseWorkRequeue(t *testing.T) {
	tests := []struct {
		name        string
		subject     string
		body        string
		wantBeadID  string
		wantPolecat string
		wantCount   int
		wantErr     bool
	}{
		{
			name:    "valid message",
			subject: "WORK_REQUEUE gs-abc123",
			body: `Work needs reassignment after circuit breaker trip.

Bead: gs-abc123
Previous Polecat: gastown/nux
Failure Count: 3
Reason: Circuit breaker tripped after consecutive failures`,
			wantBeadID:  "gs-abc123",
			wantPolecat: "gastown/nux",
			wantCount:   3,
			wantErr:     false,
		},
		{
			name:    "invalid subject",
			subject: "INVALID_MESSAGE",
			body:    "",
			wantErr: true,
		},
		{
			name:        "minimal message",
			subject:     "WORK_REQUEUE bd-xyz",
			body:        "",
			wantBeadID:  "bd-xyz",
			wantPolecat: "",
			wantCount:   0,
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := ParseWorkRequeue(tt.subject, tt.body)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if payload.BeadID != tt.wantBeadID {
				t.Errorf("BeadID = %q, want %q", payload.BeadID, tt.wantBeadID)
			}
			if payload.PreviousPolecat != tt.wantPolecat {
				t.Errorf("PreviousPolecat = %q, want %q", payload.PreviousPolecat, tt.wantPolecat)
			}
			if payload.FailureCount != tt.wantCount {
				t.Errorf("FailureCount = %d, want %d", payload.FailureCount, tt.wantCount)
			}
		})
	}
}
