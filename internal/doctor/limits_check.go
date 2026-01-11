package doctor

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Target limits for file descriptors and inotify.
// These values support heavy development workloads with multiple
// file watchers (IDEs, bundlers, daemons, Claude instances).
const (
	// TargetFileDescriptors is the target for both soft and hard limits.
	TargetFileDescriptors = 1048576

	// TargetInotifyWatches is the target for max_user_watches.
	TargetInotifyWatches = 524288

	// TargetInotifyInstances is the target for max_user_instances.
	TargetInotifyInstances = 1024
)

// Platform represents the detected runtime environment.
type Platform int

const (
	PlatformUnknown Platform = iota
	PlatformLinuxBareMetal
	PlatformLinuxContainer
	PlatformWSL
	PlatformMacOS
)

func (p Platform) String() string {
	switch p {
	case PlatformLinuxBareMetal:
		return "Linux (bare metal)"
	case PlatformLinuxContainer:
		return "Linux (container)"
	case PlatformWSL:
		return "WSL"
	case PlatformMacOS:
		return "macOS"
	default:
		return "Unknown"
	}
}

// LimitsCheck verifies file descriptor and inotify limits are adequate.
type LimitsCheck struct {
	BaseCheck
	// Diagnostic data collected during Run()
	platform      Platform
	fdSoft        uint64
	fdHard        uint64
	watches       int
	instances     int
	pamEnabled    bool
	limitsConf    map[string]string // limits.conf entries
	openFDs       map[string]int    // process name -> open FD count
	issues        []string
	fixScript     string
}

// NewLimitsCheck creates a new limits check.
func NewLimitsCheck() *LimitsCheck {
	return &LimitsCheck{
		BaseCheck: BaseCheck{
			CheckName:        "limits",
			CheckDescription: "Check file descriptor and inotify limits",
			CheckCategory:    CategoryInfrastructure,
		},
		limitsConf: make(map[string]string),
		openFDs:    make(map[string]int),
	}
}

// Run checks current limits against targets and generates fix script.
func (c *LimitsCheck) Run(ctx *CheckContext) *CheckResult {
	c.issues = nil
	c.fixScript = ""
	details := []string{}

	// Detect platform
	c.platform = detectPlatform()
	details = append(details, fmt.Sprintf("Platform: %s", c.platform))

	// Check file descriptor limits
	var rlimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlimit); err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusError,
			Message: "Failed to get file descriptor limits",
			Details: []string{err.Error()},
		}
	}
	c.fdSoft = rlimit.Cur
	c.fdHard = rlimit.Max

	details = append(details, fmt.Sprintf("File descriptors: %d soft / %d hard (target: %d)", c.fdSoft, c.fdHard, TargetFileDescriptors))

	if c.fdSoft < TargetFileDescriptors {
		c.issues = append(c.issues, fmt.Sprintf("nofile soft limit (%d) below target (%d)", c.fdSoft, TargetFileDescriptors))
	}
	if c.fdHard < TargetFileDescriptors {
		c.issues = append(c.issues, fmt.Sprintf("nofile hard limit (%d) below target (%d)", c.fdHard, TargetFileDescriptors))
	}

	// Linux-specific checks
	if runtime.GOOS == "linux" {
		// Check inotify limits
		if watches, err := readProcInt("/proc/sys/fs/inotify/max_user_watches"); err == nil {
			c.watches = watches
			details = append(details, fmt.Sprintf("inotify max_user_watches: %d (target: %d)", watches, TargetInotifyWatches))
			if watches < TargetInotifyWatches {
				c.issues = append(c.issues, fmt.Sprintf("max_user_watches (%d) below target (%d)", watches, TargetInotifyWatches))
			}
		}

		if instances, err := readProcInt("/proc/sys/fs/inotify/max_user_instances"); err == nil {
			c.instances = instances
			details = append(details, fmt.Sprintf("inotify max_user_instances: %d (target: %d)", instances, TargetInotifyInstances))
			if instances < TargetInotifyInstances {
				c.issues = append(c.issues, fmt.Sprintf("max_user_instances (%d) below target (%d)", instances, TargetInotifyInstances))
			}
		}

		// Check pam_limits.so
		c.pamEnabled = checkPamLimits()
		if c.pamEnabled {
			details = append(details, "pam_limits.so: enabled")
		} else {
			details = append(details, "pam_limits.so: not detected")
		}

		// Check limits.conf
		c.limitsConf = parseLimitsConf()
		if len(c.limitsConf) > 0 {
			for k, v := range c.limitsConf {
				details = append(details, fmt.Sprintf("limits.conf: %s = %s", k, v))
			}
		}
	}

	// macOS-specific checks
	if runtime.GOOS == "darwin" {
		if maxfiles := getMacOSMaxFiles(); maxfiles > 0 {
			details = append(details, fmt.Sprintf("launchctl maxfiles: %d", maxfiles))
		}
	}

	// Count open file descriptors for gt-related processes
	c.openFDs = countGtProcessFDs()
	if len(c.openFDs) > 0 {
		details = append(details, "Open FDs by process:")
		for proc, count := range c.openFDs {
			details = append(details, fmt.Sprintf("  %s: %d", proc, count))
		}
	}

	// Generate fix script if there are issues
	if len(c.issues) > 0 {
		c.fixScript = c.generateFixScript()
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d limit(s) below target", len(c.issues)),
			Details: append(details, "", "Fix script (review and run with sudo):", c.fixScript),
			FixHint: "Run 'gt doctor --fix' to see the fix script",
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusOK,
		Message: "All limits adequate",
		Details: details,
	}
}

// CanFix returns true - we generate a fix script.
func (c *LimitsCheck) CanFix() bool {
	return true
}

// Fix prints the fix script for the user to review and execute.
func (c *LimitsCheck) Fix(ctx *CheckContext) error {
	if c.fixScript == "" {
		return nil // Nothing to fix
	}

	// Print the script for the user to review
	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("LIMITS FIX SCRIPT")
	fmt.Println("Review the commands below, then copy/paste to execute:")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println(c.fixScript)
	fmt.Println(strings.Repeat("=", 60))

	return fmt.Errorf("manual execution required - review and run the script above")
}

// generateFixScript creates a platform-specific fix script.
func (c *LimitsCheck) generateFixScript() string {
	var sb strings.Builder

	sb.WriteString("#!/bin/bash\n")
	sb.WriteString("# Gas Town limits fix script\n")
	sb.WriteString(fmt.Sprintf("# Platform: %s\n", c.platform))
	sb.WriteString("# Generated by: gt doctor limits\n\n")

	sb.WriteString("set -e\n\n")

	switch c.platform {
	case PlatformWSL:
		c.writeWSLFixes(&sb)
	case PlatformMacOS:
		c.writeMacOSFixes(&sb)
	case PlatformLinuxContainer:
		c.writeContainerFixes(&sb)
	default:
		c.writeLinuxFixes(&sb)
	}

	// Add verification commands
	sb.WriteString("\n# Verification\n")
	sb.WriteString("echo '\\n=== Verification ==='\n")
	sb.WriteString("echo 'Current limits:'\n")
	sb.WriteString("ulimit -n\n")
	if runtime.GOOS == "linux" {
		sb.WriteString("cat /proc/sys/fs/inotify/max_user_watches\n")
		sb.WriteString("cat /proc/sys/fs/inotify/max_user_instances\n")
	}

	return sb.String()
}

func (c *LimitsCheck) writeLinuxFixes(sb *strings.Builder) {
	sb.WriteString("# Linux bare metal fixes\n\n")

	// limits.conf for nofile
	if c.fdSoft < TargetFileDescriptors || c.fdHard < TargetFileDescriptors {
		sb.WriteString("# 1. Update /etc/security/limits.conf\n")
		sb.WriteString("cat >> /etc/security/limits.conf << 'EOF'\n")
		sb.WriteString(fmt.Sprintf("* soft nofile %d\n", TargetFileDescriptors))
		sb.WriteString(fmt.Sprintf("* hard nofile %d\n", TargetFileDescriptors))
		sb.WriteString(fmt.Sprintf("root soft nofile %d\n", TargetFileDescriptors))
		sb.WriteString(fmt.Sprintf("root hard nofile %d\n", TargetFileDescriptors))
		sb.WriteString("EOF\n\n")
	}

	// sysctl for inotify
	if c.watches < TargetInotifyWatches || c.instances < TargetInotifyInstances {
		sb.WriteString("# 2. Update sysctl settings\n")
		sb.WriteString("cat > /etc/sysctl.d/99-gastown.conf << 'EOF'\n")
		sb.WriteString(fmt.Sprintf("fs.inotify.max_user_watches = %d\n", TargetInotifyWatches))
		sb.WriteString(fmt.Sprintf("fs.inotify.max_user_instances = %d\n", TargetInotifyInstances))
		sb.WriteString("EOF\n\n")

		sb.WriteString("# Apply sysctl changes immediately\n")
		sb.WriteString("sysctl -p /etc/sysctl.d/99-gastown.conf\n\n")
	}

	sb.WriteString("# 3. Note: You may need to log out and back in for limits.conf changes\n")
}

func (c *LimitsCheck) writeWSLFixes(sb *strings.Builder) {
	sb.WriteString("# WSL fixes\n")
	sb.WriteString("# NOTE: After running these commands, you MUST restart WSL:\n")
	sb.WriteString("#   wsl --shutdown\n")
	sb.WriteString("# Then reopen your WSL terminal.\n\n")

	// Same as Linux for the actual fixes
	c.writeLinuxFixes(sb)

	sb.WriteString("\n# IMPORTANT: Run this from Windows PowerShell after the above:\n")
	sb.WriteString("# wsl --shutdown\n")
	sb.WriteString("# Then reopen your WSL terminal for changes to take effect.\n")
}

func (c *LimitsCheck) writeMacOSFixes(sb *strings.Builder) {
	sb.WriteString("# macOS fixes\n\n")

	if c.fdSoft < TargetFileDescriptors || c.fdHard < TargetFileDescriptors {
		sb.WriteString("# 1. Set maxfiles limit\n")
		sb.WriteString(fmt.Sprintf("sudo launchctl limit maxfiles %d %d\n\n", TargetFileDescriptors, TargetFileDescriptors))

		sb.WriteString("# 2. Create persistent launchd plist\n")
		sb.WriteString("cat > /Library/LaunchDaemons/limit.maxfiles.plist << 'EOF'\n")
		sb.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
		sb.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
		sb.WriteString("<plist version=\"1.0\">\n")
		sb.WriteString("  <dict>\n")
		sb.WriteString("    <key>Label</key>\n")
		sb.WriteString("    <string>limit.maxfiles</string>\n")
		sb.WriteString("    <key>ProgramArguments</key>\n")
		sb.WriteString("    <array>\n")
		sb.WriteString("      <string>launchctl</string>\n")
		sb.WriteString("      <string>limit</string>\n")
		sb.WriteString("      <string>maxfiles</string>\n")
		sb.WriteString(fmt.Sprintf("      <string>%d</string>\n", TargetFileDescriptors))
		sb.WriteString(fmt.Sprintf("      <string>%d</string>\n", TargetFileDescriptors))
		sb.WriteString("    </array>\n")
		sb.WriteString("    <key>RunAtLoad</key>\n")
		sb.WriteString("    <true/>\n")
		sb.WriteString("  </dict>\n")
		sb.WriteString("</plist>\n")
		sb.WriteString("EOF\n\n")

		sb.WriteString("# Load the new limit\n")
		sb.WriteString("sudo launchctl load -w /Library/LaunchDaemons/limit.maxfiles.plist\n\n")
	}

	sb.WriteString("# Note: macOS doesn't have inotify (uses FSEvents instead)\n")
}

func (c *LimitsCheck) writeContainerFixes(sb *strings.Builder) {
	sb.WriteString("# Container fixes\n")
	sb.WriteString("# NOTE: Container limits are often constrained by the host.\n")
	sb.WriteString("# You may need to adjust these settings on the host system.\n\n")

	// Check if we can modify these values
	sb.WriteString("# Try to apply sysctl changes (may fail in containers)\n")
	if c.watches < TargetInotifyWatches || c.instances < TargetInotifyInstances {
		sb.WriteString(fmt.Sprintf("sysctl -w fs.inotify.max_user_watches=%d || echo 'Cannot modify max_user_watches in container'\n", TargetInotifyWatches))
		sb.WriteString(fmt.Sprintf("sysctl -w fs.inotify.max_user_instances=%d || echo 'Cannot modify max_user_instances in container'\n\n", TargetInotifyInstances))
	}

	sb.WriteString("# If running Docker, add these to your docker run command:\n")
	sb.WriteString("#   --sysctl fs.inotify.max_user_watches=524288\n")
	sb.WriteString("#   --sysctl fs.inotify.max_user_instances=1024\n")
	sb.WriteString("#   --ulimit nofile=1048576:1048576\n\n")

	sb.WriteString("# For Kubernetes, set in SecurityContext:\n")
	sb.WriteString("#   securityContext:\n")
	sb.WriteString("#     sysctls:\n")
	sb.WriteString("#       - name: fs.inotify.max_user_watches\n")
	sb.WriteString("#         value: \"524288\"\n")
}

// detectPlatform identifies the runtime environment.
func detectPlatform() Platform {
	if runtime.GOOS == "darwin" {
		return PlatformMacOS
	}

	if runtime.GOOS != "linux" {
		return PlatformUnknown
	}

	// Check for WSL
	if isWSL() {
		return PlatformWSL
	}

	// Check for container
	if isContainer() {
		return PlatformLinuxContainer
	}

	return PlatformLinuxBareMetal
}

// isWSL checks if running in Windows Subsystem for Linux.
func isWSL() bool {
	// Check /proc/version for Microsoft or WSL
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	version := strings.ToLower(string(data))
	return strings.Contains(version, "microsoft") || strings.Contains(version, "wsl")
}

// isContainer checks if running inside a container.
func isContainer() bool {
	// Check for /.dockerenv
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}

	// Check cgroup for docker/lxc/containerd
	data, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	cgroup := string(data)
	return strings.Contains(cgroup, "docker") ||
		strings.Contains(cgroup, "lxc") ||
		strings.Contains(cgroup, "containerd") ||
		strings.Contains(cgroup, "kubepods")
}

// checkPamLimits checks if pam_limits.so is enabled.
func checkPamLimits() bool {
	pamFiles := []string{
		"/etc/pam.d/common-session",
		"/etc/pam.d/common-session-noninteractive",
		"/etc/pam.d/login",
		"/etc/pam.d/sshd",
	}

	for _, f := range pamFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "pam_limits.so") {
			return true
		}
	}
	return false
}

// parseLimitsConf reads relevant entries from /etc/security/limits.conf.
func parseLimitsConf() map[string]string {
	result := make(map[string]string)

	file, err := os.Open("/etc/security/limits.conf")
	if err != nil {
		return result
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[2] == "nofile" {
			key := fmt.Sprintf("%s %s nofile", fields[0], fields[1])
			result[key] = fields[3]
		}
	}

	return result
}

// getMacOSMaxFiles gets the current maxfiles limit on macOS.
func getMacOSMaxFiles() int {
	cmd := exec.Command("launchctl", "limit", "maxfiles")
	output, err := cmd.Output()
	if err != nil {
		return 0
	}

	// Parse: maxfiles    256            unlimited
	fields := strings.Fields(string(output))
	if len(fields) >= 2 {
		val, _ := strconv.Atoi(fields[1])
		return val
	}
	return 0
}

// countGtProcessFDs counts open file descriptors for gt-related processes.
func countGtProcessFDs() map[string]int {
	result := make(map[string]int)

	if runtime.GOOS != "linux" {
		return result
	}

	// Find gt-related processes
	procDir, err := os.Open("/proc")
	if err != nil {
		return result
	}
	defer procDir.Close()

	entries, err := procDir.Readdirnames(-1)
	if err != nil {
		return result
	}

	gtPatterns := regexp.MustCompile(`(?i)(gt|gastown|claude|node|deacon|daemon)`)

	for _, entry := range entries {
		pid, err := strconv.Atoi(entry)
		if err != nil {
			continue
		}

		// Read process command line
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry, "cmdline"))
		if err != nil {
			continue
		}

		cmd := strings.ReplaceAll(string(cmdline), "\x00", " ")
		if !gtPatterns.MatchString(cmd) {
			continue
		}

		// Count open FDs
		fdDir := filepath.Join("/proc", entry, "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}

		// Get process name
		procName := getProcessName(pid, cmd)
		result[procName] = len(fds)
	}

	return result
}

// getProcessName extracts a readable process name.
func getProcessName(pid int, cmdline string) string {
	// Try to get a short name from cmdline
	parts := strings.Fields(cmdline)
	if len(parts) > 0 {
		name := filepath.Base(parts[0])
		// Truncate long names
		if len(name) > 20 {
			name = name[:20]
		}
		return fmt.Sprintf("%s[%d]", name, pid)
	}
	return fmt.Sprintf("pid[%d]", pid)
}

// readProcInt reads an integer value from a /proc file.
func readProcInt(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}
