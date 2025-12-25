package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var upCmd = &cobra.Command{
	Use:     "up",
	GroupID: GroupServices,
	Short:   "Bring up all Gas Town services",
	Long: `Start all Gas Town long-lived services.

This is the idempotent "boot" command for Gas Town. It ensures all
infrastructure agents are running:

  • Daemon     - Go background process that pokes agents
  • Deacon     - Health orchestrator (monitors Mayor/Witnesses)
  • Mayor      - Global work coordinator
  • Witnesses  - Per-rig polecat managers
  • Refineries - Per-rig merge queue processors

Polecats are NOT started by this command - they are transient workers
spawned on demand by the Mayor or Witnesses.

Running 'gt up' multiple times is safe - it only starts services that
aren't already running.`,
	RunE: runUp,
}

var (
	upQuiet bool
)

func init() {
	upCmd.Flags().BoolVarP(&upQuiet, "quiet", "q", false, "Only show errors")
	rootCmd.AddCommand(upCmd)
}

func runUp(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	t := tmux.NewTmux()
	allOK := true

	// 1. Daemon (Go process)
	if err := ensureDaemon(townRoot); err != nil {
		printStatus("Daemon", false, err.Error())
		allOK = false
	} else {
		running, pid, _ := daemon.IsRunning(townRoot)
		if running {
			printStatus("Daemon", true, fmt.Sprintf("PID %d", pid))
		}
	}

	// 2. Deacon (Claude agent)
	if err := ensureSession(t, DeaconSessionName, townRoot, "deacon"); err != nil {
		printStatus("Deacon", false, err.Error())
		allOK = false
	} else {
		printStatus("Deacon", true, "gt-deacon")
	}

	// 3. Mayor (Claude agent)
	if err := ensureSession(t, MayorSessionName, townRoot, "mayor"); err != nil {
		printStatus("Mayor", false, err.Error())
		allOK = false
	} else {
		printStatus("Mayor", true, "gt-mayor")
	}

	// 4. Witnesses (one per rig)
	rigs := discoverRigs(townRoot)
	for _, rigName := range rigs {
		sessionName := fmt.Sprintf("gt-%s-witness", rigName)
		rigPath := filepath.Join(townRoot, rigName)

		if err := ensureWitness(t, sessionName, rigPath, rigName); err != nil {
			printStatus(fmt.Sprintf("Witness (%s)", rigName), false, err.Error())
			allOK = false
		} else {
			printStatus(fmt.Sprintf("Witness (%s)", rigName), true, sessionName)
		}
	}

	// 5. Refineries (one per rig)
	for _, rigName := range rigs {
		_, r, err := getRig(rigName)
		if err != nil {
			printStatus(fmt.Sprintf("Refinery (%s)", rigName), false, err.Error())
			allOK = false
			continue
		}

		mgr := refinery.NewManager(r)
		if err := mgr.Start(false); err != nil {
			if err == refinery.ErrAlreadyRunning {
				sessionName := fmt.Sprintf("gt-%s-refinery", rigName)
				printStatus(fmt.Sprintf("Refinery (%s)", rigName), true, sessionName)
			} else {
				printStatus(fmt.Sprintf("Refinery (%s)", rigName), false, err.Error())
				allOK = false
			}
		} else {
			sessionName := fmt.Sprintf("gt-%s-refinery", rigName)
			printStatus(fmt.Sprintf("Refinery (%s)", rigName), true, sessionName)
		}
	}

	fmt.Println()
	if allOK {
		fmt.Printf("%s All services running\n", style.Bold.Render("✓"))
	} else {
		fmt.Printf("%s Some services failed to start\n", style.Bold.Render("✗"))
		return fmt.Errorf("not all services started")
	}

	return nil
}

func printStatus(name string, ok bool, detail string) {
	if upQuiet && ok {
		return
	}
	if ok {
		fmt.Printf("%s %s: %s\n", style.SuccessPrefix, name, style.Dim.Render(detail))
	} else {
		fmt.Printf("%s %s: %s\n", style.ErrorPrefix, name, detail)
	}
}

// ensureDaemon starts the daemon if not running.
func ensureDaemon(townRoot string) error {
	running, _, err := daemon.IsRunning(townRoot)
	if err != nil {
		return err
	}
	if running {
		return nil
	}

	// Start daemon
	gtPath, err := os.Executable()
	if err != nil {
		return err
	}

	cmd := exec.Command(gtPath, "daemon", "run")
	cmd.Dir = townRoot
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return err
	}

	// Wait for daemon to initialize
	time.Sleep(300 * time.Millisecond)

	// Verify it started
	running, _, err = daemon.IsRunning(townRoot)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("daemon failed to start")
	}

	return nil
}

// ensureSession starts a Claude session if not running.
func ensureSession(t *tmux.Tmux, sessionName, workDir, role string) error {
	running, err := t.HasSession(sessionName)
	if err != nil {
		return err
	}
	if running {
		return nil
	}

	// Create session
	if err := t.NewSession(sessionName, workDir); err != nil {
		return err
	}

	// Set environment
	_ = t.SetEnvironment(sessionName, "GT_ROLE", role)
	_ = t.SetEnvironment(sessionName, "BD_ACTOR", role)

	// Apply theme based on role
	switch role {
	case "mayor":
		theme := tmux.MayorTheme()
		_ = t.ConfigureGasTownSession(sessionName, theme, "", "Mayor", "coordinator")
	case "deacon":
		theme := tmux.DeaconTheme()
		_ = t.ConfigureGasTownSession(sessionName, theme, "", "Deacon", "health-check")
	}

	// Launch Claude
	// Export GT_ROLE and BD_ACTOR in the command since tmux SetEnvironment only affects new panes
	var claudeCmd string
	if role == "deacon" {
		// Deacon uses respawn loop
		claudeCmd = `export GT_ROLE=deacon BD_ACTOR=deacon && while true; do echo "⛪ Starting Deacon session..."; claude --dangerously-skip-permissions; echo ""; echo "Deacon exited. Restarting in 2s... (Ctrl-C to stop)"; sleep 2; done`
	} else {
		claudeCmd = fmt.Sprintf(`export GT_ROLE=%s BD_ACTOR=%s && claude --dangerously-skip-permissions`, role, role)
	}

	if err := t.SendKeysDelayed(sessionName, claudeCmd, 200); err != nil {
		return err
	}

	return nil
}

// ensureWitness starts a witness session for a rig.
func ensureWitness(t *tmux.Tmux, sessionName, rigPath, rigName string) error {
	running, err := t.HasSession(sessionName)
	if err != nil {
		return err
	}
	if running {
		return nil
	}

	// Create session in rig directory
	if err := t.NewSession(sessionName, rigPath); err != nil {
		return err
	}

	// Set environment
	bdActor := fmt.Sprintf("%s/witness", rigName)
	_ = t.SetEnvironment(sessionName, "GT_ROLE", "witness")
	_ = t.SetEnvironment(sessionName, "GT_RIG", rigName)
	_ = t.SetEnvironment(sessionName, "BD_ACTOR", bdActor)

	// Apply theme (use rig-based theme)
	theme := tmux.AssignTheme(rigName)
	_ = t.ConfigureGasTownSession(sessionName, theme, "", "Witness", rigName)

	// Launch Claude
	// Export GT_ROLE and BD_ACTOR in the command since tmux SetEnvironment only affects new panes
	claudeCmd := fmt.Sprintf(`export GT_ROLE=witness BD_ACTOR=%s && claude --dangerously-skip-permissions`, bdActor)
	if err := t.SendKeysDelayed(sessionName, claudeCmd, 200); err != nil {
		return err
	}

	return nil
}

// discoverRigs finds all rigs in the town.
func discoverRigs(townRoot string) []string {
	var rigs []string

	// Try rigs.json first
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	if rigsConfig, err := config.LoadRigsConfig(rigsConfigPath); err == nil {
		for name := range rigsConfig.Rigs {
			rigs = append(rigs, name)
		}
		return rigs
	}

	// Fallback: scan directory for rig-like directories
	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return rigs
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		// Skip known non-rig directories
		if name == "mayor" || name == "daemon" || name == "deacon" ||
			name == ".git" || name == "docs" || name[0] == '.' {
			continue
		}

		dirPath := filepath.Join(townRoot, name)

		// Check for .beads directory (indicates a rig)
		beadsPath := filepath.Join(dirPath, ".beads")
		if _, err := os.Stat(beadsPath); err == nil {
			rigs = append(rigs, name)
			continue
		}

		// Check for polecats directory (indicates a rig)
		polecatsPath := filepath.Join(dirPath, "polecats")
		if _, err := os.Stat(polecatsPath); err == nil {
			rigs = append(rigs, name)
		}
	}

	return rigs
}
