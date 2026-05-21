// Package config reads splice's user-facing settings from
// <cwd>/.splice/config.json (project-local) or, if missing, from
// $HOME/.splice/config.json (global). Defaults apply when both are absent.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the structured form of the JSON file. Always treat zero values
// as "not configured" and fall back to defaults via Resolved().
type Config struct {
	// AskOnIntercept controls whether intercepts surface as a Claude Code
	// permission prompt (true) or a hard deny with cached injection (false).
	// Pointer so we can distinguish "absent" from "explicitly false".
	AskOnIntercept *bool `json:"ask_on_intercept,omitempty"`

	// SnapshotEvictionAfter is the number of consecutive PreToolUse events
	// after a compaction that may pass without producing a cache hit before
	// splice drops the snapshot entirely. A cache hit resets the counter.
	// Set to 0 to disable eviction (snapshot survives until the next freeze
	// or session deletion). Pointer to distinguish absent-from-explicit-zero.
	SnapshotEvictionAfter *int `json:"snapshot_eviction_after,omitempty"`

	// NeverCacheBashPatterns are user-provided Bash command patterns whose
	// results should always be re-queried after compaction. This is for live
	// status checks such as simulator logs, queues, and process probes.
	NeverCacheBashPatterns *[]string `json:"never_cache_bash_patterns,omitempty"`
}

// Resolved holds the post-default decision values.
type Resolved struct {
	AskOnIntercept         bool
	SnapshotEvictionAfter  int
	NeverCacheBashPatterns []string
}

// Load reads project-local first, then user-global, then applies defaults.
// Errors only on malformed JSON — missing files are silently treated as empty.
func Load(cwd string) (Resolved, error) {
	merged := Config{}

	if home, err := os.UserHomeDir(); err == nil {
		if c, err := readIfExists(filepath.Join(home, ".splice", "config.json")); err != nil {
			return Resolved{}, err
		} else if c != nil {
			merged = mergeOver(merged, *c)
		}
	}

	if cwd != "" {
		if c, err := readIfExists(filepath.Join(cwd, ".splice", "config.json")); err != nil {
			return Resolved{}, err
		} else if c != nil {
			merged = mergeOver(merged, *c)
		}
	}

	return resolved(merged), nil
}

func readIfExists(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	if len(raw) == 0 {
		return &Config{}, nil
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return &c, nil
}

func mergeOver(base, top Config) Config {
	if top.AskOnIntercept != nil {
		base.AskOnIntercept = top.AskOnIntercept
	}
	if top.SnapshotEvictionAfter != nil {
		base.SnapshotEvictionAfter = top.SnapshotEvictionAfter
	}
	if top.NeverCacheBashPatterns != nil {
		base.NeverCacheBashPatterns = top.NeverCacheBashPatterns
	}
	return base
}

// DefaultSnapshotEvictionAfter is the threshold splice uses when the
// user hasn't configured one. Tuned roughly to "a normal user takes
// 20-ish tool calls to wander away from a topic" — small enough to
// drop stale snapshots quickly, large enough that legitimate post-
// compact re-runs (which usually happen within the first few tool
// calls) still get the cache benefit.
const DefaultSnapshotEvictionAfter = 20

func resolved(c Config) Resolved {
	r := Resolved{
		AskOnIntercept:        true, // default
		SnapshotEvictionAfter: DefaultSnapshotEvictionAfter,
	}
	if c.AskOnIntercept != nil {
		r.AskOnIntercept = *c.AskOnIntercept
	}
	if c.SnapshotEvictionAfter != nil {
		r.SnapshotEvictionAfter = *c.SnapshotEvictionAfter
	}
	if c.NeverCacheBashPatterns != nil {
		r.NeverCacheBashPatterns = append([]string(nil), (*c.NeverCacheBashPatterns)...)
	}
	return r
}
