package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
)

func runCrewRemove(cmd *cobra.Command, args []string) error {
	name := args[0]
	// Parse rig/name format (e.g., "beads/emma" -> rig=beads, name=emma)
	if rig, crewName, ok := parseRigSlashName(name); ok {
		if crewRig == "" {
			crewRig = rig
		}
		name = crewName
	}

	crewMgr, r, err := getCrewManager(crewRig)
	if err != nil {
		return err
	}

	// Check for running session (unless forced)
	if !crewForce {
		t := tmux.NewTmux()
		sessionID := crewSessionName(r.Name, name)
		hasSession, _ := t.HasSession(sessionID)
		if hasSession {
			return fmt.Errorf("session '%s' is running (use --force to kill and remove)", sessionID)
		}
	}

	// Kill session if it exists
	t := tmux.NewTmux()
	sessionID := crewSessionName(r.Name, name)
	if hasSession, _ := t.HasSession(sessionID); hasSession {
		if err := t.KillSession(sessionID); err != nil {
			return fmt.Errorf("killing session: %w", err)
		}
		fmt.Printf("Killed session %s\n", sessionID)
	}

	// Remove the crew workspace
	if err := crewMgr.Remove(name, crewForce); err != nil {
		if err == crew.ErrCrewNotFound {
			return fmt.Errorf("crew workspace '%s' not found", name)
		}
		if err == crew.ErrHasChanges {
			return fmt.Errorf("crew workspace has uncommitted changes (use --force to remove anyway)")
		}
		return fmt.Errorf("removing crew workspace: %w", err)
	}

	fmt.Printf("%s Removed crew workspace: %s/%s\n",
		style.Bold.Render("✓"), r.Name, name)
	return nil
}

func runCrewRefresh(cmd *cobra.Command, args []string) error {
	name := args[0]
	// Parse rig/name format (e.g., "beads/emma" -> rig=beads, name=emma)
	if rig, crewName, ok := parseRigSlashName(name); ok {
		if crewRig == "" {
			crewRig = rig
		}
		name = crewName
	}

	crewMgr, r, err := getCrewManager(crewRig)
	if err != nil {
		return err
	}

	// Get the crew worker
	worker, err := crewMgr.Get(name)
	if err != nil {
		if err == crew.ErrCrewNotFound {
			return fmt.Errorf("crew workspace '%s' not found", name)
		}
		return fmt.Errorf("getting crew worker: %w", err)
	}

	t := tmux.NewTmux()
	sessionID := crewSessionName(r.Name, name)

	// Check if session exists
	hasSession, _ := t.HasSession(sessionID)

	// Create handoff message
	handoffMsg := crewMessage
	if handoffMsg == "" {
		handoffMsg = fmt.Sprintf("Context refresh for %s. Check mail and beads for current work state.", name)
	}

	// Send handoff mail to self
	mailDir := filepath.Join(worker.ClonePath, "mail")
	if _, err := os.Stat(mailDir); os.IsNotExist(err) {
		if err := os.MkdirAll(mailDir, 0755); err != nil {
			return fmt.Errorf("creating mail dir: %w", err)
		}
	}

	// Create and send mail
	mailbox := mail.NewMailbox(mailDir)
	msg := &mail.Message{
		From:    fmt.Sprintf("%s/%s", r.Name, name),
		To:      fmt.Sprintf("%s/%s", r.Name, name),
		Subject: "🤝 HANDOFF: Context Refresh",
		Body:    handoffMsg,
	}
	if err := mailbox.Append(msg); err != nil {
		return fmt.Errorf("sending handoff mail: %w", err)
	}
	fmt.Printf("Sent handoff mail to %s/%s\n", r.Name, name)

	// Kill existing session if running
	if hasSession {
		if err := t.KillSession(sessionID); err != nil {
			return fmt.Errorf("killing old session: %w", err)
		}
		fmt.Printf("Killed old session %s\n", sessionID)
	}

	// Start new session
	if err := t.NewSession(sessionID, worker.ClonePath); err != nil {
		return fmt.Errorf("creating session: %w", err)
	}

	// Set environment (non-fatal: session works without these)
	_ = t.SetEnvironment(sessionID, "GT_RIG", r.Name)
	_ = t.SetEnvironment(sessionID, "GT_CREW", name)

	// Wait for shell to be ready
	if err := t.WaitForShellReady(sessionID, constants.ShellReadyTimeout); err != nil {
		return fmt.Errorf("waiting for shell: %w", err)
	}

	// Start claude (refresh uses regular permissions, reads handoff mail)
	if err := t.SendKeys(sessionID, "claude"); err != nil {
		return fmt.Errorf("starting claude: %w", err)
	}

	fmt.Printf("%s Refreshed crew workspace: %s/%s\n",
		style.Bold.Render("✓"), r.Name, name)
	fmt.Printf("Attach with: %s\n", style.Dim.Render(fmt.Sprintf("gt crew at %s", name)))

	return nil
}

// runCrewStart is an alias for runStartCrew, handling multiple input formats.
// It supports: "name", "rig/name", and "rig/crew/name" formats.
func runCrewStart(cmd *cobra.Command, args []string) error {
	name := args[0]

	// Handle rig/crew/name format (e.g., "gastown/crew/joe" -> "gastown/joe")
	if strings.Contains(name, "/crew/") {
		parts := strings.SplitN(name, "/crew/", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			name = parts[0] + "/" + parts[1]
		}
	}

	// Set the start.go flags from crew.go flags before calling
	startCrewRig = crewRig
	startCrewAccount = crewAccount

	return runStartCrew(cmd, []string{name})
}

func runCrewRestart(cmd *cobra.Command, args []string) error {
	// Handle --all flag
	if crewAll {
		return runCrewRestartAll()
	}

	name := args[0]
	// Parse rig/name format (e.g., "beads/emma" -> rig=beads, name=emma)
	if rig, crewName, ok := parseRigSlashName(name); ok {
		if crewRig == "" {
			crewRig = rig
		}
		name = crewName
	}

	crewMgr, r, err := getCrewManager(crewRig)
	if err != nil {
		return err
	}

	// Get the crew worker, create if not exists (idempotent)
	worker, err := crewMgr.Get(name)
	if err == crew.ErrCrewNotFound {
		fmt.Printf("Creating crew workspace %s in %s...\n", name, r.Name)
		worker, err = crewMgr.Add(name, false) // No feature branch for crew
		if err != nil {
			return fmt.Errorf("creating crew workspace: %w", err)
		}
		fmt.Printf("Created crew workspace: %s/%s\n", r.Name, name)
	} else if err != nil {
		return fmt.Errorf("getting crew worker: %w", err)
	}

	t := tmux.NewTmux()
	sessionID := crewSessionName(r.Name, name)

	// Kill existing session if running
	if hasSession, _ := t.HasSession(sessionID); hasSession {
		if err := t.KillSession(sessionID); err != nil {
			return fmt.Errorf("killing old session: %w", err)
		}
		fmt.Printf("Killed session %s\n", sessionID)
	}

	// Start new session
	if err := t.NewSession(sessionID, worker.ClonePath); err != nil {
		return fmt.Errorf("creating session: %w", err)
	}

	// Set environment
	t.SetEnvironment(sessionID, "GT_ROLE", "crew")
	t.SetEnvironment(sessionID, "GT_RIG", r.Name)
	t.SetEnvironment(sessionID, "GT_CREW", name)

	// Apply rig-based theming (non-fatal: theming failure doesn't affect operation)
	theme := getThemeForRig(r.Name)
	_ = t.ConfigureGasTownSession(sessionID, theme, r.Name, name, "crew")

	// Wait for shell to be ready
	if err := t.WaitForShellReady(sessionID, constants.ShellReadyTimeout); err != nil {
		return fmt.Errorf("waiting for shell: %w", err)
	}

	// Start claude with skip permissions (crew workers are trusted)
	// Export GT_ROLE and BD_ACTOR since tmux SetEnvironment only affects new panes
	bdActor := fmt.Sprintf("%s/crew/%s", r.Name, name)
	claudeCmd := fmt.Sprintf("export GT_ROLE=crew GT_RIG=%s GT_CREW=%s BD_ACTOR=%s && claude --dangerously-skip-permissions", r.Name, name, bdActor)
	if err := t.SendKeys(sessionID, claudeCmd); err != nil {
		return fmt.Errorf("starting claude: %w", err)
	}

	// Wait for Claude to start, then prime it
	shells := constants.SupportedShells
	if err := t.WaitForCommand(sessionID, shells, constants.ClaudeStartTimeout); err != nil {
		style.PrintWarning("Timeout waiting for Claude to start: %v", err)
	}
	// Give Claude time to initialize after process starts
	time.Sleep(constants.ShutdownNotifyDelay)
	if err := t.SendKeys(sessionID, "gt prime"); err != nil {
		// Non-fatal: Claude started but priming failed
		style.PrintWarning("Could not send prime command: %v", err)
	}

	// Send crew resume prompt after prime completes
	// Use NudgeSession (the canonical way to message Claude) with longer pre-delay
	// to ensure gt prime has finished processing
	time.Sleep(5 * time.Second)
	crewPrompt := "Read your mail, act on anything urgent, else await instructions."
	if err := t.NudgeSession(sessionID, crewPrompt); err != nil {
		style.PrintWarning("Could not send resume prompt: %v", err)
	}

	fmt.Printf("%s Restarted crew workspace: %s/%s\n",
		style.Bold.Render("✓"), r.Name, name)
	fmt.Printf("Attach with: %s\n", style.Dim.Render(fmt.Sprintf("gt crew at %s", name)))

	return nil
}

// runCrewRestartAll restarts all running crew sessions.
// If crewRig is set, only restarts crew in that rig.
func runCrewRestartAll() error {
	// Get all agent sessions (including polecats to find crew)
	agents, err := getAgentSessions(true)
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}

	// Filter to crew agents only
	var targets []*AgentSession
	for _, agent := range agents {
		if agent.Type != AgentCrew {
			continue
		}
		// Filter by rig if specified
		if crewRig != "" && agent.Rig != crewRig {
			continue
		}
		targets = append(targets, agent)
	}

	if len(targets) == 0 {
		fmt.Println("No running crew sessions to restart.")
		if crewRig != "" {
			fmt.Printf("  (filtered by rig: %s)\n", crewRig)
		}
		return nil
	}

	// Dry run - just show what would be restarted
	if crewDryRun {
		fmt.Printf("Would restart %d crew session(s):\n\n", len(targets))
		for _, agent := range targets {
			fmt.Printf("  %s %s/crew/%s\n", AgentTypeIcons[AgentCrew], agent.Rig, agent.AgentName)
		}
		return nil
	}

	fmt.Printf("Restarting %d crew session(s)...\n\n", len(targets))

	var succeeded, failed int
	var failures []string

	for _, agent := range targets {
		agentName := fmt.Sprintf("%s/crew/%s", agent.Rig, agent.AgentName)

		// Use crewRig temporarily to get the right crew manager
		savedRig := crewRig
		crewRig = agent.Rig

		crewMgr, r, err := getCrewManager(crewRig)
		if err != nil {
			failed++
			failures = append(failures, fmt.Sprintf("%s: %v", agentName, err))
			fmt.Printf("  %s %s\n", style.ErrorPrefix, agentName)
			crewRig = savedRig
			continue
		}

		worker, err := crewMgr.Get(agent.AgentName)
		if err != nil {
			failed++
			failures = append(failures, fmt.Sprintf("%s: %v", agentName, err))
			fmt.Printf("  %s %s\n", style.ErrorPrefix, agentName)
			crewRig = savedRig
			continue
		}

		// Restart the session
		if err := restartCrewSession(r.Name, agent.AgentName, worker.ClonePath); err != nil {
			failed++
			failures = append(failures, fmt.Sprintf("%s: %v", agentName, err))
			fmt.Printf("  %s %s\n", style.ErrorPrefix, agentName)
		} else {
			succeeded++
			fmt.Printf("  %s %s\n", style.SuccessPrefix, agentName)
		}

		crewRig = savedRig

		// Small delay between restarts to avoid overwhelming the system
		time.Sleep(constants.ShutdownNotifyDelay)
	}

	fmt.Println()
	if failed > 0 {
		fmt.Printf("%s Restart complete: %d succeeded, %d failed\n",
			style.WarningPrefix, succeeded, failed)
		for _, f := range failures {
			fmt.Printf("  %s\n", style.Dim.Render(f))
		}
		return fmt.Errorf("%d restart(s) failed", failed)
	}

	fmt.Printf("%s Restart complete: %d crew session(s) restarted\n", style.SuccessPrefix, succeeded)
	return nil
}

// restartCrewSession handles the core restart logic for a single crew session.
func restartCrewSession(rigName, crewName, clonePath string) error {
	t := tmux.NewTmux()
	sessionID := crewSessionName(rigName, crewName)

	// Kill existing session if running
	if hasSession, _ := t.HasSession(sessionID); hasSession {
		if err := t.KillSession(sessionID); err != nil {
			return fmt.Errorf("killing old session: %w", err)
		}
	}

	// Start new session
	if err := t.NewSession(sessionID, clonePath); err != nil {
		return fmt.Errorf("creating session: %w", err)
	}

	// Set environment
	t.SetEnvironment(sessionID, "GT_ROLE", "crew")
	t.SetEnvironment(sessionID, "GT_RIG", rigName)
	t.SetEnvironment(sessionID, "GT_CREW", crewName)

	// Apply rig-based theming
	theme := getThemeForRig(rigName)
	_ = t.ConfigureGasTownSession(sessionID, theme, rigName, crewName, "crew")

	// Wait for shell to be ready
	if err := t.WaitForShellReady(sessionID, constants.ShellReadyTimeout); err != nil {
		return fmt.Errorf("waiting for shell: %w", err)
	}

	// Start claude with skip permissions
	bdActor := fmt.Sprintf("%s/crew/%s", rigName, crewName)
	claudeCmd := fmt.Sprintf("export GT_ROLE=crew GT_RIG=%s GT_CREW=%s BD_ACTOR=%s && claude --dangerously-skip-permissions", rigName, crewName, bdActor)
	if err := t.SendKeys(sessionID, claudeCmd); err != nil {
		return fmt.Errorf("starting claude: %w", err)
	}

	// Wait for Claude to start, then prime it
	shells := constants.SupportedShells
	if err := t.WaitForCommand(sessionID, shells, constants.ClaudeStartTimeout); err != nil {
		// Non-fatal warning
	}
	time.Sleep(constants.ShutdownNotifyDelay)
	if err := t.SendKeys(sessionID, "gt prime"); err != nil {
		// Non-fatal
	}

	// Send crew resume prompt after prime completes
	time.Sleep(5 * time.Second)
	crewPrompt := "Read your mail, act on anything urgent, else await instructions."
	_ = t.NudgeSession(sessionID, crewPrompt)

	return nil
}
