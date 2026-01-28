# Loopflow DAG Architecture Investigation

**Bead:** gs-u3xj
**Date:** 2026-01-27
**Author:** gastown/polecats/slit

## Executive Summary

This investigation compares Gas Town's emergent coordination model with Loopflow's declarative DAG-based approach. The key insight: Gas Town's O(n) communication patterns become bottlenecks at scale, while Loopflow's independent agent execution minimizes inter-agent coupling.

**Finding:** Gas Town has no true O(n^2) patterns, but several O(n) patterns with timeouts create practical scaling limits.

## Key Architectural Differences

| Aspect | Gas Town | Loopflow |
|--------|----------|----------|
| Coordination model | Emergent (mail, nudges, patrols) | Declarative (DAG, stimulus) |
| Agent coupling | High (watch each other) | None (independent execution) |
| Scaling | O(n) with timeouts | O(n) parallel execution |
| Failure handling | Watchers check watchers | Circuit breaker per agent |
| Work definition | Beads + molecules + mail | Flow DAG + goals |
| Triggers | Polling loops | External stimulus (cron, watch) |

## Current Bottlenecks Identified

### 1. Agent Validation on Every Send (O(n) scan)
**Location:** `internal/mail/router.go:627-648`

```go
// validateRecipient queries ALL agents on every single-recipient send
func (r *Router) validateRecipient(identity string) error {
    agents, err := r.queryAgents("")  // Full scan
    for _, agent := range agents {
        if agentBeadToAddress(agent) == identity {
            return nil
        }
    }
    return fmt.Errorf("no agent found")
}
```

**Impact:** With 50 polecats + town agents, every mail send scans 60+ agents.

### 2. Sequential Health Checks (O(n) x timeout)
**Location:** `internal/deacon/stuck.go:185-215`

Health checks are serial with 30-second timeouts. For 10 agents:
- Best case: 10 agents x ~100ms response = 1 second
- Worst case: 10 agents x 30s timeout = 5+ minutes

### 3. tmux Pool Reconciliation (O(n) system calls)
**Location:** `internal/polecat/manager.go:767-833`

```go
// ReconcilePool runs tmux has-session for each pooled name
for _, name := range m.pool.Names {
    hasSession, _ := tmux.HasSession(name)  // Shell invocation
    // ...
}
```

**Impact:** 50-name pool = 50 tmux commands on every allocation.

### 4. Staleness Detection (O(n) x 4 ops)
**Location:** `internal/polecat/manager.go:1109-1159`

Each polecat check requires:
1. tmux session check
2. git status
3. git rev-list
4. beads query

**Impact:** 20 polecats = 80+ system calls.

### 5. Merge Queue Rescanning (O(n) per poll)
**Location:** `internal/refinery/manager.go:228-273`

Queue is rescanned and resorted on every poll cycle. No caching.

---

## Proposed PRs

### PR 1: Agent Registry Cache with TTL-based Invalidation

**Problem:** Every mail send queries the full agent list via `bd list --type=agent`.

**Solution:** Implement an in-memory agent registry with:
- TTL-based cache (30 seconds default)
- Event-driven invalidation on agent birth/death
- Single-lookup validation instead of full scan

**Files to modify:**
- `internal/mail/router.go` - Add cache field, modify `validateRecipient`
- `internal/mail/registry.go` (new) - Agent registry implementation

**Implementation sketch:**
```go
type AgentRegistry struct {
    mu      sync.RWMutex
    agents  map[string]*AgentInfo  // identity -> info
    lastRefresh time.Time
    ttl     time.Duration
}

func (r *AgentRegistry) Exists(identity string) bool {
    r.mu.RLock()
    defer r.mu.RUnlock()
    _, exists := r.agents[identity]
    return exists
}

func (r *AgentRegistry) RefreshIfStale() error {
    if time.Since(r.lastRefresh) < r.ttl {
        return nil
    }
    // ... refresh from beads
}
```

**Priority:** P2 - High impact, low risk

---

### PR 2: Parallel Health Checks with Bounded Concurrency

**Problem:** Serial health checks take O(n) x timeout.

**Solution:** Parallel health checks with:
- Worker pool (max 5 concurrent)
- Shorter timeout (10s) with exponential backoff
- Circuit breaker pattern per agent

**Files to modify:**
- `internal/deacon/stuck.go` - Parallel health check implementation
- `internal/deacon/circuitbreaker.go` (new) - Per-agent circuit breaker

**Implementation sketch:**
```go
func (d *Deacon) CheckHealthParallel(agents []string) []HealthResult {
    results := make(chan HealthResult, len(agents))
    sem := make(chan struct{}, 5)  // Max 5 concurrent

    for _, agent := range agents {
        go func(a string) {
            sem <- struct{}{}
            defer func() { <-sem }()
            results <- d.checkSingleHealth(a)
        }(agent)
    }
    // ... collect results
}
```

**Priority:** P2 - Reduces health check time from minutes to seconds

---

### PR 3: Stimulus-Based Patrol Triggers

**Problem:** Patrols run on timer loops, consuming agent cycles even when nothing changed.

**Solution:** Replace timer-based patrols with stimulus triggers:
- File watch triggers for sandbox changes
- Beads event triggers for state changes
- Cron-style triggers for periodic maintenance

**Files to modify:**
- `internal/deacon/manager.go` - Stimulus-based patrol
- `internal/witness/manager.go` - Stimulus-based patrol
- `internal/stimulus/` (new package) - Trigger infrastructure

**Implementation sketch:**
```go
type StimulusConfig struct {
    FileWatch []string  // Paths to watch
    Cron      string    // Cron expression
    Events    []string  // Beads event types
}

func (s *Stimulus) Start(config StimulusConfig, handler func()) {
    // File watcher
    if len(config.FileWatch) > 0 {
        s.startFileWatcher(config.FileWatch, handler)
    }
    // Cron schedule
    if config.Cron != "" {
        s.startCron(config.Cron, handler)
    }
}
```

**Priority:** P3 - Reduces CPU overhead, more responsive to changes

---

### PR 4: Lazy Pool Reconciliation

**Problem:** Full pool reconciliation runs on every name allocation.

**Solution:**
- Reconcile only on allocation failure (name collision)
- Background reconciliation on configurable interval
- Cache tmux session state with short TTL

**Files to modify:**
- `internal/polecat/manager.go` - Lazy reconciliation
- `internal/polecat/pool.go` - Session state cache

**Implementation sketch:**
```go
func (m *Manager) AllocateName() (string, error) {
    name, err := m.pool.TryAllocate()
    if err == ErrNameCollision {
        // Only reconcile on failure
        m.ReconcilePool()
        return m.pool.TryAllocate()
    }
    return name, err
}
```

**Priority:** P3 - Reduces 50 tmux calls to ~1 on average

---

### PR 5: Cached Merge Queue with Score Memoization

**Problem:** Merge queue rescans and rescores on every poll.

**Solution:**
- Cache queue with event-driven invalidation
- Memoize scores (only recalculate on priority/retry changes)
- Incremental updates instead of full rescan

**Files to modify:**
- `internal/refinery/manager.go` - Queue caching
- `internal/refinery/queue.go` (new) - Cached queue implementation

**Priority:** P3 - Reduces database load under high MR volume

---

### PR 6: Batch Staleness Detection

**Problem:** Staleness detection is serial with 4 ops per polecat.

**Solution:**
- Batch git operations (single `git status --porcelain` for all)
- Parallel tmux checks with bounded concurrency
- Single beads query with filter instead of N queries

**Files to modify:**
- `internal/polecat/manager.go` - Batch operations
- `internal/polecat/staleness.go` (new) - Batch staleness checker

**Implementation sketch:**
```go
func (m *Manager) DetectStalePolecatsBatch() ([]StalePolecat, error) {
    // Single beads query for all polecat agents
    agents, _ := m.bd.ListByType("agent", "--filter", "role:polecat")

    // Parallel tmux checks
    sessions := m.checkSessionsBatch(polecatNames)

    // Single git status for all (from root)
    statuses := m.gitStatusBatch(polecatDirs)

    // Combine results
    // ...
}
```

**Priority:** P2 - Reduces 80+ calls to ~5 for 20 polecats

---

## Implementation Roadmap

### Phase 1: Quick Wins (Low Risk, High Impact)
1. **PR 1: Agent Registry Cache** - Eliminates O(n) scan per send
2. **PR 2: Parallel Health Checks** - 10x faster health monitoring

### Phase 2: Efficiency Improvements
3. **PR 4: Lazy Pool Reconciliation** - Reduces allocation overhead
4. **PR 6: Batch Staleness Detection** - Faster witness patrol

### Phase 3: Architecture Evolution
5. **PR 3: Stimulus-Based Triggers** - Move from polling to events
6. **PR 5: Cached Merge Queue** - Refinery efficiency

---

## Loopflow Principles Applied

The core Loopflow principles that inform these proposals:

1. **Independence over Coordination**: Each PR reduces inter-agent coupling
2. **Declarative over Emergent**: Stimulus triggers replace polling loops
3. **Caching over Querying**: Reduce database/system call overhead
4. **Parallel over Serial**: Bounded concurrency for O(n) operations
5. **Circuit Breakers**: Fail fast, recover gracefully

These changes are incremental - no full architecture rewrite required.

---

## References

- [Loopflow GitHub](https://github.com/loop-flow/loopflow) - Minimal public content
- [Dagster](https://dagster.io) - DAG-based orchestration
- [Prefect](https://www.prefect.io) - Workflow orchestration with agents
- Gas Town ZFC documents (internal)

---

## Next Steps

1. Create individual beads for each PR proposal
2. Prioritize based on witness/deacon pain points
3. Start with PR 1 (Agent Registry Cache) as proof of concept
