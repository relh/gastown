package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

// BeadsMessage represents a message from gt mail inbox --json.
type BeadsMessage struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	To       string `json:"to"`
	Subject  string `json:"subject"`
	Body     string `json:"body"`
	Read     bool   `json:"read"`
	Priority string `json:"priority"`
	Type     string `json:"type"`
}

// ProcessLifecycleRequests checks for and processes lifecycle requests from the deacon inbox.
func (d *Daemon) ProcessLifecycleRequests() {
	// Get mail for deacon identity (using gt mail, not bd mail)
	cmd := exec.Command("gt", "mail", "inbox", "--identity", "deacon/", "--json")
	cmd.Dir = d.config.TownRoot

	output, err := cmd.Output()
	if err != nil {
		// gt mail might not be available or inbox empty
		return
	}

	if len(output) == 0 || string(output) == "[]" || string(output) == "[]\n" {
		return
	}

	var messages []BeadsMessage
	if err := json.Unmarshal(output, &messages); err != nil {
		d.logger.Printf("Error parsing mail: %v", err)
		return
	}

	for _, msg := range messages {
		if msg.Read {
			continue // Already processed
		}

		request := d.parseLifecycleRequest(&msg)
		if request == nil {
			continue // Not a lifecycle request
		}

		d.logger.Printf("Processing lifecycle request from %s: %s", request.From, request.Action)

		if err := d.executeLifecycleAction(request); err != nil {
			d.logger.Printf("Error executing lifecycle action: %v", err)
			continue
		}

		// Mark message as read (close the issue)
		if err := d.closeMessage(msg.ID); err != nil {
			d.logger.Printf("Warning: failed to close message %s: %v", msg.ID, err)
		}
	}
}

// LifecycleBody is the structured body format for lifecycle requests.
// Claude should send mail with JSON body: {"action": "cycle"} or {"action": "shutdown"}
type LifecycleBody struct {
	Action string `json:"action"`
}

// parseLifecycleRequest extracts a lifecycle request from a message.
// Uses structured body parsing instead of keyword matching on subject.
func (d *Daemon) parseLifecycleRequest(msg *BeadsMessage) *LifecycleRequest {
	// Gate: subject must start with "LIFECYCLE:"
	subject := strings.ToLower(msg.Subject)
	if !strings.HasPrefix(subject, "lifecycle:") {
		return nil
	}

	// Parse structured body for action
	var body LifecycleBody
	if err := json.Unmarshal([]byte(msg.Body), &body); err != nil {
		// Fallback: check for simple action strings in body
		bodyLower := strings.ToLower(strings.TrimSpace(msg.Body))
		switch {
		case bodyLower == "restart" || bodyLower == "action: restart":
			body.Action = "restart"
		case bodyLower == "shutdown" || bodyLower == "action: shutdown" || bodyLower == "stop":
			body.Action = "shutdown"
		case bodyLower == "cycle" || bodyLower == "action: cycle":
			body.Action = "cycle"
		default:
			d.logger.Printf("Lifecycle request with unparseable body: %q", msg.Body)
			return nil
		}
	}

	// Map action string to enum
	var action LifecycleAction
	switch strings.ToLower(body.Action) {
	case "restart":
		action = ActionRestart
	case "shutdown", "stop":
		action = ActionShutdown
	case "cycle":
		action = ActionCycle
	default:
		d.logger.Printf("Unknown lifecycle action: %q", body.Action)
		return nil
	}

	return &LifecycleRequest{
		From:      msg.From,
		Action:    action,
		Timestamp: time.Now(),
	}
}

// executeLifecycleAction performs the requested lifecycle action.
func (d *Daemon) executeLifecycleAction(request *LifecycleRequest) error {
	// Determine session name from sender identity
	sessionName := d.identityToSession(request.From)
	if sessionName == "" {
		return fmt.Errorf("unknown agent identity: %s", request.From)
	}

	d.logger.Printf("Executing %s for session %s", request.Action, sessionName)

	// Verify agent state shows requesting_<action>=true before killing
	if err := d.verifyAgentRequestingState(request.From, request.Action); err != nil {
		return fmt.Errorf("state verification failed: %w", err)
	}

	// Check if session exists
	running, err := d.tmux.HasSession(sessionName)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}

	switch request.Action {
	case ActionShutdown:
		if running {
			if err := d.tmux.KillSession(sessionName); err != nil {
				return fmt.Errorf("killing session: %w", err)
			}
			d.logger.Printf("Killed session %s", sessionName)
		}
		return nil

	case ActionCycle, ActionRestart:
		if running {
			// Kill the session first
			if err := d.tmux.KillSession(sessionName); err != nil {
				return fmt.Errorf("killing session: %w", err)
			}
			d.logger.Printf("Killed session %s for restart", sessionName)

			// Wait a moment
			time.Sleep(500 * time.Millisecond)
		}

		// Restart the session
		if err := d.restartSession(sessionName, request.From); err != nil {
			return fmt.Errorf("restarting session: %w", err)
		}
		d.logger.Printf("Restarted session %s", sessionName)

		// Clear the requesting state so we don't cycle again
		if err := d.clearAgentRequestingState(request.From, request.Action); err != nil {
			d.logger.Printf("Warning: failed to clear agent state: %v", err)
		}
		return nil

	default:
		return fmt.Errorf("unknown action: %s", request.Action)
	}
}

// identityToSession converts a beads identity to a tmux session name.
func (d *Daemon) identityToSession(identity string) string {
	// Handle known identities
	switch identity {
	case "mayor":
		return "gt-mayor"
	default:
		// Pattern: <rig>-witness → gt-<rig>-witness
		if strings.HasSuffix(identity, "-witness") {
			return "gt-" + identity
		}
		// Pattern: <rig>-refinery → gt-<rig>-refinery
		if strings.HasSuffix(identity, "-refinery") {
			return "gt-" + identity
		}
		// Pattern: <rig>-crew-<name> → gt-<rig>-crew-<name>
		if strings.Contains(identity, "-crew-") {
			return "gt-" + identity
		}
		// Unknown identity
		return ""
	}
}

// restartSession starts a new session for the given agent.
func (d *Daemon) restartSession(sessionName, identity string) error {
	// Determine working directory and startup command based on agent type
	var workDir, startCmd string
	var rigName string
	var agentRole string
	var needsPreSync bool

	if identity == "mayor" {
		workDir = d.config.TownRoot
		startCmd = "exec claude --dangerously-skip-permissions"
		agentRole = "coordinator"
	} else if strings.HasSuffix(identity, "-witness") {
		// Extract rig name: <rig>-witness → <rig>
		rigName = strings.TrimSuffix(identity, "-witness")
		workDir = d.config.TownRoot + "/" + rigName
		startCmd = "exec claude --dangerously-skip-permissions"
		agentRole = "witness"
	} else if strings.HasSuffix(identity, "-refinery") {
		// Extract rig name: <rig>-refinery → <rig>
		rigName = strings.TrimSuffix(identity, "-refinery")
		workDir = filepath.Join(d.config.TownRoot, rigName, "refinery", "rig")
		startCmd = "exec claude --dangerously-skip-permissions"
		agentRole = "refinery"
		needsPreSync = true
	} else if strings.Contains(identity, "-crew-") {
		// Extract rig and crew name: <rig>-crew-<name> → <rig>, <name>
		parts := strings.SplitN(identity, "-crew-", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid crew identity format: %s", identity)
		}
		rigName = parts[0]
		crewName := parts[1]
		workDir = filepath.Join(d.config.TownRoot, rigName, "crew", crewName)
		startCmd = "exec claude --dangerously-skip-permissions"
		agentRole = "crew"
		needsPreSync = true
	} else {
		return fmt.Errorf("don't know how to restart %s", identity)
	}

	// Pre-sync workspace for agents with git worktrees (refinery)
	if needsPreSync {
		d.logger.Printf("Pre-syncing workspace for %s at %s", identity, workDir)
		d.syncWorkspace(workDir)
	}

	// Create session
	if err := d.tmux.NewSession(sessionName, workDir); err != nil {
		return fmt.Errorf("creating session: %w", err)
	}

	// Set environment
	_ = d.tmux.SetEnvironment(sessionName, "GT_ROLE", identity)
	// BD_ACTOR uses slashes instead of dashes for path-like identity
	bdActor := identityToBDActor(identity)
	_ = d.tmux.SetEnvironment(sessionName, "BD_ACTOR", bdActor)

	// Apply theme
	if identity == "mayor" {
		theme := tmux.MayorTheme()
		_ = d.tmux.ConfigureGasTownSession(sessionName, theme, "", "Mayor", "coordinator")
	} else if rigName != "" {
		theme := tmux.AssignTheme(rigName)
		_ = d.tmux.ConfigureGasTownSession(sessionName, theme, rigName, agentRole, agentRole)
	}

	// Send startup command
	if err := d.tmux.SendKeys(sessionName, startCmd); err != nil {
		return fmt.Errorf("sending startup command: %w", err)
	}

	// Note: gt prime is handled by Claude's SessionStart hook, not injected here.
	// Injecting it via SendKeysDelayed causes rogue text to appear in the terminal.

	return nil
}

// syncWorkspace syncs a git workspace before starting a new session.
// This ensures agents with persistent clones (like refinery) start with current code.
func (d *Daemon) syncWorkspace(workDir string) {
	// Fetch latest from origin
	fetchCmd := exec.Command("git", "fetch", "origin")
	fetchCmd.Dir = workDir
	if err := fetchCmd.Run(); err != nil {
		d.logger.Printf("Warning: git fetch failed in %s: %v", workDir, err)
	}

	// Pull with rebase to incorporate changes
	pullCmd := exec.Command("git", "pull", "--rebase", "origin", "main")
	pullCmd.Dir = workDir
	if err := pullCmd.Run(); err != nil {
		d.logger.Printf("Warning: git pull failed in %s: %v", workDir, err)
		// Don't fail - agent can handle conflicts
	}

	// Sync beads
	bdCmd := exec.Command("bd", "sync")
	bdCmd.Dir = workDir
	if err := bdCmd.Run(); err != nil {
		d.logger.Printf("Warning: bd sync failed in %s: %v", workDir, err)
	}
}

// closeMessage removes a lifecycle mail message after processing.
// We use delete instead of read because gt mail read intentionally
// doesn't mark messages as read (to preserve handoff messages).
func (d *Daemon) closeMessage(id string) error {
	// Use gt mail delete to actually remove the message
	cmd := exec.Command("gt", "mail", "delete", id)
	cmd.Dir = d.config.TownRoot
	return cmd.Run()
}

// verifyAgentRequestingState verifies that the agent has set requesting_<action>=true
// in its state.json before we kill its session. This ensures the agent is actually
// ready to be killed and has completed its pre-shutdown tasks (git clean, handoff mail, etc).
func (d *Daemon) verifyAgentRequestingState(identity string, action LifecycleAction) error {
	stateFile := d.identityToStateFile(identity)
	if stateFile == "" {
		// If we can't determine state file, log warning but allow action
		// This maintains backwards compatibility with agents that don't support state files yet
		d.logger.Printf("Warning: cannot determine state file for %s, skipping verification", identity)
		return nil
	}

	data, err := os.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("agent state file not found: %s (agent must set requesting_%s=true before lifecycle request)", stateFile, action)
		}
		return fmt.Errorf("reading agent state: %w", err)
	}

	var state map[string]interface{}
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parsing agent state: %w", err)
	}

	// Check for requesting_<action>=true
	key := "requesting_" + string(action)
	val, ok := state[key]
	if !ok {
		return fmt.Errorf("agent state missing %s field (agent must set this before lifecycle request)", key)
	}

	requesting, ok := val.(bool)
	if !ok || !requesting {
		return fmt.Errorf("agent state %s is not true (got: %v)", key, val)
	}

	d.logger.Printf("Verified agent %s has %s=true", identity, key)
	return nil
}

// clearAgentRequestingState clears the requesting_<action>=true flag after
// successfully completing a lifecycle action. This prevents the daemon from
// repeatedly cycling the same session.
func (d *Daemon) clearAgentRequestingState(identity string, action LifecycleAction) error {
	stateFile := d.identityToStateFile(identity)
	if stateFile == "" {
		return fmt.Errorf("cannot determine state file for %s", identity)
	}

	data, err := os.ReadFile(stateFile)
	if err != nil {
		return fmt.Errorf("reading state file: %w", err)
	}

	var state map[string]interface{}
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parsing state: %w", err)
	}

	// Remove the requesting_<action> key
	key := "requesting_" + string(action)
	delete(state, key)
	delete(state, "requesting_time") // Also clean up the timestamp

	// Write back
	newData, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	if err := os.WriteFile(stateFile, newData, 0644); err != nil {
		return fmt.Errorf("writing state file: %w", err)
	}

	d.logger.Printf("Cleared %s from agent %s state", key, identity)
	return nil
}

// identityToStateFile maps an agent identity to its state.json file path.
func (d *Daemon) identityToStateFile(identity string) string {
	switch identity {
	case "mayor":
		return filepath.Join(d.config.TownRoot, "mayor", "state.json")
	default:
		// Pattern: <rig>-witness → <townRoot>/<rig>/witness/state.json
		if strings.HasSuffix(identity, "-witness") {
			rigName := strings.TrimSuffix(identity, "-witness")
			return filepath.Join(d.config.TownRoot, rigName, "witness", "state.json")
		}
		// Pattern: <rig>-refinery → <townRoot>/<rig>/refinery/state.json
		if strings.HasSuffix(identity, "-refinery") {
			rigName := strings.TrimSuffix(identity, "-refinery")
			return filepath.Join(d.config.TownRoot, rigName, "refinery", "state.json")
		}
		// Pattern: <rig>-crew-<name> → <townRoot>/<rig>/crew/<name>/state.json
		if strings.Contains(identity, "-crew-") {
			parts := strings.SplitN(identity, "-crew-", 2)
			if len(parts) == 2 {
				rigName := parts[0]
				crewName := parts[1]
				return filepath.Join(d.config.TownRoot, rigName, "crew", crewName, "state.json")
			}
		}
		// Unknown identity - can't determine state file
		return ""
	}
}

// identityToBDActor converts a daemon identity (with dashes) to BD_ACTOR format (with slashes).
// Examples:
//   - "mayor" → "mayor"
//   - "gastown-witness" → "gastown/witness"
//   - "gastown-refinery" → "gastown/refinery"
//   - "gastown-crew-max" → "gastown/crew/max"
func identityToBDActor(identity string) string {
	switch identity {
	case "mayor", "deacon":
		return identity
	default:
		// Pattern: <rig>-witness → <rig>/witness
		if strings.HasSuffix(identity, "-witness") {
			rigName := strings.TrimSuffix(identity, "-witness")
			return rigName + "/witness"
		}
		// Pattern: <rig>-refinery → <rig>/refinery
		if strings.HasSuffix(identity, "-refinery") {
			rigName := strings.TrimSuffix(identity, "-refinery")
			return rigName + "/refinery"
		}
		// Pattern: <rig>-crew-<name> → <rig>/crew/<name>
		if strings.Contains(identity, "-crew-") {
			parts := strings.SplitN(identity, "-crew-", 2)
			if len(parts) == 2 {
				return parts[0] + "/crew/" + parts[1]
			}
		}
		// Unknown format - return as-is
		return identity
	}
}
