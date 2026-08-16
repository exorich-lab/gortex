package zcode

// remove.go is the counterpart to Apply for `gortex uninstall --global`.
//
// Every deletion is gated on evidence Gortex authored the thing being
// deleted, never on the path it sits at — ~/.zcode is a SHARED tree
// (the user's own skills, commands and settings live there beside
// ours). The config file is never deleted: we only ever added keys to
// it. Curated files are removed only while byte-identical to what we
// ship; a user's edit is kept with a warning, the same never-clobber
// posture writeCuratedFile takes on the way in.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

// RemoveGlobal strips the user-level ZCode footprint `gortex install`
// wrote: the mcp.servers.gortex entry and lifecycle hooks from
// ~/.zcode/cli/config.json, the rule block from ~/.zcode/AGENTS.md,
// and the curated skill / command pack. Returns the number of
// artifacts removed-or-cleaned and any per-artifact failures — a
// partial clean still reports rather than aborting.
func (a *Adapter) RemoveGlobal(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	if env.Home == "" {
		return 0, []string{"zcode: global cleanup requires a resolved home directory"}
	}
	w := env.Stderr

	// 1. ~/.zcode/cli/config.json — the MCP entry and the hooks.
	configAction, err := agents.MergeJSON(w, globalConfigPath(env.Home), func(root map[string]any, _ bool) (bool, error) {
		changed := removeMCPServer(root)
		if removeHooks(root) {
			changed = true
		}
		return changed, nil
	}, opts)
	if err != nil {
		failures = append(failures, fmt.Sprintf("zcode: %s: %v", globalConfigPath(env.Home), err))
	} else if configAction.Action != agents.ActionSkip {
		removed++
	}

	// 2. ~/.zcode/AGENTS.md — an empty body makes UpsertMarkedBlock
	// drop the marker-fenced block in place, preserving surrounding
	// user prose.
	insAction, err := agents.UpsertMarkedBlock(w, GlobalInstructionsPath(env.Home), "",
		agents.GlobalRulesStartMarker, agents.GlobalRulesEndMarker, opts)
	if err != nil {
		failures = append(failures, fmt.Sprintf("zcode: %s: %v", GlobalInstructionsPath(env.Home), err))
	} else if insAction.Action != agents.ActionSkip {
		removed++
	}

	// 3. ~/.zcode/{skills,commands} — the curated pack.
	packRemoved, packFailures := removeCuratedPack(env, opts)
	removed += packRemoved
	failures = append(failures, packFailures...)

	return removed, failures
}

// globalConfigPath is the user-level config file (see configPath).
func globalConfigPath(home string) string {
	return filepath.Join(home, ".zcode", "cli", "config.json")
}

// removeMCPServer strips mcp.servers.gortex, pruning the parents our
// entry was the last occupant of.
func removeMCPServer(root map[string]any) bool {
	mcp, ok := root["mcp"].(map[string]any)
	if !ok {
		return false
	}
	servers, ok := mcp["servers"].(map[string]any)
	if !ok {
		return false
	}
	if _, exists := servers["gortex"]; !exists {
		return false
	}
	delete(servers, "gortex")
	if len(servers) == 0 {
		delete(mcp, "servers")
	} else {
		mcp["servers"] = servers
	}
	if len(mcp) == 0 {
		delete(root, "mcp")
	} else {
		root["mcp"] = mcp
	}
	return true
}

// removeHooks strips every gortex-authored entry from hooks.events.
// User-added hook entries survive; the events map and the hooks key
// are pruned when our entries were their last content.
func removeHooks(root map[string]any) bool {
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return false
	}
	events, ok := hooks["events"].(map[string]any)
	if !ok {
		return false
	}
	changed := false
	for event, raw := range events {
		entries, ok := raw.([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(entries))
		for _, entry := range entries {
			if entryInvokesGortexHook(entry) {
				changed = true
				continue
			}
			kept = append(kept, entry)
		}
		if len(kept) == 0 {
			delete(events, event)
		} else {
			events[event] = kept
		}
	}
	if len(events) == 0 {
		delete(hooks, "events")
	} else {
		hooks["events"] = events
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooks
	}
	return changed
}

// GlobalArtifacts lists the user-level ZCode paths that currently carry
// a Gortex footprint, sorted. It applies the SAME ownership tests
// RemoveGlobal does, so the uninstall wizard's preview can never
// promise a deletion that will not happen.
func GlobalArtifacts(home string) []string {
	if home == "" {
		return nil
	}
	var present []string

	if path := globalConfigPath(home); hasGortexFootprint(path) {
		present = append(present, path)
	}
	if ins := GlobalInstructionsPath(home); fileContains(ins, agents.GlobalRulesStartMarker) {
		present = append(present, ins)
	}
	for path, shipped := range ownedPackFiles(home) {
		if data, err := os.ReadFile(path); err == nil && string(data) == shipped {
			present = append(present, path)
		}
	}

	sort.Strings(present)
	return present
}

// hasGortexFootprint reports whether the config at path carries our
// MCP entry or any of our hook entries. It parses rather than
// substring-matching: "gortex" appears in a user's config for plenty
// of innocent reasons. An unreadable or malformed config reads as
// "nothing of ours" — the safe direction, removal will skip it too.
func hasGortexFootprint(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal(agents.StripJSONComments(data), &root); err != nil {
		return false
	}
	if mcp, ok := root["mcp"].(map[string]any); ok {
		if servers, ok := mcp["servers"].(map[string]any); ok {
			if _, exists := servers["gortex"]; exists {
				return true
			}
		}
	}
	if hooks, ok := root["hooks"].(map[string]any); ok {
		if events, ok := hooks["events"].(map[string]any); ok {
			for _, raw := range events {
				entries, ok := raw.([]any)
				if !ok {
					continue
				}
				for _, entry := range entries {
					if entryInvokesGortexHook(entry) {
						return true
					}
				}
			}
		}
	}
	return false
}

// ownedPackFiles maps every curated skill / command path to the body
// Gortex ships there. One map so the remover and the preview enumerate
// exactly the same set.
func ownedPackFiles(home string) map[string]string {
	out := make(map[string]string)
	if skills, err := Skills(); err == nil {
		root := globalSkillsDir(home)
		for _, s := range skills {
			out[filepath.Join(root, s.ID, skillFileName)] = renderSkill(s)
		}
	}
	if commands, err := Commands(); err == nil {
		root := globalCommandsDir(home)
		for _, c := range commands {
			out[filepath.Join(root, c.ID+commandFileExt)] = renderCommand(c)
		}
	}
	return out
}

// removeCuratedPack deletes the shipped skills and commands whose
// bytes are still ours, pruning each skill's directory and then the
// roots. A user's file in the same root is never looked at, and the
// roots survive as long as it does.
func removeCuratedPack(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	owned := ownedPackFiles(env.Home)
	paths := make([]string, 0, len(owned))
	for path := range owned {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		existing, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		if string(existing) != owned[path] {
			internalutil.Warnf(env.Stderr, "keeping customised %s", path)
			continue
		}
		if opts.DryRun {
			removed++
			continue
		}
		if err := os.Remove(path); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		internalutil.Logf(env.Stderr, "[gortex uninstall] removed %s", path)
		removed++
		if filepath.Base(path) == skillFileName {
			pruneEmptyDir(filepath.Dir(path))
		}
	}
	if removed > 0 && !opts.DryRun {
		pruneEmptyDir(globalSkillsDir(env.Home))
		pruneEmptyDir(globalCommandsDir(env.Home))
	}
	return removed, failures
}

// pruneEmptyDir removes dir only when it holds nothing. os.Remove
// refuses a non-empty directory, which is exactly the guard we want.
func pruneEmptyDir(dir string) {
	_ = os.Remove(dir)
}

func fileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), needle)
}
