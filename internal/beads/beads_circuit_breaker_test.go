package beads

import (
	"testing"
)

// TestCircuitBreakerConstants verifies the circuit breaker state constants.
func TestCircuitBreakerConstants(t *testing.T) {
	if CircuitClosed != "closed" {
		t.Errorf("CircuitClosed = %q, want closed", CircuitClosed)
	}
	if CircuitOpen != "open" {
		t.Errorf("CircuitOpen = %q, want open", CircuitOpen)
	}
	if CircuitHalfOpen != "half_open" {
		t.Errorf("CircuitHalfOpen = %q, want half_open", CircuitHalfOpen)
	}
	if DefaultMaxFailures != 3 {
		t.Errorf("DefaultMaxFailures = %d, want 3", DefaultMaxFailures)
	}
}

// TestParseAgentFieldsCircuitBreaker verifies parsing of circuit breaker fields.
func TestParseAgentFieldsCircuitBreaker(t *testing.T) {
	tests := []struct {
		name         string
		description  string
		wantCount    int
		wantState    string
	}{
		{
			name: "with failure count and circuit state",
			description: `Test Agent

role_type: polecat
rig: testrig
agent_state: working
hook_bead: null
cleanup_status: null
active_mr: null
notification_level: null
failure_count: 2
circuit_state: half_open`,
			wantCount: 2,
			wantState: "half_open",
		},
		{
			name: "with zero failure count",
			description: `Test Agent

role_type: polecat
failure_count: 0
circuit_state: closed`,
			wantCount: 0,
			wantState: "closed",
		},
		{
			name: "with high failure count and open circuit",
			description: `Test Agent

failure_count: 5
circuit_state: open`,
			wantCount: 5,
			wantState: "open",
		},
		{
			name: "no circuit breaker fields (legacy)",
			description: `Test Agent

role_type: polecat
agent_state: working`,
			wantCount: 0,
			wantState: "",
		},
		{
			name: "invalid failure count",
			description: `Test Agent

failure_count: not-a-number
circuit_state: closed`,
			wantCount: 0, // Should default to 0 on parse error
			wantState: "closed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields := ParseAgentFields(tt.description)
			if fields.FailureCount != tt.wantCount {
				t.Errorf("FailureCount = %d, want %d", fields.FailureCount, tt.wantCount)
			}
			if fields.CircuitState != tt.wantState {
				t.Errorf("CircuitState = %q, want %q", fields.CircuitState, tt.wantState)
			}
		})
	}
}

// TestFormatAgentDescriptionCircuitBreaker verifies formatting of circuit breaker fields.
func TestFormatAgentDescriptionCircuitBreaker(t *testing.T) {
	tests := []struct {
		name   string
		fields *AgentFields
		wantFC string // expected failure_count line
		wantCS string // expected circuit_state line
	}{
		{
			name: "closed circuit with zero failures",
			fields: &AgentFields{
				RoleType:     "polecat",
				FailureCount: 0,
				CircuitState: CircuitClosed,
			},
			wantFC: "failure_count: 0",
			wantCS: "circuit_state: closed",
		},
		{
			name: "open circuit with failures",
			fields: &AgentFields{
				RoleType:     "polecat",
				FailureCount: 3,
				CircuitState: CircuitOpen,
			},
			wantFC: "failure_count: 3",
			wantCS: "circuit_state: open",
		},
		{
			name: "half-open circuit",
			fields: &AgentFields{
				RoleType:     "polecat",
				FailureCount: 2,
				CircuitState: CircuitHalfOpen,
			},
			wantFC: "failure_count: 2",
			wantCS: "circuit_state: half_open",
		},
		{
			name: "empty circuit state defaults to closed",
			fields: &AgentFields{
				RoleType:     "polecat",
				FailureCount: 1,
				CircuitState: "",
			},
			wantFC: "failure_count: 1",
			wantCS: "circuit_state: closed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			desc := FormatAgentDescription("Test Agent", tt.fields)
			if !containsLine(desc, tt.wantFC) {
				t.Errorf("description missing %q:\n%s", tt.wantFC, desc)
			}
			if !containsLine(desc, tt.wantCS) {
				t.Errorf("description missing %q:\n%s", tt.wantCS, desc)
			}
		})
	}
}

// TestAgentFieldUpdatesCircuitBreaker verifies the AgentFieldUpdates struct.
func TestAgentFieldUpdatesCircuitBreaker(t *testing.T) {
	// Verify the struct has the new fields
	count := 5
	state := CircuitOpen
	updates := AgentFieldUpdates{
		FailureCount: &count,
		CircuitState: &state,
	}

	if updates.FailureCount == nil || *updates.FailureCount != 5 {
		t.Errorf("FailureCount not set correctly")
	}
	if updates.CircuitState == nil || *updates.CircuitState != CircuitOpen {
		t.Errorf("CircuitState not set correctly")
	}
}

// TestCircuitBreakerRoundTrip verifies that circuit breaker fields survive
// a format→parse round trip.
func TestCircuitBreakerRoundTrip(t *testing.T) {
	original := &AgentFields{
		RoleType:     "polecat",
		Rig:          "testrig",
		AgentState:   "working",
		FailureCount: 7,
		CircuitState: CircuitHalfOpen,
	}

	// Format to description
	desc := FormatAgentDescription("Test Agent", original)

	// Parse back
	parsed := ParseAgentFields(desc)

	if parsed.FailureCount != original.FailureCount {
		t.Errorf("FailureCount round-trip: got %d, want %d",
			parsed.FailureCount, original.FailureCount)
	}
	if parsed.CircuitState != original.CircuitState {
		t.Errorf("CircuitState round-trip: got %q, want %q",
			parsed.CircuitState, original.CircuitState)
	}
}

// containsLine checks if a string contains a specific line.
func containsLine(s, line string) bool {
	for _, l := range splitLines(s) {
		if l == line {
			return true
		}
	}
	return false
}

// splitLines splits a string into lines, trimming each.
func splitLines(s string) []string {
	var lines []string
	for _, line := range split(s, "\n") {
		lines = append(lines, trim(line))
	}
	return lines
}

// split is a simple string split helper.
func split(s, sep string) []string {
	var result []string
	for len(s) > 0 {
		idx := indexOf(s, sep)
		if idx == -1 {
			result = append(result, s)
			break
		}
		result = append(result, s[:idx])
		s = s[idx+len(sep):]
	}
	return result
}

// indexOf returns the index of substr in s, or -1 if not found.
func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// trim removes leading and trailing whitespace.
func trim(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t' || s[start] == '\r') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
