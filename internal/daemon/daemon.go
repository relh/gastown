package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

// Daemon is the town-level background service.
// Its only job is to ensure Deacon is running and send periodic heartbeats.
// All health checking, nudging, and decision-making belongs in the Deacon molecule.
type Daemon struct {
	config        *Config
	tmux          *tmux.Tmux
	logger        *log.Logger
	ctx           context.Context
	cancel        context.CancelFunc
	lastMOTDIndex int // tracks last MOTD to avoid consecutive repeats
}

// New creates a new daemon instance.
func New(config *Config) (*Daemon, error) {
	// Ensure daemon directory exists
	daemonDir := filepath.Dir(config.LogFile)
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		return nil, fmt.Errorf("creating daemon directory: %w", err)
	}

	// Open log file
	logFile, err := os.OpenFile(config.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("opening log file: %w", err)
	}

	logger := log.New(logFile, "", log.LstdFlags)
	ctx, cancel := context.WithCancel(context.Background())

	return &Daemon{
		config: config,
		tmux:   tmux.NewTmux(),
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

// Run starts the daemon main loop.
func (d *Daemon) Run() error {
	d.logger.Printf("Daemon starting (PID %d)", os.Getpid())

	// Write PID file
	if err := os.WriteFile(d.config.PidFile, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		return fmt.Errorf("writing PID file: %w", err)
	}
	defer func() { _ = os.Remove(d.config.PidFile) }()

	// Update state
	state := &State{
		Running:   true,
		PID:       os.Getpid(),
		StartedAt: time.Now(),
	}
	if err := SaveState(d.config.TownRoot, state); err != nil {
		d.logger.Printf("Warning: failed to save state: %v", err)
	}

	// Handle signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)

	// Heartbeat ticker
	ticker := time.NewTicker(d.config.HeartbeatInterval)
	defer ticker.Stop()

	d.logger.Printf("Daemon running, heartbeat every %v", d.config.HeartbeatInterval)

	// Initial heartbeat
	d.heartbeat(state)

	for {
		select {
		case <-d.ctx.Done():
			d.logger.Println("Daemon context cancelled, shutting down")
			return d.shutdown(state)

		case sig := <-sigChan:
			if sig == syscall.SIGUSR1 {
				// SIGUSR1: immediate lifecycle processing (from gt handoff)
				d.logger.Println("Received SIGUSR1, processing lifecycle requests immediately")
				d.processLifecycleRequests()
			} else {
				d.logger.Printf("Received signal %v, shutting down", sig)
				return d.shutdown(state)
			}

		case <-ticker.C:
			d.heartbeat(state)
		}
	}
}

// heartbeat performs one heartbeat cycle.
// The daemon's job is minimal: ensure Deacon is running and send heartbeats.
// All health checking and decision-making belongs in the Deacon molecule.
func (d *Daemon) heartbeat(state *State) {
	d.logger.Println("Heartbeat starting")

	// 1. Ensure Deacon is running (process management)
	d.ensureDeaconRunning()

	// 2. Send heartbeat to Deacon (simple notification, no decision-making)
	d.pokeDeacon()

	// 3. Process lifecycle requests
	d.processLifecycleRequests()

	// Update state
	state.LastHeartbeat = time.Now()
	state.HeartbeatCount++
	if err := SaveState(d.config.TownRoot, state); err != nil {
		d.logger.Printf("Warning: failed to save state: %v", err)
	}

	d.logger.Printf("Heartbeat complete (#%d)", state.HeartbeatCount)
}

// DeaconSessionName is the tmux session name for the Deacon.
const DeaconSessionName = "gt-deacon"

// DeaconRole is the role name for the Deacon's handoff bead.
const DeaconRole = "deacon"

// deaconMOTDMessages contains rotating motivational and educational tips
// for the Deacon heartbeat. These make the thankless patrol role more fun.
var deaconMOTDMessages = []string{
	"Thanks for keeping the town running!",
	"You are Gas Town's most critical role.",
	"You are the heart of Gas Town! Be watchful!",
	"Tip: Polecats are transient - spawn freely, kill liberally.",
	"Tip: Witnesses monitor polecats; you monitor witnesses.",
	"Tip: Wisps are transient molecules for patrol cycles.",
	"The town sleeps soundly because you never do.",
	"Tip: Mayor handles cross-rig coordination; you handle health.",
	"Your vigilance keeps the agents honest.",
	"Tip: Use 'gt deacon heartbeat' to signal you're alive.",
	"Every heartbeat you check keeps Gas Town beating.",
	"Tip: Stale agents need nudging; very stale ones need restarting.",
}

// nextMOTD returns the next MOTD message, rotating through the list
// and avoiding consecutive repeats.
func (d *Daemon) nextMOTD() string {
	if len(deaconMOTDMessages) == 0 {
		return "HEARTBEAT: run your rounds"
	}

	// Pick a random index that's different from the last one
	nextIdx := d.lastMOTDIndex
	for nextIdx == d.lastMOTDIndex && len(deaconMOTDMessages) > 1 {
		nextIdx = int(time.Now().UnixNano() % int64(len(deaconMOTDMessages)))
	}
	d.lastMOTDIndex = nextIdx
	return deaconMOTDMessages[nextIdx]
}

// ensureDeaconRunning checks if the Deacon session exists and Claude is running.
// If the session exists but Claude has exited, it restarts Claude.
// If the session doesn't exist, it creates it and starts Claude.
// The Deacon is the system's heartbeat - it must always be running.
func (d *Daemon) ensureDeaconRunning() {
	sessionExists, err := d.tmux.HasSession(DeaconSessionName)
	if err != nil {
		d.logger.Printf("Error checking Deacon session: %v", err)
		return
	}

	if sessionExists {
		// Session exists - check if Claude is actually running
		cmd, err := d.tmux.GetPaneCommand(DeaconSessionName)
		if err != nil {
			d.logger.Printf("Error checking Deacon pane command: %v", err)
			return
		}

		// If Claude is running (node process), we're good
		if cmd == "node" {
			return
		}

		// Claude has exited (shell is showing) - restart it
		d.logger.Printf("Deacon session exists but Claude exited (cmd=%s), restarting...", cmd)
		if err := d.tmux.SendKeys(DeaconSessionName, "export GT_ROLE=deacon BD_ACTOR=deacon && claude --dangerously-skip-permissions"); err != nil {
			d.logger.Printf("Error restarting Claude in Deacon session: %v", err)
		}
		return
	}

	// Session doesn't exist - create it and start Claude
	d.logger.Println("Deacon session not running, starting...")

	// Create session in deacon directory (ensures correct CLAUDE.md is loaded)
	deaconDir := filepath.Join(d.config.TownRoot, "deacon")
	if err := d.tmux.NewSession(DeaconSessionName, deaconDir); err != nil {
		d.logger.Printf("Error creating Deacon session: %v", err)
		return
	}

	// Set environment
	_ = d.tmux.SetEnvironment(DeaconSessionName, "GT_ROLE", "deacon")
	_ = d.tmux.SetEnvironment(DeaconSessionName, "BD_ACTOR", "deacon")

	// Launch Claude directly (no shell respawn loop)
	// The daemon will detect if Claude exits and restart it on next heartbeat
	// Export GT_ROLE and BD_ACTOR so Claude inherits them (tmux SetEnvironment doesn't export to processes)
	if err := d.tmux.SendKeys(DeaconSessionName, "export GT_ROLE=deacon BD_ACTOR=deacon && claude --dangerously-skip-permissions"); err != nil {
		d.logger.Printf("Error launching Claude in Deacon session: %v", err)
		return
	}

	d.logger.Println("Deacon session started successfully")
}

// pokeDeacon sends a heartbeat message to the Deacon session.
// Simple notification - no staleness checking or backoff logic.
// The Deacon molecule decides what to do with heartbeats.
func (d *Daemon) pokeDeacon() {
	running, err := d.tmux.HasSession(DeaconSessionName)
	if err != nil {
		d.logger.Printf("Error checking Deacon session: %v", err)
		return
	}

	if !running {
		d.logger.Println("Deacon session not running after ensure, skipping poke")
		return
	}

	// Send heartbeat message with rotating MOTD
	motd := d.nextMOTD()
	msg := fmt.Sprintf("HEARTBEAT: %s", motd)
	if err := d.tmux.SendKeysReplace(DeaconSessionName, msg, 50); err != nil {
		d.logger.Printf("Error poking Deacon: %v", err)
		return
	}

	d.logger.Println("Poked Deacon")
}

// NOTE: pokeMayor, pokeWitnesses, and pokeWitness have been removed.
// The Deacon molecule is responsible for monitoring Mayor and Witnesses.
// The daemon only ensures Deacon is running and sends it heartbeats.

// processLifecycleRequests checks for and processes lifecycle requests.
func (d *Daemon) processLifecycleRequests() {
	d.ProcessLifecycleRequests()
}

// shutdown performs graceful shutdown.
func (d *Daemon) shutdown(state *State) error {
	d.logger.Println("Daemon shutting down")

	state.Running = false
	if err := SaveState(d.config.TownRoot, state); err != nil {
		d.logger.Printf("Warning: failed to save final state: %v", err)
	}

	d.logger.Println("Daemon stopped")
	return nil
}

// Stop signals the daemon to stop.
func (d *Daemon) Stop() {
	d.cancel()
}

// IsRunning checks if a daemon is running for the given town.
func IsRunning(townRoot string) (bool, int, error) {
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, nil
		}
		return false, 0, err
	}

	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return false, 0, nil
	}

	// Check if process is running
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, 0, nil
	}

	// On Unix, FindProcess always succeeds. Send signal 0 to check if alive.
	err = process.Signal(syscall.Signal(0))
	if err != nil {
		// Process not running, clean up stale PID file
		_ = os.Remove(pidFile)
		return false, 0, nil
	}

	return true, pid, nil
}

// StopDaemon stops the running daemon for the given town.
func StopDaemon(townRoot string) error {
	running, pid, err := IsRunning(townRoot)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("daemon is not running")
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding process: %w", err)
	}

	// Send SIGTERM for graceful shutdown
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("sending SIGTERM: %w", err)
	}

	// Wait a bit for graceful shutdown
	time.Sleep(500 * time.Millisecond)

	// Check if still running
	if err := process.Signal(syscall.Signal(0)); err == nil {
		// Still running, force kill
		_ = process.Signal(syscall.SIGKILL)
	}

	// Clean up PID file
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
	_ = os.Remove(pidFile)

	return nil
}
