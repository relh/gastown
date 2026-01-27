// Package witness provides the polecat monitoring agent.
package witness

import (
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Circuit breaker configuration defaults.
// These can be overridden via rig config.
const (
	DefaultMaxFailures    = 3               // Failures before circuit trips
	DefaultCooldownPeriod = 5 * time.Minute // Time before half-open retry
)

// CircuitBreakerConfig holds configurable parameters for circuit breaker behavior.
type CircuitBreakerConfig struct {
	MaxFailures    int           `json:"max_failures"`    // Consecutive failures to trip circuit
	CooldownPeriod time.Duration `json:"cooldown_period"` // Cooldown before half-open state
}

// DefaultCircuitBreakerConfig returns the default circuit breaker config.
func DefaultCircuitBreakerConfig() *CircuitBreakerConfig {
	return &CircuitBreakerConfig{
		MaxFailures:    DefaultMaxFailures,
		CooldownPeriod: DefaultCooldownPeriod,
	}
}

// CircuitBreakerResult represents the outcome of processing a tripped circuit.
type CircuitBreakerResult struct {
	PolecatName   string
	AgentBeadID   string
	CircuitState  string
	FailureCount  int
	Action        string // "requeued", "nuked", "escalated", "half_open"
	WispID        string // ID of requeued work wisp (if applicable)
	MailSent      string // ID of sent mail (if applicable)
	Error         error
}

// CheckCircuitBreakers scans all polecat agent beads for tripped circuits.
// For each tripped (open) circuit:
//   - Auto-nukes the polecat (work already failed, no recovery)
//   - Sends WORK_REQUEUE mail to get fresh polecat assignment
//
// Returns results for each processed circuit breaker.
func CheckCircuitBreakers(workDir, rigName string, config *CircuitBreakerConfig, router *mail.Router) []*CircuitBreakerResult {
	if config == nil {
		config = DefaultCircuitBreakerConfig()
	}

	var results []*CircuitBreakerResult

	// Find town root
	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		return results
	}

	// Get beads wrapper
	bd := beads.NewWithBeadsDir(workDir, beads.ResolveBeadsDir(workDir))

	// List all agent beads
	agentBeads, err := bd.ListAgentBeads()
	if err != nil {
		return results
	}

	// Check each polecat agent bead for tripped circuits
	for id, issue := range agentBeads {
		fields := beads.ParseAgentFields(issue.Description)

		// Only process polecat agents in this rig
		if fields.RoleType != "polecat" || fields.Rig != rigName {
			continue
		}

		// Skip if circuit is not open (tripped)
		if fields.CircuitState != beads.CircuitOpen {
			continue
		}

		result := &CircuitBreakerResult{
			PolecatName:  extractPolecatName(id),
			AgentBeadID:  id,
			CircuitState: fields.CircuitState,
			FailureCount: fields.FailureCount,
		}

		// Process the tripped circuit
		processTrippedCircuit(workDir, rigName, id, fields, config, router, result)

		results = append(results, result)
	}

	return results
}

// processTrippedCircuit handles a polecat with a tripped circuit breaker.
// This implements the isolation pattern: nuke the failing polecat and requeue the work.
func processTrippedCircuit(
	workDir, rigName, agentBeadID string,
	fields *beads.AgentFields,
	config *CircuitBreakerConfig,
	router *mail.Router,
	result *CircuitBreakerResult,
) {
	// If there's hooked work, requeue it before nuking
	if fields.HookBead != "" {
		mailID, err := sendWorkRequeue(router, rigName, result.PolecatName, fields.HookBead, fields.FailureCount)
		if err != nil {
			result.Error = fmt.Errorf("sending WORK_REQUEUE: %w", err)
			result.Action = "error"
			return
		}
		result.MailSent = mailID
	}

	// Auto-nuke the polecat (circuit tripped = irrecoverable failure)
	nukeResult := AutoNukeIfClean(workDir, rigName, result.PolecatName)
	if nukeResult.Error != nil {
		result.Error = nukeResult.Error
		result.Action = "escalated"
		// Escalate to Mayor for manual intervention
		if router != nil {
			escalateTrippedCircuit(router, rigName, result.PolecatName, fields, nukeResult.Reason)
		}
		return
	}

	if nukeResult.Nuked {
		result.Action = "requeued"
	} else {
		// Couldn't nuke (dirty state) - escalate
		result.Action = "escalated"
		if router != nil {
			escalateTrippedCircuit(router, rigName, result.PolecatName, fields, nukeResult.Reason)
		}
	}
}

// sendWorkRequeue sends a WORK_REQUEUE message to get fresh polecat assignment.
// The work is NOT retried by the same polecat - a new polecat will be assigned.
func sendWorkRequeue(router *mail.Router, rigName, polecatName, hookBeadID string, failureCount int) (string, error) {
	if router == nil {
		return "", fmt.Errorf("router is nil")
	}

	msg := &mail.Message{
		From:     fmt.Sprintf("%s/witness", rigName),
		To:       "mayor/",
		Subject:  fmt.Sprintf("WORK_REQUEUE %s", hookBeadID),
		Priority: mail.PriorityHigh,
		Type:     mail.TypeTask,
		Body: fmt.Sprintf(`Work needs reassignment after circuit breaker trip.

Bead: %s
Previous Polecat: %s/%s
Failure Count: %d
Reason: Circuit breaker tripped after consecutive failures

The previous polecat has been nuked. Please assign this work to a fresh polecat.
This is NOT a retry - the work may need investigation for consistent failures.`,
			hookBeadID,
			rigName,
			polecatName,
			failureCount,
		),
	}

	if err := router.Send(msg); err != nil {
		return "", err
	}

	return msg.ID, nil
}

// escalateTrippedCircuit sends an escalation to Mayor when a tripped polecat
// couldn't be auto-nuked (usually due to dirty state).
func escalateTrippedCircuit(router *mail.Router, rigName, polecatName string, fields *beads.AgentFields, reason string) {
	msg := &mail.Message{
		From:     fmt.Sprintf("%s/witness", rigName),
		To:       "mayor/",
		Subject:  fmt.Sprintf("CIRCUIT_BREAKER_ESCALATION %s/%s", rigName, polecatName),
		Priority: mail.PriorityUrgent,
		Type:     mail.TypeTask,
		Body: fmt.Sprintf(`Circuit breaker tripped but couldn't auto-nuke polecat.

Polecat: %s/%s
Circuit State: %s
Failure Count: %d
Cleanup Status: %s
Hook Bead: %s
Nuke Blocked: %s

Please investigate and manually resolve:
1. Check if polecat has valuable uncommitted work
2. Either recover work or authorize force-nuke
3. Reassign the work (if any) to a fresh polecat`,
			rigName,
			polecatName,
			fields.CircuitState,
			fields.FailureCount,
			fields.CleanupStatus,
			fields.HookBead,
			reason,
		),
	}

	_ = router.Send(msg)
}

// extractPolecatName extracts the polecat name from an agent bead ID.
// ID format: <prefix>-<rig>-polecat-<name>
func extractPolecatName(agentBeadID string) string {
	// Simple extraction - find last part after "-polecat-"
	const marker = "-polecat-"
	idx := len(agentBeadID) - 1
	for i := len(agentBeadID) - len(marker); i >= 0; i-- {
		if agentBeadID[i:i+len(marker)] == marker {
			return agentBeadID[i+len(marker):]
		}
	}
	// Fallback to last part
	for i := idx; i >= 0; i-- {
		if agentBeadID[i] == '-' {
			return agentBeadID[i+1:]
		}
	}
	return agentBeadID
}

// HandlePolecatFailure processes a polecat exit that indicates failure.
// This is called by HandlePolecatDone when Exit is ESCALATED or DEFERRED.
// Returns the new failure count and whether the circuit was tripped.
func HandlePolecatFailure(workDir, rigName, polecatName string, maxFailures int) (int, bool, error) {
	// Find town root
	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		return 0, false, fmt.Errorf("finding town root: %v", err)
	}

	// Construct agent bead ID
	prefix := beads.GetPrefixForRig(townRoot, rigName)
	agentBeadID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)

	// Get beads wrapper
	bd := beads.NewWithBeadsDir(workDir, beads.ResolveBeadsDir(workDir))

	// Increment failure count and potentially trip circuit
	return bd.IncrementAgentFailureCount(agentBeadID, maxFailures)
}

// HandlePolecatSuccess resets the failure count for a successful completion.
// This is called by HandlePolecatDone when Exit is COMPLETED.
func HandlePolecatSuccess(workDir, rigName, polecatName string) error {
	// Find town root
	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		return fmt.Errorf("finding town root: %v", err)
	}

	// Construct agent bead ID
	prefix := beads.GetPrefixForRig(townRoot, rigName)
	agentBeadID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)

	// Get beads wrapper
	bd := beads.NewWithBeadsDir(workDir, beads.ResolveBeadsDir(workDir))

	// Reset failure count
	return bd.ResetAgentFailureCount(agentBeadID)
}

// SetHalfOpenState transitions a polecat's circuit to half-open state.
// This allows a single retry after cooldown period expires.
func SetHalfOpenState(workDir, rigName, polecatName string) error {
	// Find town root
	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		return fmt.Errorf("finding town root: %v", err)
	}

	// Construct agent bead ID
	prefix := beads.GetPrefixForRig(townRoot, rigName)
	agentBeadID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)

	// Get beads wrapper
	bd := beads.NewWithBeadsDir(workDir, beads.ResolveBeadsDir(workDir))

	// Update to half-open state
	return bd.UpdateAgentCircuitState(agentBeadID, beads.CircuitHalfOpen)
}
