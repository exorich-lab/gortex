// Package zcode implements the Gortex init integration for the ZCode
// editor. ZCode reads its workspace config from <repo>/.zcode/config.json
// and its user config from ~/.zcode/cli/config.json; both nest MCP
// servers under a {"mcp": {"servers": {...}}} key rather than the flat
// mcpServers shape most clients use:
//
//	{
//	  "mcp": {
//	    "servers": {
//	      "gortex": {"command": "gortex", "args": ["mcp"]}
//	    }
//	  }
//	}
//
// Lifecycle hooks live under a "hooks" key shaped
// {"enabled": true, "events": {"<Event>": [{"hooks": [...]}]}} and speak
// the Claude Code wire format on stdin/stdout, so `gortex hook
// --agent=zcode` rides the shared external-agent dispatcher without a
// protocol branch of its own. Instructions come from AGENTS.md — the
// repo-root file per workspace, ~/.zcode/AGENTS.md machine-wide.
package zcode

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

const (
	Name    = "zcode"
	DocsURL = "https://github.com/exorich-lab/gortex/blob/main/docs/agents.md#zcode"
)

// zcodeHookTimeoutSeconds bounds each lifecycle hook. SessionStart
// builds a graph orientation and PostToolUse a stale-index hint; both
// are local daemon reads, but the daemon may be cold-starting on the
// first event of a session.
const zcodeHookTimeoutSeconds = 10

type Adapter struct{}

func New() *Adapter                { return &Adapter{} }
func (a *Adapter) Name() string    { return Name }
func (a *Adapter) DocsURL() string { return DocsURL }

// configPath returns the config file for the current Env. Project mode
// writes <repo>/.zcode/config.json (workspace scope, shareable through
// version control); global mode writes ~/.zcode/cli/config.json (user
// scope, applies to every workspace).
func configPath(env agents.Env) string {
	if env.Mode == agents.ModeGlobal && env.Home != "" {
		return filepath.Join(env.Home, ".zcode", "cli", "config.json")
	}
	return filepath.Join(env.Root, ".zcode", "config.json")
}

// Detect checks for a ~/.zcode home (the editor creates it on first
// run) or a zcode binary on PATH.
func (a *Adapter) Detect(env agents.Env) (bool, error) {
	if p, err := exec.LookPath("zcode"); err == nil && p != "" {
		return true, nil
	}
	if env.Home == "" {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(env.Home, ".zcode")); err == nil {
		return true, nil
	}
	return false, nil
}

func (a *Adapter) Plan(env agents.Env) (*agents.Plan, error) {
	keys := []string{"mcp"}
	if env.InstallHooks {
		keys = append(keys, "hooks")
	}
	p := &agents.Plan{Files: []agents.FileAction{
		{Path: configPath(env), Action: agents.ActionWouldMerge, Keys: keys},
	}}
	if env.Mode == agents.ModeGlobal && env.InstallGlobalInstructions && env.Home != "" {
		p.Files = append(p.Files, agents.FileAction{
			Path: GlobalInstructionsPath(env.Home), Action: agents.ActionWouldMerge,
			Keys: []string{"gortex-rules-block"},
		})
	}
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		p.Files = append(p.Files, agents.FileAction{
			Path: filepath.Join(env.Root, "AGENTS.md"), Action: agents.ActionWouldMerge,
			Keys: []string{"communities-block"},
		})
	}
	skillFiles, err := planSkills(env)
	if err != nil {
		return nil, err
	}
	p.Files = append(p.Files, skillFiles...)
	return p, nil
}

// GlobalInstructionsPath is ZCode's user-level instructions file — the
// ZCode analogue of ~/.claude/CLAUDE.md. ZCode injects it into every
// workspace session ahead of the repo's own AGENTS.md.
func GlobalInstructionsPath(home string) string {
	return filepath.Join(home, ".zcode", "AGENTS.md")
}

func (a *Adapter) Apply(env agents.Env, opts agents.ApplyOpts) (*agents.Result, error) {
	res := &agents.Result{Name: Name, DocsURL: DocsURL}
	detected, _ := a.Detect(env)
	res.Detected = detected
	if !detected && !opts.ForceDetect {
		internalutil.Logf(env.Stderr, "[gortex init] skip ZCode setup (zcode not detected)")
		return res, nil
	}
	internalutil.Logf(env.Stderr, "[gortex init] setting up ZCode integration...")

	action, err := agents.MergeJSON(env.Stderr, configPath(env), func(root map[string]any, _ bool) (bool, error) {
		changed := upsertMCPServer(root, opts)
		if env.InstallHooks {
			if upsertHooks(root, env, opts) {
				changed = true
			}
		}
		return changed, nil
	}, opts)
	if err != nil {
		return res, err
	}
	res.Files = append(res.Files, action)

	// User-level instructions → ~/.zcode/AGENTS.md. ZCode loads the
	// file verbatim (no @-include resolution), so the profile body is
	// inlined and refreshed in place, exactly like Codex's copy.
	if env.Mode == agents.ModeGlobal && env.InstallGlobalInstructions {
		insAction, err := agents.UpsertMarkedBlock(nil, GlobalInstructionsPath(env.Home),
			agents.GlobalInlineBody(agents.InstructionsDir(env)),
			agents.GlobalRulesStartMarker, agents.GlobalRulesEndMarker, opts)
		if err != nil {
			return res, fmt.Errorf("zcode global instructions: %w", err)
		}
		if insAction.Keys != nil {
			insAction.Keys = []string{"gortex-rules-block"}
		}
		if !opts.DryRun && insAction.Action != agents.ActionSkip {
			internalutil.Logf(env.Stderr, "[gortex install] wrote rule block to %s", GlobalInstructionsPath(env.Home))
		}
		res.Files = append(res.Files, insAction)
	}

	// Repo-local community routing → AGENTS.md at the repo root. The
	// same file Codex and OpenCode write their routing block into; the
	// marker-guarded upsert converges across adapters.
	if env.Mode != agents.ModeGlobal && env.SkillsRouting != "" {
		routingAction, err := agents.UpsertMarkedBlock(env.Stderr, filepath.Join(env.Root, "AGENTS.md"), env.SkillsRouting,
			agents.CommunitiesStartMarker, agents.CommunitiesEndMarker, opts)
		if err != nil {
			return res, err
		}
		res.Files = append(res.Files, routingAction)
	}

	skillActions, err := applySkills(env, opts)
	if err != nil {
		return res, fmt.Errorf("zcode skills: %w", err)
	}
	res.Files = append(res.Files, skillActions...)

	res.Configured = true
	return res, nil
}

// upsertMCPServer merges the gortex stanza into the nested
// mcp.servers map. Migration semantics mirror
// agents.UpsertMCPServerWithMigration: an entry we authored is
// rewritten to the current shape, a byte-identical entry is left
// alone, and a user-customized entry is never touched without Force.
func upsertMCPServer(root map[string]any, opts agents.ApplyOpts) bool {
	mcp, ok := root["mcp"].(map[string]any)
	if !ok {
		mcp = make(map[string]any)
	}
	servers, ok := mcp["servers"].(map[string]any)
	if !ok {
		servers = make(map[string]any)
	}
	entry := agents.DefaultGortexMCPEntry()
	if existing, exists := servers["gortex"]; exists {
		if agents.MCPEntriesEqual(existing, entry) {
			return false
		}
		if !opts.Force && !agents.IsGortexAuthoredMCPEntry(existing) {
			return false
		}
	}
	servers["gortex"] = entry
	mcp["servers"] = servers
	root["mcp"] = mcp
	return true
}

// hookCommand is the shell command baked into every ZCode lifecycle
// hook. --agent=zcode routes `gortex hook` to the external-agent
// dispatcher, whose Claude-format envelope is what ZCode parses.
func hookCommand(env agents.Env) string {
	base := strings.TrimSpace(env.HookCommand)
	if base == "" {
		base = "gortex hook"
	}
	return base + " --agent=zcode"
}

// hookEvents is the hook set this adapter maintains: a SessionStart
// orientation plus a PostToolUse stale-index hint. Matchers are
// omitted — every tool event reaches the hint builder, which stays
// silent unless something is actionable.
var hookEvents = []struct {
	event         string
	statusMessage string
}{
	{"SessionStart", "Loading Gortex graph orientation..."},
	{"PostToolUse", "Loading Gortex graph context..."},
}

func hookEntry(env agents.Env, statusMessage string) map[string]any {
	return map[string]any{
		"hooks": []any{map[string]any{
			"type":          "command",
			"command":       hookCommand(env),
			"timeout":       zcodeHookTimeoutSeconds,
			"statusMessage": statusMessage,
		}},
	}
}

// entryCommand returns the first handler command in a hooks.events
// entry, or "" when the entry carries none.
func entryCommand(entry any) string {
	group, ok := entry.(map[string]any)
	if !ok {
		return ""
	}
	handlers, ok := group["hooks"].([]any)
	if !ok {
		return ""
	}
	for _, h := range handlers {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := hm["command"].(string); strings.TrimSpace(cmd) != "" {
			return cmd
		}
	}
	return ""
}

// entryInvokesGortexHook reports whether a hooks.events entry was
// authored by this adapter: any handler command naming gortex and
// targeting the zcode agent.
func entryInvokesGortexHook(entry any) bool {
	cmd := strings.ToLower(entryCommand(entry))
	return strings.Contains(cmd, "gortex") &&
		(strings.Contains(cmd, "--agent=zcode") || strings.Contains(cmd, "--agent zcode"))
}

// upsertHooks maintains the hooks.events entries for every event in
// hookEvents and forces hooks.enabled=true — ZCode skips all
// configuration-file hooks until that flag is set. Stale
// gortex-authored entries are replaced rather than accumulated;
// entries the user added are preserved.
func upsertHooks(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		hooks = make(map[string]any)
	}
	events, ok := hooks["events"].(map[string]any)
	if !ok {
		events = make(map[string]any)
	}

	changed := false
	if enabled, _ := hooks["enabled"].(bool); !enabled {
		hooks["enabled"] = true
		changed = true
	}
	for _, he := range hookEvents {
		entries, _ := events[he.event].([]any)
		kept := make([]any, 0, len(entries)+1)
		found := false
		for _, entry := range entries {
			if !entryInvokesGortexHook(entry) {
				kept = append(kept, entry)
				continue
			}
			// A gortex-authored entry with the current command stays;
			// one with a stale command (an older install's hook shape)
			// is replaced rather than accumulated next to.
			if !opts.Force && entryCommand(entry) == hookCommand(env) {
				found = true
				kept = append(kept, entry)
				continue
			}
			changed = true
		}
		if !found {
			kept = append(kept, hookEntry(env, he.statusMessage))
			changed = true
		}
		events[he.event] = kept
	}
	hooks["events"] = events
	root["hooks"] = hooks
	return changed
}
