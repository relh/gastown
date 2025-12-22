package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/keepalive"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Daemon is the town-level background service.
type Daemon struct {
	config        *Config
	tmux          *tmux.Tmux
	logger        *log.Logger
	ctx           context.Context
	cancel        context.CancelFunc
	backoff       *BackoffManager
	notifications *NotificationManager
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

	// Initialize notification manager for slot-based deduplication
	notifDir := filepath.Join(daemonDir, "notifications")
	notifMaxAge := 5 * time.Minute // Notifications expire after 5 minutes

	return &Daemon{
		config:       config,
		tmux:         tmux.NewTmux(),
		logger:       logger,
		ctx:          ctx,
		cancel:       cancel,
		backoff:      NewBackoffManager(DefaultBackoffConfig()),
		notifications: NewNotificationManager(notifDir, notifMaxAge),
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
func (d *Daemon) heartbeat(state *State) {
	d.logger.Println("Heartbeat starting")

	// 0. Clean up stale notification slots periodically
	_ = d.notifications.ClearStaleSlots()

	// 1. Ensure Deacon is running (the Deacon is the heartbeat of the system)
	d.ensureDeaconRunning()

	// 2. Check Deacon attachment - spawn patrol if naked
	d.checkDeaconAttachment()

	// 3. Poke Deacon - the Deacon monitors Mayor and Witnesses
	d.pokeDeacon()

	// 4. Process lifecycle requests
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

// DeaconPatrolMolecule is the well-known ID for the deacon patrol molecule.
const DeaconPatrolMolecule = "mol-deacon-patrol"

// deaconMOTDMessages contains rotating motivational and educational tips
// for the Deacon heartbeat. These make the thankless patrol role more fun.
var deaconMOTDMessages = []string{
	"Thanks for keeping the town running!",
	"You are Gas Town's most critical role.",
	"You are the heart of Gas Town! Be watchful!",
	"Tip: Polecats are ephemeral - spawn freely, kill liberally.",
	"Tip: Witnesses monitor polecats; you monitor witnesses.",
	"Tip: Wisps are ephemeral molecules for patrol cycles.",
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

// ensureDeaconRunning checks if the Deacon session exists and starts it if not.
// The Deacon is the system's heartbeat - it must always be running.
func (d *Daemon) ensureDeaconRunning() {
	running, err := d.tmux.HasSession(DeaconSessionName)
	if err != nil {
		d.logger.Printf("Error checking Deacon session: %v", err)
		return
	}

	if running {
		return // Deacon is running, nothing to do
	}

	// Deacon is not running - start it
	d.logger.Println("Deacon session not running, starting...")

	// Create session in town root
	if err := d.tmux.NewSession(DeaconSessionName, d.config.TownRoot); err != nil {
		d.logger.Printf("Error creating Deacon session: %v", err)
		return
	}

	// Set environment
	_ = d.tmux.SetEnvironment(DeaconSessionName, "GT_ROLE", "deacon")

	// Launch Claude in a respawn loop - session survives restarts
	loopCmd := `while true; do echo "⛪ Starting Deacon session..."; claude --dangerously-skip-permissions; echo ""; echo "Deacon exited. Restarting in 2s... (Ctrl-C to stop)"; sleep 2; done`
	if err := d.tmux.SendKeysDelayed(DeaconSessionName, loopCmd, 200); err != nil {
		d.logger.Printf("Error launching Claude in Deacon session: %v", err)
		return
	}

	d.logger.Println("Deacon session started successfully")
}

// checkDeaconAttachment checks if the Deacon's pinned bead has an attached molecule.
// If the Deacon is "naked" (no attachment), this spawns mol-deacon-patrol and
// attaches it to drive the Deacon's patrol loop.
func (d *Daemon) checkDeaconAttachment() {
	// Get beads client for the town root
	bd := beads.New(d.config.TownRoot)

	// Find or create the Deacon's handoff bead
	pinnedBead, err := bd.GetOrCreateHandoffBead(DeaconRole)
	if err != nil {
		d.logger.Printf("Error getting Deacon handoff bead: %v", err)
		return
	}

	// Check if the Deacon is naked (no attached molecule)
	attachment := beads.ParseAttachmentFields(pinnedBead)
	if attachment != nil && attachment.AttachedMolecule != "" {
		d.logger.Printf("Deacon has attachment: %s", attachment.AttachedMolecule)
		return
	}

	// Deacon is naked - need to attach mol-deacon-patrol
	d.logger.Println("Deacon is naked (no attached molecule), spawning patrol...")

	// Find the mol-deacon-patrol template
	molTemplate, err := d.findDeaconPatrolMolecule(bd)
	if err != nil {
		d.logger.Printf("Error finding deacon patrol molecule: %v", err)
		return
	}

	// Instantiate the patrol molecule with the Deacon's pinned bead as parent
	steps, err := bd.InstantiateMolecule(molTemplate, pinnedBead, beads.InstantiateOptions{})
	if err != nil {
		d.logger.Printf("Error instantiating deacon patrol: %v", err)
		return
	}
	d.logger.Printf("Instantiated mol-deacon-patrol with %d steps", len(steps))

	// Attach the molecule to the Deacon's pinned bead
	if _, err := bd.AttachMolecule(pinnedBead.ID, molTemplate.ID); err != nil {
		d.logger.Printf("Error attaching molecule: %v", err)
		return
	}
	d.logger.Printf("Attached %s to Deacon's pinned bead", molTemplate.ID)

	// Nudge the Deacon to start the patrol
	d.nudgeDeaconForPatrol()
}

// findDeaconPatrolMolecule finds the mol-deacon-patrol template issue.
// It first looks for an existing molecule, then falls back to creating from builtin.
func (d *Daemon) findDeaconPatrolMolecule(bd *beads.Beads) (*beads.Issue, error) {
	// Look for existing molecule by title
	molecules, err := bd.List(beads.ListOptions{Type: "molecule", Priority: -1})
	if err != nil {
		return nil, fmt.Errorf("listing molecules: %w", err)
	}

	// Find "Deacon Patrol" molecule
	for _, mol := range molecules {
		if mol.Title == "Deacon Patrol" {
			return mol, nil
		}
	}

	// Not found - seed builtin molecules and try again
	d.logger.Println("Deacon Patrol molecule not found, seeding builtins...")
	created, err := bd.SeedBuiltinMolecules()
	if err != nil {
		return nil, fmt.Errorf("seeding builtin molecules: %w", err)
	}
	d.logger.Printf("Seeded %d builtin molecules", created)

	// Try again
	molecules, err = bd.List(beads.ListOptions{Type: "molecule", Priority: -1})
	if err != nil {
		return nil, fmt.Errorf("listing molecules after seed: %w", err)
	}

	for _, mol := range molecules {
		if mol.Title == "Deacon Patrol" {
			return mol, nil
		}
	}

	return nil, fmt.Errorf("Deacon Patrol molecule not found after seeding")
}

// nudgeDeaconForPatrol sends a message to the Deacon to start its patrol loop.
func (d *Daemon) nudgeDeaconForPatrol() {
	running, err := d.tmux.HasSession(DeaconSessionName)
	if err != nil {
		d.logger.Printf("Error checking Deacon session for nudge: %v", err)
		return
	}

	if !running {
		d.logger.Println("Deacon session not running, cannot nudge")
		return
	}

	// Send patrol start message
	msg := "PATROL: mol-deacon-patrol attached. Start your patrol loop."
	if err := d.tmux.SendKeysReplace(DeaconSessionName, msg, 50); err != nil {
		d.logger.Printf("Error nudging Deacon for patrol: %v", err)
		return
	}

	d.logger.Println("Nudged Deacon to start patrol")
}

// pokeDeacon sends a heartbeat message to the Deacon session.
// The Deacon is responsible for monitoring Mayor and Witnesses.
func (d *Daemon) pokeDeacon() {
	const agentID = "deacon"

	running, err := d.tmux.HasSession(DeaconSessionName)
	if err != nil {
		d.logger.Printf("Error checking Deacon session: %v", err)
		return
	}

	if !running {
		d.logger.Println("Deacon session not running after ensure, skipping poke")
		return
	}

	// Check deacon heartbeat to see if it's active
	deaconHeartbeatFile := filepath.Join(d.config.TownRoot, "deacon", "heartbeat.json")
	var isFresh, isStale, isVeryStale bool

	data, err := os.ReadFile(deaconHeartbeatFile)
	if err == nil {
		var hb struct {
			Timestamp time.Time `json:"timestamp"`
		}
		if json.Unmarshal(data, &hb) == nil {
			age := time.Since(hb.Timestamp)
			isFresh = age < 2*time.Minute
			isStale = age >= 2*time.Minute && age < 5*time.Minute
			isVeryStale = age >= 5*time.Minute
		} else {
			isVeryStale = true
		}
	} else {
		isVeryStale = true // No heartbeat file
	}

	if isFresh {
		// Deacon is actively working, reset backoff and mark notifications consumed
		d.backoff.RecordActivity(agentID)
		_ = d.notifications.MarkConsumed(DeaconSessionName, SlotHeartbeat)
		d.logger.Println("Deacon is fresh, skipping poke")
		return
	}

	// Check if we should poke based on backoff interval
	if !d.backoff.ShouldPoke(agentID) {
		interval := d.backoff.GetInterval(agentID)
		d.logger.Printf("Deacon backoff in effect (interval: %v), skipping poke", interval)
		return
	}

	// Check if we should send (slot-based deduplication)
	shouldSend, _ := d.notifications.ShouldSend(DeaconSessionName, SlotHeartbeat)
	if !shouldSend {
		d.logger.Println("Heartbeat already pending for Deacon, skipping")
		return
	}

	// Send heartbeat message with rotating MOTD
	motd := d.nextMOTD()
	msg := fmt.Sprintf("HEARTBEAT: %s", motd)
	if err := d.tmux.SendKeysReplace(DeaconSessionName, msg, 50); err != nil {
		d.logger.Printf("Error poking Deacon: %v", err)
		return
	}

	// Record the send for slot deduplication
	_ = d.notifications.RecordSend(DeaconSessionName, SlotHeartbeat, msg)
	d.backoff.RecordPoke(agentID)

	// Adjust backoff based on staleness
	if isVeryStale {
		d.backoff.RecordMiss(agentID)
		interval := d.backoff.GetInterval(agentID)
		d.logger.Printf("Poked Deacon (very stale, backoff now: %v)", interval)
	} else if isStale {
		d.logger.Println("Poked Deacon (stale)")
	} else {
		d.logger.Println("Poked Deacon")
	}
}

// pokeMayor sends a heartbeat to the Mayor session.
func (d *Daemon) pokeMayor() {
	mayorSession := constants.SessionMayor
	agentID := constants.RoleMayor

	running, err := d.tmux.HasSession(mayorSession)
	if err != nil {
		d.logger.Printf("Error checking Mayor session: %v", err)
		return
	}

	if !running {
		d.logger.Println("Mayor session not running, skipping poke")
		return
	}

	// Check keepalive to see if agent is active
	state := keepalive.Read(d.config.TownRoot)
	if state != nil && state.IsFresh() {
		// Agent is actively working, reset backoff and mark notifications consumed
		d.backoff.RecordActivity(agentID)
		_ = d.notifications.MarkConsumed(mayorSession, SlotHeartbeat)
		d.logger.Printf("Mayor is fresh (cmd: %s), skipping poke", state.LastCommand)
		return
	}

	// Check if we should poke based on backoff interval
	if !d.backoff.ShouldPoke(agentID) {
		interval := d.backoff.GetInterval(agentID)
		d.logger.Printf("Mayor backoff in effect (interval: %v), skipping poke", interval)
		return
	}

	// Check if we should send (slot-based deduplication)
	shouldSend, _ := d.notifications.ShouldSend(mayorSession, SlotHeartbeat)
	if !shouldSend {
		d.logger.Println("Heartbeat already pending for Mayor, skipping")
		return
	}

	// Send heartbeat message via tmux, replacing any pending input
	msg := "HEARTBEAT: check your rigs"
	if err := d.tmux.SendKeysReplace(mayorSession, msg, 50); err != nil {
		d.logger.Printf("Error poking Mayor: %v", err)
		return
	}

	// Record the send for slot deduplication
	_ = d.notifications.RecordSend(mayorSession, SlotHeartbeat, msg)
	d.backoff.RecordPoke(agentID)

	// If agent is stale or very stale, record a miss (increase backoff)
	if state == nil || state.IsVeryStale() {
		d.backoff.RecordMiss(agentID)
		interval := d.backoff.GetInterval(agentID)
		d.logger.Printf("Poked Mayor (very stale, backoff now: %v)", interval)
	} else if state.IsStale() {
		// Stale but not very stale - don't increase backoff, but don't reset either
		d.logger.Println("Poked Mayor (stale)")
	} else {
		d.logger.Println("Poked Mayor")
	}
}

// pokeWitnesses sends heartbeats to all Witness sessions.
// Uses proper rig discovery from rigs.json instead of scanning tmux sessions.
func (d *Daemon) pokeWitnesses() {
	// Discover rigs from configuration
	rigs := d.discoverRigs()
	if len(rigs) == 0 {
		d.logger.Println("No rigs discovered")
		return
	}

	for _, r := range rigs {
		session := fmt.Sprintf("gt-%s-witness", r.Name)

		// Check if witness session exists
		running, err := d.tmux.HasSession(session)
		if err != nil {
			d.logger.Printf("Error checking witness session for rig %s: %v", r.Name, err)
			continue
		}

		if !running {
			// Rig exists but no witness session - log for visibility
			d.logger.Printf("Rig %s has no witness session (may need: gt witness start %s)", r.Name, r.Name)
			continue
		}

		d.pokeWitness(session)
	}
}

// discoverRigs finds all registered rigs using the rig manager.
// Falls back to directory scanning if rigs.json is not available.
func (d *Daemon) discoverRigs() []*rig.Rig {
	// Load rigs config from mayor/rigs.json
	rigsConfigPath := constants.MayorRigsPath(d.config.TownRoot)
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		// Try fallback: scan town directory for rig directories
		return d.discoverRigsFromDirectory()
	}

	// Use rig manager for proper discovery
	g := git.NewGit(d.config.TownRoot)
	mgr := rig.NewManager(d.config.TownRoot, rigsConfig, g)
	rigs, err := mgr.DiscoverRigs()
	if err != nil {
		d.logger.Printf("Error discovering rigs from config: %v", err)
		return d.discoverRigsFromDirectory()
	}

	return rigs
}

// discoverRigsFromDirectory scans the town directory for rig directories.
// A directory is considered a rig if it has a .beads subdirectory or config.json.
func (d *Daemon) discoverRigsFromDirectory() []*rig.Rig {
	entries, err := os.ReadDir(d.config.TownRoot)
	if err != nil {
		d.logger.Printf("Error reading town directory: %v", err)
		return nil
	}

	var rigs []*rig.Rig
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		// Skip known non-rig directories
		if name == "mayor" || name == "daemon" || name == ".git" || name[0] == '.' {
			continue
		}

		dirPath := filepath.Join(d.config.TownRoot, name)

		// Check for .beads directory (indicates a rig)
		beadsPath := filepath.Join(dirPath, ".beads")
		if _, err := os.Stat(beadsPath); err == nil {
			rigs = append(rigs, &rig.Rig{Name: name, Path: dirPath})
			continue
		}

		// Check for config.json with type: rig
		configPath := filepath.Join(dirPath, "config.json")
		if _, err := os.Stat(configPath); err == nil {
			// For simplicity, assume any directory with config.json is a rig
			rigs = append(rigs, &rig.Rig{Name: name, Path: dirPath})
		}
	}

	return rigs
}

// pokeWitness sends a heartbeat to a single witness session with backoff.
func (d *Daemon) pokeWitness(session string) {
	// Extract rig name from session (gt-<rig>-witness -> <rig>)
	rigName := extractRigName(session)
	agentID := session // Use session name as agent ID

	// Find the rig's workspace for keepalive check
	rigWorkspace := filepath.Join(d.config.TownRoot, "gastown", rigName)

	// Check keepalive to see if the witness is active
	state := keepalive.Read(rigWorkspace)
	if state != nil && state.IsFresh() {
		// Witness is actively working, reset backoff and mark notifications consumed
		d.backoff.RecordActivity(agentID)
		_ = d.notifications.MarkConsumed(session, SlotHeartbeat)
		d.logger.Printf("Witness %s is fresh (cmd: %s), skipping poke", session, state.LastCommand)
		return
	}

	// Check if we should poke based on backoff interval
	if !d.backoff.ShouldPoke(agentID) {
		interval := d.backoff.GetInterval(agentID)
		d.logger.Printf("Witness %s backoff in effect (interval: %v), skipping poke", session, interval)
		return
	}

	// Check if we should send (slot-based deduplication)
	shouldSend, _ := d.notifications.ShouldSend(session, SlotHeartbeat)
	if !shouldSend {
		d.logger.Printf("Heartbeat already pending for Witness %s, skipping", session)
		return
	}

	// Send heartbeat message, replacing any pending input
	msg := "HEARTBEAT: check your workers"
	if err := d.tmux.SendKeysReplace(session, msg, 50); err != nil {
		d.logger.Printf("Error poking Witness %s: %v", session, err)
		return
	}

	// Record the send for slot deduplication
	_ = d.notifications.RecordSend(session, SlotHeartbeat, msg)
	d.backoff.RecordPoke(agentID)

	// If agent is stale or very stale, record a miss (increase backoff)
	if state == nil || state.IsVeryStale() {
		d.backoff.RecordMiss(agentID)
		interval := d.backoff.GetInterval(agentID)
		d.logger.Printf("Poked Witness %s (very stale, backoff now: %v)", session, interval)
	} else if state.IsStale() {
		d.logger.Printf("Poked Witness %s (stale)", session)
	} else {
		d.logger.Printf("Poked Witness %s", session)
	}
}

// extractRigName extracts the rig name from a witness session name.
// "gt-gastown-witness" -> "gastown"
func extractRigName(session string) string {
	// Remove "gt-" prefix and "-witness" suffix
	name := strings.TrimPrefix(session, "gt-")
	name = strings.TrimSuffix(name, "-witness")
	return name
}

// isWitnessSession checks if a session name is a witness session.
func isWitnessSession(name string) bool {
	// Pattern: gt-<rig>-witness
	if len(name) < 12 { // "gt-x-witness" minimum
		return false
	}
	return name[:3] == "gt-" && name[len(name)-8:] == "-witness"
}

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
