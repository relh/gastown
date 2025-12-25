package refinery

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/claude"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Common errors
var (
	ErrNotRunning    = errors.New("refinery not running")
	ErrAlreadyRunning = errors.New("refinery already running")
	ErrNoQueue       = errors.New("no items in queue")
)

// Manager handles refinery lifecycle and queue operations.
type Manager struct {
	rig     *rig.Rig
	workDir string
	output  io.Writer // Output destination for user-facing messages
}

// NewManager creates a new refinery manager for a rig.
func NewManager(r *rig.Rig) *Manager {
	return &Manager{
		rig:     r,
		workDir: r.Path,
		output:  os.Stdout,
	}
}

// SetOutput sets the output writer for user-facing messages.
// This is useful for testing or redirecting output.
func (m *Manager) SetOutput(w io.Writer) {
	m.output = w
}

// stateFile returns the path to the refinery state file.
func (m *Manager) stateFile() string {
	return filepath.Join(m.rig.Path, ".runtime", "refinery.json")
}

// sessionName returns the tmux session name for this refinery.
func (m *Manager) sessionName() string {
	return fmt.Sprintf("gt-%s-refinery", m.rig.Name)
}

// loadState loads refinery state from disk.
func (m *Manager) loadState() (*Refinery, error) {
	data, err := os.ReadFile(m.stateFile())
	if err != nil {
		if os.IsNotExist(err) {
			return &Refinery{
				RigName: m.rig.Name,
				State:   StateStopped,
			}, nil
		}
		return nil, err
	}

	var ref Refinery
	if err := json.Unmarshal(data, &ref); err != nil {
		return nil, err
	}

	return &ref, nil
}

// saveState persists refinery state to disk.
func (m *Manager) saveState(ref *Refinery) error {
	dir := filepath.Dir(m.stateFile())
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(ref, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(m.stateFile(), data, 0644)
}

// Status returns the current refinery status.
func (m *Manager) Status() (*Refinery, error) {
	ref, err := m.loadState()
	if err != nil {
		return nil, err
	}

	// Check if tmux session exists
	t := tmux.NewTmux()
	sessionID := m.sessionName()
	sessionRunning, _ := t.HasSession(sessionID)

	// If tmux session is running, refinery is running
	if sessionRunning {
		if ref.State != StateRunning {
			// Update state to match reality
			now := time.Now()
			ref.State = StateRunning
			if ref.StartedAt == nil {
				ref.StartedAt = &now
			}
			_ = m.saveState(ref)
		}
		return ref, nil
	}

	// If state says running but tmux session doesn't exist, check PID
	if ref.State == StateRunning {
		if ref.PID > 0 && processExists(ref.PID) {
			// Process is still running (foreground mode without tmux)
			return ref, nil
		}
		// Neither session nor process exists - mark as stopped
		ref.State = StateStopped
		ref.PID = 0
		_ = m.saveState(ref)
	}

	return ref, nil
}

// Start starts the refinery.
// If foreground is true, runs in the current process (blocking) using the Go-based polling loop.
// Otherwise, spawns a Claude agent in a tmux session to process the merge queue.
func (m *Manager) Start(foreground bool) error {
	ref, err := m.loadState()
	if err != nil {
		return err
	}

	t := tmux.NewTmux()
	sessionID := m.sessionName()

	if foreground {
		// In foreground mode, we're likely running inside the tmux session
		// that background mode created. Only check PID to avoid self-detection.
		if ref.State == StateRunning && ref.PID > 0 && processExists(ref.PID) {
			return ErrAlreadyRunning
		}

		// Running in foreground - update state and run the Go-based polling loop
		now := time.Now()
		ref.State = StateRunning
		ref.StartedAt = &now
		ref.PID = os.Getpid()

		if err := m.saveState(ref); err != nil {
			return err
		}

		// Run the processing loop (blocking)
		return m.run(ref)
	}

	// Background mode: check if session already exists
	running, _ := t.HasSession(sessionID)
	if running {
		return ErrAlreadyRunning
	}

	// Also check via PID for backwards compatibility
	if ref.State == StateRunning && ref.PID > 0 && processExists(ref.PID) {
		return ErrAlreadyRunning
	}

	// Background mode: spawn a Claude agent in a tmux session
	// The Claude agent handles MR processing using git commands and beads

	// Working directory is the refinery worktree (shares .git with mayor/polecats)
	refineryRigDir := filepath.Join(m.rig.Path, "refinery", "rig")
	if _, err := os.Stat(refineryRigDir); os.IsNotExist(err) {
		// Fall back to rig path if refinery/rig doesn't exist
		refineryRigDir = m.workDir
	}

	// Ensure Claude settings exist (autonomous role needs mail in SessionStart)
	if err := claude.EnsureSettingsForRole(refineryRigDir, "refinery"); err != nil {
		return fmt.Errorf("ensuring Claude settings: %w", err)
	}

	if err := t.NewSession(sessionID, refineryRigDir); err != nil {
		return fmt.Errorf("creating tmux session: %w", err)
	}

	// Set environment variables
	bdActor := fmt.Sprintf("%s/refinery", m.rig.Name)
	_ = t.SetEnvironment(sessionID, "GT_RIG", m.rig.Name)
	_ = t.SetEnvironment(sessionID, "GT_REFINERY", "1")
	_ = t.SetEnvironment(sessionID, "GT_ROLE", "refinery")
	_ = t.SetEnvironment(sessionID, "BD_ACTOR", bdActor)

	// Set beads environment - refinery uses rig-level beads
	beadsDir := filepath.Join(m.rig.Path, "mayor", "rig", ".beads")
	_ = t.SetEnvironment(sessionID, "BEADS_DIR", beadsDir)
	_ = t.SetEnvironment(sessionID, "BEADS_NO_DAEMON", "1")
	_ = t.SetEnvironment(sessionID, "BEADS_AGENT_NAME", fmt.Sprintf("%s/refinery", m.rig.Name))

	// Apply theme (same as rig polecats)
	theme := tmux.AssignTheme(m.rig.Name)
	_ = t.ConfigureGasTownSession(sessionID, theme, m.rig.Name, "refinery", "refinery")

	// Update state to running
	now := time.Now()
	ref.State = StateRunning
	ref.StartedAt = &now
	ref.PID = 0 // Claude agent doesn't have a PID we track
	if err := m.saveState(ref); err != nil {
		_ = t.KillSession(sessionID)
		return fmt.Errorf("saving state: %w", err)
	}

	// Start Claude agent with full permissions (like polecats)
	// NOTE: No gt prime injection needed - SessionStart hook handles it automatically
	// Restarts are handled by daemon via LIFECYCLE mail, not shell loops
	command := "claude --dangerously-skip-permissions"
	if err := t.SendKeys(sessionID, command); err != nil {
		// Clean up the session on failure
		_ = t.KillSession(sessionID)
		return fmt.Errorf("starting Claude agent: %w", err)
	}

	return nil
}

// Stop stops the refinery.
func (m *Manager) Stop() error {
	ref, err := m.loadState()
	if err != nil {
		return err
	}

	// Check if tmux session exists
	t := tmux.NewTmux()
	sessionID := m.sessionName()
	sessionRunning, _ := t.HasSession(sessionID)

	// If neither state nor session indicates running, it's not running
	if ref.State != StateRunning && !sessionRunning {
		return ErrNotRunning
	}

	// Kill tmux session if it exists
	if sessionRunning {
		_ = t.KillSession(sessionID)
	}

	// If we have a PID and it's a different process, try to stop it gracefully
	if ref.PID > 0 && ref.PID != os.Getpid() && processExists(ref.PID) {
		// Send SIGTERM
		if proc, err := os.FindProcess(ref.PID); err == nil {
			_ = proc.Signal(os.Interrupt)
		}
	}

	ref.State = StateStopped
	ref.PID = 0

	return m.saveState(ref)
}

// Queue returns the current merge queue.
func (m *Manager) Queue() ([]QueueItem, error) {
	// Discover branches that look like polecat work branches
	branches, err := m.discoverWorkBranches()
	if err != nil {
		return nil, err
	}

	// Load any pending MRs from state
	ref, err := m.loadState()
	if err != nil {
		return nil, err
	}

	// Build queue items
	var items []QueueItem
	pos := 1

	// Add current processing item
	if ref.CurrentMR != nil {
		items = append(items, QueueItem{
			Position: 0, // 0 = currently processing
			MR:       ref.CurrentMR,
			Age:      formatAge(ref.CurrentMR.CreatedAt),
		})
	}

	// Add discovered branches as pending
	for _, branch := range branches {
		mr := m.branchToMR(branch)
		if mr != nil {
			items = append(items, QueueItem{
				Position: pos,
				MR:       mr,
				Age:      formatAge(mr.CreatedAt),
			})
			pos++
		}
	}

	return items, nil
}

// discoverWorkBranches finds branches that look like polecat work.
func (m *Manager) discoverWorkBranches() ([]string, error) {
	cmd := exec.Command("git", "branch", "-r", "--list", "origin/polecat/*")
	cmd.Dir = m.workDir

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		return nil, nil // No remote branches
	}

	var branches []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		branch := strings.TrimSpace(line)
		if branch != "" && !strings.Contains(branch, "->") {
			// Remove origin/ prefix
			branch = strings.TrimPrefix(branch, "origin/")
			branches = append(branches, branch)
		}
	}

	return branches, nil
}

// branchToMR converts a branch name to a merge request.
func (m *Manager) branchToMR(branch string) *MergeRequest {
	// Expected format: polecat/<worker>/<issue> or polecat/<worker>
	pattern := regexp.MustCompile(`^polecat/([^/]+)(?:/(.+))?$`)
	matches := pattern.FindStringSubmatch(branch)
	if matches == nil {
		return nil
	}

	worker := matches[1]
	issueID := ""
	if len(matches) > 2 {
		issueID = matches[2]
	}

	return &MergeRequest{
		ID:           fmt.Sprintf("mr-%s-%d", worker, time.Now().Unix()),
		Branch:       branch,
		Worker:       worker,
		IssueID:      issueID,
		TargetBranch: "main", // Default; swarm would use integration branch
		CreatedAt:    time.Now(), // Would ideally get from git
		Status:       MROpen,
	}
}

// run is the main processing loop (for foreground mode).
func (m *Manager) run(ref *Refinery) error {
	fmt.Fprintln(m.output, "Refinery running...")
	fmt.Fprintln(m.output, "Press Ctrl+C to stop")

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		// Process queue
		if err := m.ProcessQueue(); err != nil {
			fmt.Fprintf(m.output, "Queue processing error: %v\n", err)
		}
	}
	return nil
}

// ProcessQueue processes all pending merge requests.
func (m *Manager) ProcessQueue() error {
	queue, err := m.Queue()
	if err != nil {
		return err
	}

	for _, item := range queue {
		if !item.MR.IsOpen() {
			continue
		}

		fmt.Fprintf(m.output, "Processing: %s (%s)\n", item.MR.Branch, item.MR.Worker)

		result := m.ProcessMR(item.MR)
		if result.Success {
			fmt.Fprintln(m.output, "  ✓ Merged successfully")
		} else {
			fmt.Fprintf(m.output, "  ✗ Failed: %s\n", result.Error)
		}
	}

	return nil
}

// MergeResult contains the result of a merge attempt.
type MergeResult struct {
	Success     bool
	MergeCommit string // SHA of merge commit on success
	Error       string
	Conflict    bool
	TestsFailed bool
}

// ProcessMR processes a single merge request.
func (m *Manager) ProcessMR(mr *MergeRequest) MergeResult {
	ref, _ := m.loadState()
	config := m.getMergeConfig()

	// Claim the MR (open → in_progress)
	if err := mr.Claim(); err != nil {
		return MergeResult{Error: fmt.Sprintf("cannot claim MR: %v", err)}
	}
	ref.CurrentMR = mr
	_ = m.saveState(ref)

	result := MergeResult{}

	// 1. Fetch the branch
	if err := m.gitRun("fetch", "origin", mr.Branch); err != nil {
		result.Error = fmt.Sprintf("fetch failed: %v", err)
		m.completeMR(mr, "", result.Error) // Reopen for retry
		return result
	}

	// 2. Checkout target branch
	if err := m.gitRun("checkout", mr.TargetBranch); err != nil {
		result.Error = fmt.Sprintf("checkout target failed: %v", err)
		m.completeMR(mr, "", result.Error) // Reopen for retry
		return result
	}

	// Pull latest
	_ = m.gitRun("pull", "origin", mr.TargetBranch) // Ignore errors

	// 3. Merge
	err := m.gitRun("merge", "--no-ff", "-m",
		fmt.Sprintf("Merge %s from %s", mr.Branch, mr.Worker),
		"origin/"+mr.Branch)

	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "CONFLICT") || strings.Contains(errStr, "conflict") {
			result.Conflict = true
			result.Error = "merge conflict"
			// Abort the merge
			_ = m.gitRun("merge", "--abort")
			m.completeMR(mr, "", "merge conflict - polecat must rebase") // Reopen for rebase
			// Notify worker about conflict
			m.notifyWorkerConflict(mr)
			return result
		}
		result.Error = fmt.Sprintf("merge failed: %v", err)
		m.completeMR(mr, "", result.Error) // Reopen for retry
		return result
	}

	// 4. Run tests if configured
	if config.RunTests && config.TestCommand != "" {
		if err := m.runTests(config.TestCommand); err != nil {
			result.TestsFailed = true
			result.Error = fmt.Sprintf("tests failed: %v", err)
			// Reset to before merge
			_ = m.gitRun("reset", "--hard", "HEAD~1")
			m.completeMR(mr, "", result.Error) // Reopen for fixes
			return result
		}
	}

	// 5. Push with retry logic
	if err := m.pushWithRetry(mr.TargetBranch, config); err != nil {
		result.Error = fmt.Sprintf("push failed: %v", err)
		// Reset to before merge
		_ = m.gitRun("reset", "--hard", "HEAD~1")
		m.completeMR(mr, "", result.Error) // Reopen for retry
		return result
	}

	// 6. Get merge commit SHA
	mergeCommit, err := m.gitOutput("rev-parse", "HEAD")
	if err != nil {
		mergeCommit = "" // Non-fatal, continue
	}

	// Success!
	result.Success = true
	result.MergeCommit = mergeCommit
	m.completeMR(mr, CloseReasonMerged, "")

	// Notify worker of success
	m.notifyWorkerMerged(mr)

	// Optionally delete the merged branch (local only - branches never go to origin)
	if config.DeleteMergedBranches {
		_ = m.gitRun("branch", "-D", mr.Branch)
	}

	return result
}

// completeMR marks an MR as complete and updates stats.
// For success, pass closeReason (e.g., CloseReasonMerged).
// For failures that should return to open, pass empty closeReason.
func (m *Manager) completeMR(mr *MergeRequest, closeReason CloseReason, errMsg string) {
	ref, _ := m.loadState()
	mr.Error = errMsg
	ref.CurrentMR = nil

	now := time.Now()

	if closeReason != "" {
		// Close the MR (in_progress → closed)
		if err := mr.Close(closeReason); err != nil {
			// Log error but continue - this shouldn't happen
			fmt.Fprintf(m.output, "Warning: failed to close MR: %v\n", err)
		}
		switch closeReason {
		case CloseReasonMerged:
			ref.LastMergeAt = &now
			ref.Stats.TotalMerged++
			ref.Stats.TodayMerged++
		case CloseReasonSuperseded:
			ref.Stats.TotalSkipped++
		default:
			// Other close reasons (rejected, conflict) count as failed
			ref.Stats.TotalFailed++
			ref.Stats.TodayFailed++
		}
	} else {
		// Reopen the MR for rework (in_progress → open)
		if err := mr.Reopen(); err != nil {
			// Log error but continue
			fmt.Fprintf(m.output, "Warning: failed to reopen MR: %v\n", err)
		}
		ref.Stats.TotalFailed++
		ref.Stats.TodayFailed++
	}

	_ = m.saveState(ref)
}

// getTestCommand returns the test command if configured.
func (m *Manager) getTestCommand() string {
	// Check settings/config.json for test_command
	settingsPath := filepath.Join(m.rig.Path, "settings", "config.json")
	settings, err := config.LoadRigSettings(settingsPath)
	if err != nil {
		return ""
	}
	if settings.MergeQueue != nil && settings.MergeQueue.TestCommand != "" {
		return settings.MergeQueue.TestCommand
	}
	return ""
}

// runTests executes the test command.
func (m *Manager) runTests(testCmd string) error {
	parts := strings.Fields(testCmd)
	if len(parts) == 0 {
		return nil
	}

	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Dir = m.workDir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(stderr.String()))
	}

	return nil
}

// gitRun executes a git command.
func (m *Manager) gitRun(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = m.workDir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("%s", errMsg)
		}
		return err
	}

	return nil
}

// gitOutput executes a git command and returns stdout.
func (m *Manager) gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = m.workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return "", fmt.Errorf("%s", errMsg)
		}
		return "", err
	}

	return strings.TrimSpace(stdout.String()), nil
}

// getMergeConfig loads the merge configuration from disk.
// Returns default config if not configured.
func (m *Manager) getMergeConfig() MergeConfig {
	mergeConfig := DefaultMergeConfig()

	// Check settings/config.json for merge_queue settings
	settingsPath := filepath.Join(m.rig.Path, "settings", "config.json")
	settings, err := config.LoadRigSettings(settingsPath)
	if err != nil {
		return mergeConfig
	}

	// Apply merge_queue config if present
	if settings.MergeQueue != nil {
		mq := settings.MergeQueue
		mergeConfig.TestCommand = mq.TestCommand
		mergeConfig.RunTests = mq.RunTests
		mergeConfig.DeleteMergedBranches = mq.DeleteMergedBranches
		// Note: PushRetryCount and PushRetryDelayMs use defaults if not explicitly set
	}

	return mergeConfig
}

// pushWithRetry pushes to the target branch with exponential backoff retry.
func (m *Manager) pushWithRetry(targetBranch string, config MergeConfig) error {
	var lastErr error
	delay := time.Duration(config.PushRetryDelayMs) * time.Millisecond

	for attempt := 0; attempt <= config.PushRetryCount; attempt++ {
		if attempt > 0 {
			fmt.Fprintf(m.output, "Push retry %d/%d after %v\n", attempt, config.PushRetryCount, delay)
			time.Sleep(delay)
			delay *= 2 // Exponential backoff
		}

		err := m.gitRun("push", "origin", targetBranch)
		if err == nil {
			return nil // Success
		}
		lastErr = err
	}

	return fmt.Errorf("push failed after %d retries: %v", config.PushRetryCount, lastErr)
}

// processExists checks if a process with the given PID exists.
func processExists(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds; signal 0 tests existence
	err = proc.Signal(nil)
	return err == nil
}

// formatAge formats a duration since the given time.
func formatAge(t time.Time) string {
	d := time.Since(t)

	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// notifyWorkerConflict sends a conflict notification to a polecat.
func (m *Manager) notifyWorkerConflict(mr *MergeRequest) {
	router := mail.NewRouter(m.workDir)
	msg := &mail.Message{
		From: fmt.Sprintf("%s/refinery", m.rig.Name),
		To:   fmt.Sprintf("%s/%s", m.rig.Name, mr.Worker),
		Subject: "Merge conflict - rebase required",
		Body: fmt.Sprintf(`Your branch %s has conflicts with %s.

Please rebase your changes:
  git fetch origin
  git rebase origin/%s
  git push -f

Then the Refinery will retry the merge.`,
			mr.Branch, mr.TargetBranch, mr.TargetBranch),
		Priority: mail.PriorityHigh,
	}
	_ = router.Send(msg)
}

// notifyWorkerMerged sends a success notification to a polecat.
func (m *Manager) notifyWorkerMerged(mr *MergeRequest) {
	router := mail.NewRouter(m.workDir)
	msg := &mail.Message{
		From: fmt.Sprintf("%s/refinery", m.rig.Name),
		To:   fmt.Sprintf("%s/%s", m.rig.Name, mr.Worker),
		Subject: "Work merged successfully",
		Body: fmt.Sprintf(`Your branch %s has been merged to %s.

Issue: %s
Thank you for your contribution!`,
			mr.Branch, mr.TargetBranch, mr.IssueID),
	}
	_ = router.Send(msg)
}

// Common errors for MR operations
var (
	ErrMRNotFound  = errors.New("merge request not found")
	ErrMRNotFailed = errors.New("merge request has not failed")
)

// GetMR returns a merge request by ID from the state.
func (m *Manager) GetMR(id string) (*MergeRequest, error) {
	ref, err := m.loadState()
	if err != nil {
		return nil, err
	}

	// Check if it's the current MR
	if ref.CurrentMR != nil && ref.CurrentMR.ID == id {
		return ref.CurrentMR, nil
	}

	// Check pending MRs
	if ref.PendingMRs != nil {
		if mr, ok := ref.PendingMRs[id]; ok {
			return mr, nil
		}
	}

	return nil, ErrMRNotFound
}

// FindMR finds a merge request by ID or branch name in the queue.
func (m *Manager) FindMR(idOrBranch string) (*MergeRequest, error) {
	queue, err := m.Queue()
	if err != nil {
		return nil, err
	}

	for _, item := range queue {
		// Match by ID
		if item.MR.ID == idOrBranch {
			return item.MR, nil
		}
		// Match by branch name (with or without polecat/ prefix)
		if item.MR.Branch == idOrBranch {
			return item.MR, nil
		}
		if "polecat/"+idOrBranch == item.MR.Branch {
			return item.MR, nil
		}
		// Match by worker name (partial match for convenience)
		if strings.Contains(item.MR.ID, idOrBranch) {
			return item.MR, nil
		}
	}

	return nil, ErrMRNotFound
}

// Retry resets a failed merge request so it can be processed again.
// If processNow is true, immediately processes the MR instead of waiting for the loop.
func (m *Manager) Retry(id string, processNow bool) error {
	ref, err := m.loadState()
	if err != nil {
		return err
	}

	// Find the MR
	var mr *MergeRequest
	if ref.PendingMRs != nil {
		mr = ref.PendingMRs[id]
	}
	if mr == nil {
		return ErrMRNotFound
	}

	// Verify it's in a failed state (open with an error)
	if mr.Status != MROpen || mr.Error == "" {
		return ErrMRNotFailed
	}

	// Clear the error to mark as ready for retry
	mr.Error = ""

	// Save the state
	if err := m.saveState(ref); err != nil {
		return err
	}

	// If --now flag, process immediately
	if processNow {
		result := m.ProcessMR(mr)
		if !result.Success {
			return fmt.Errorf("retry failed: %s", result.Error)
		}
	}

	return nil
}

// RegisterMR adds a merge request to the pending queue.
func (m *Manager) RegisterMR(mr *MergeRequest) error {
	ref, err := m.loadState()
	if err != nil {
		return err
	}

	if ref.PendingMRs == nil {
		ref.PendingMRs = make(map[string]*MergeRequest)
	}

	ref.PendingMRs[mr.ID] = mr
	return m.saveState(ref)
}

// RejectMR manually rejects a merge request.
// It closes the MR with rejected status and optionally notifies the worker.
// Returns the rejected MR for display purposes.
func (m *Manager) RejectMR(idOrBranch string, reason string, notify bool) (*MergeRequest, error) {
	mr, err := m.FindMR(idOrBranch)
	if err != nil {
		return nil, err
	}

	// Verify MR is open or in_progress (can't reject already closed)
	if mr.IsClosed() {
		return nil, fmt.Errorf("%w: MR is already closed with reason: %s", ErrClosedImmutable, mr.CloseReason)
	}

	// Close with rejected reason
	if err := mr.Close(CloseReasonRejected); err != nil {
		return nil, fmt.Errorf("failed to close MR: %w", err)
	}
	mr.Error = reason

	// Optionally notify worker
	if notify {
		m.notifyWorkerRejected(mr, reason)
	}

	return mr, nil
}

// notifyWorkerRejected sends a rejection notification to a polecat.
func (m *Manager) notifyWorkerRejected(mr *MergeRequest, reason string) {
	router := mail.NewRouter(m.workDir)
	msg := &mail.Message{
		From:    fmt.Sprintf("%s/refinery", m.rig.Name),
		To:      fmt.Sprintf("%s/%s", m.rig.Name, mr.Worker),
		Subject: "Merge request rejected",
		Body: fmt.Sprintf(`Your merge request has been rejected.

Branch: %s
Issue: %s
Reason: %s

Please review the feedback and address the issues before resubmitting.`,
			mr.Branch, mr.IssueID, reason),
		Priority: mail.PriorityNormal,
	}
	_ = router.Send(msg)
}

// findTownRoot walks up directories to find the town root.
func findTownRoot(startPath string) string {
	path := startPath
	for {
		// Check for mayor/ subdirectory (indicates town root)
		if _, err := os.Stat(filepath.Join(path, "mayor")); err == nil {
			return path
		}
		// Check for config.json with type: workspace
		configPath := filepath.Join(path, "config.json")
		if data, err := os.ReadFile(configPath); err == nil {
			if strings.Contains(string(data), `"type": "workspace"`) {
				return path
			}
		}

		parent := filepath.Dir(path)
		if parent == path {
			break // Reached root
		}
		path = parent
	}
	return ""
}
