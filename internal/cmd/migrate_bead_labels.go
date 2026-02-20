package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

var migrateBeadLabelsDryRun bool

var migrateBeadLabelsCmd = &cobra.Command{
	Use:     "migrate-bead-labels",
	Short:   "Add gt:* labels to beads created before label-based types",
	GroupID: GroupWorkspace,
	Long: `Migrate existing beads to use gt:* labels.

Gas Town migrated from dedicated bead types (agent, role, rig, convoy, slot)
to label-based types (gt:agent, gt:role, etc.). This command adds the new
labels to beads that were created before the migration.

The migration iterates all databases (town + per-rig) and for each GT custom
type, finds beads missing the corresponding gt:* label and adds it.

Examples:
  gt migrate-bead-labels            # Run the migration
  gt migrate-bead-labels --dry-run  # Preview what would be migrated`,
	RunE: runMigrateBeadLabels,
}

func init() {
	rootCmd.AddCommand(migrateBeadLabelsCmd)
	migrateBeadLabelsCmd.Flags().BoolVar(&migrateBeadLabelsDryRun, "dry-run", false, "Preview what would be migrated without making changes")
}

// gtTypesToMigrate lists the original GT types that need label migration.
// Later types (queue, event, message, etc.) already use labels at creation time.
var gtTypesToMigrate = []string{"agent", "role", "rig", "convoy", "slot"}

// migrateBeadLabelListItem is the minimal structure needed from bd list --json output.
type migrateBeadLabelListItem struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Labels []string `json:"labels,omitempty"`
}

// dbMigrationStats tracks per-database migration statistics.
type dbMigrationStats struct {
	name    string
	updated int
	skipped int
	failed  int
}

func runMigrateBeadLabels(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}

	// Load routes to discover all beads databases
	townBeadsDir := beads.GetTownBeadsPath(townRoot)
	routes, err := beads.LoadRoutes(townBeadsDir)
	if err != nil {
		return fmt.Errorf("loading routes: %w", err)
	}

	// Build list of databases to process: town + each rig
	type dbTarget struct {
		name     string // display name
		beadsDir string // path to .beads directory
	}
	var targets []dbTarget

	// Town-level beads
	targets = append(targets, dbTarget{
		name:     "town",
		beadsDir: townBeadsDir,
	})

	// Per-rig beads from routes
	for _, route := range routes {
		if route.Path == "." {
			continue // Already handled as town
		}
		rigBeadsDir := filepath.Join(townRoot, route.Path, ".beads")
		if _, err := os.Stat(rigBeadsDir); os.IsNotExist(err) {
			continue // Skip if rig beads dir doesn't exist
		}
		targets = append(targets, dbTarget{
			name:     route.Path,
			beadsDir: rigBeadsDir,
		})
	}

	fmt.Printf("%s Migrating bead labels across %d database(s)\n\n",
		style.Bold.Render("Labels Migration"), len(targets))

	var allStats []dbMigrationStats

	for _, target := range targets {
		stats := migrateDatabase(target.name, target.beadsDir, migrateBeadLabelsDryRun)
		allStats = append(allStats, stats)
	}

	// Print summary
	fmt.Printf("\n%s\n", style.Bold.Render("Summary:"))
	totalUpdated, totalSkipped, totalFailed := 0, 0, 0
	for _, s := range allStats {
		totalUpdated += s.updated
		totalSkipped += s.skipped
		totalFailed += s.failed
		if s.updated > 0 || s.failed > 0 {
			fmt.Printf("  %s: %d updated, %d skipped, %d failed\n",
				s.name, s.updated, s.skipped, s.failed)
		} else {
			fmt.Printf("  %s: %s\n", s.name,
				style.Dim.Render(fmt.Sprintf("%d skipped (already labeled)", s.skipped)))
		}
	}

	if migrateBeadLabelsDryRun {
		fmt.Printf("\n%s Would update %d bead(s), skip %d\n",
			style.Bold.Render("[DRY RUN]"), totalUpdated, totalSkipped)
	} else if totalFailed > 0 {
		fmt.Printf("\n%s Updated %d, skipped %d, failed %d\n",
			style.Bold.Render("Done:"), totalUpdated, totalSkipped, totalFailed)
	} else {
		fmt.Printf("\n%s Migrated %d bead(s), skipped %d (already labeled)\n",
			style.Success.Render("✓"), totalUpdated, totalSkipped)
	}

	return nil
}

// migrateDatabase processes all GT types for a single beads database.
func migrateDatabase(name, beadsDir string, dryRun bool) dbMigrationStats {
	stats := dbMigrationStats{name: name}

	for _, typeName := range gtTypesToMigrate {
		label := "gt:" + typeName
		updated, skipped, failed := migrateType(name, beadsDir, typeName, label, dryRun)
		stats.updated += updated
		stats.skipped += skipped
		stats.failed += failed
	}

	return stats
}

// migrateType adds gt:<type> labels to beads of the given type that are missing them.
func migrateType(dbName, beadsDir, typeName, label string, dryRun bool) (updated, skipped, failed int) {
	// Run bd list --type=<type> directly (bypassing Go wrapper which converts --type to --label)
	listArgs := []string{
		"list",
		"--type=" + typeName,
		"--status=all",
		"--limit=0",
		"--json",
	}

	listCmd := exec.Command("bd", listArgs...) //nolint:gosec // G204: bd is a trusted internal tool
	listCmd.Env = append(os.Environ(), "BEADS_DIR="+beadsDir)
	listOutput, err := listCmd.Output()
	if err != nil {
		// No beads of this type or bd command failed — not an error
		return 0, 0, 0
	}

	var items []migrateBeadLabelListItem
	if err := json.Unmarshal(listOutput, &items); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: %s: parsing %s list: %v\n", dbName, typeName, err)
		return 0, 0, 0
	}

	if len(items) == 0 {
		return 0, 0, 0
	}

	for _, item := range items {
		// Check if bead already has the label
		issue := &beads.Issue{Labels: item.Labels}
		if beads.HasLabel(issue, label) {
			skipped++
			continue
		}

		if dryRun {
			fmt.Printf("  %s %s (%s) — would add label %s\n",
				style.Dim.Render("[DRY RUN]"), item.ID, item.Title, label)
			updated++
			continue
		}

		// Add the label
		updateCmd := exec.Command("bd", "update", item.ID, "--add-label="+label) //nolint:gosec // G204
		updateCmd.Env = append(os.Environ(), "BEADS_DIR="+beadsDir)
		if err := updateCmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: %s: failed to add %s to %s: %v\n",
				dbName, label, item.ID, err)
			failed++
			continue
		}

		fmt.Printf("  %s %s (%s) + %s\n",
			style.Success.Render("✓"), item.ID, item.Title, label)
		updated++
	}

	return updated, skipped, failed
}
