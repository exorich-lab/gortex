package zcode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// readServers pulls the nested mcp.servers map out of a config file.
func readServers(t *testing.T, path string) map[string]any {
	t.Helper()
	cfg := agentstest.ReadJSON(t, path)
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("missing mcp section in %s: %v", path, cfg)
	}
	servers, ok := mcp["servers"].(map[string]any)
	if !ok {
		t.Fatalf("missing mcp.servers in %s: %v", path, cfg)
	}
	return servers
}

func detectEnv(t *testing.T, env agents.Env) agents.Env {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(env.Home, ".zcode"), 0o755); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestApplyProjectWritesWorkspaceConfig(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env = detectEnv(t, env)

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true")
	}

	path := filepath.Join(env.Root, ".zcode", "config.json")
	servers := readServers(t, path)
	entry, ok := servers["gortex"].(map[string]any)
	if !ok {
		t.Fatalf("gortex missing from mcp.servers: %v", servers)
	}
	if cmd, _ := entry["command"].(string); cmd != "gortex" {
		t.Fatalf("command = %q, want gortex", cmd)
	}

	// Hooks: enabled flag plus the two maintained events, all speaking
	// the --agent=zcode protocol.
	cfg := agentstest.ReadJSON(t, path)
	hooks, ok := cfg["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("missing hooks: %v", cfg)
	}
	if enabled, _ := hooks["enabled"].(bool); !enabled {
		t.Fatal("hooks.enabled must be true or ZCode skips every config hook")
	}
	events, ok := hooks["events"].(map[string]any)
	if !ok {
		t.Fatalf("missing hooks.events: %v", hooks)
	}
	for _, event := range []string{"SessionStart", "PostToolUse"} {
		list, ok := events[event].([]any)
		if !ok || len(list) != 1 {
			t.Fatalf("hooks.events.%s = %v, want one entry", event, events[event])
		}
		group := list[0].(map[string]any)
		handler := group["hooks"].([]any)[0].(map[string]any)
		cmd, _ := handler["command"].(string)
		if !strings.Contains(cmd, "hook --agent=zcode") {
			t.Fatalf("%s hook command = %q, want gortex hook --agent=zcode", event, cmd)
		}
	}

	// Community routing lands in the repo-root AGENTS.md.
	agentsMd, err := os.ReadFile(filepath.Join(env.Root, "AGENTS.md"))
	if err != nil || !strings.Contains(string(agentsMd), agents.CommunitiesStartMarker) {
		t.Fatalf("AGENTS.md communities block missing (err: %v)", err)
	}

	// Generated community skills materialise under .zcode/skills.
	skillPath := filepath.Join(env.Root, ".zcode", "skills", "gortex-stub", "SKILL.md")
	if _, err := os.Stat(skillPath); err != nil {
		t.Fatalf("generated skill missing: %v", err)
	}
}

func TestApplyPreservesForeignConfigKeys(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env = detectEnv(t, env)
	path := filepath.Join(env.Root, ".zcode", "config.json")
	agentstest.WriteJSON(t, path, map[string]any{
		"plugins": map[string]any{"enabledPlugins": map[string]any{"ponytail@ponytail": true}},
		"mcp":     map[string]any{"servers": map[string]any{"other": map[string]any{"command": "npx"}}},
		"hooks": map[string]any{
			"enabled": true,
			"events": map[string]any{
				"Stop": []any{map[string]any{"hooks": []any{map[string]any{
					"type": "command", "command": "echo user-hook",
				}}}},
			},
		},
	})

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	cfg := agentstest.ReadJSON(t, path)
	if plugins, ok := cfg["plugins"].(map[string]any); !ok || plugins["enabledPlugins"] == nil {
		t.Fatalf("user plugins key clobbered: %v", cfg)
	}
	servers := readServers(t, path)
	if _, ok := servers["other"]; !ok {
		t.Fatal("foreign MCP server was removed")
	}
	// The user's Stop hook survives our PostToolUse addition.
	events := cfg["hooks"].(map[string]any)["events"].(map[string]any)
	stopList := events["Stop"].([]any)
	if len(stopList) != 1 || !strings.Contains(stopList[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"].(string), "user-hook") {
		t.Fatalf("user Stop hook clobbered: %v", stopList)
	}
}

func TestApplyIdempotent(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env = detectEnv(t, env)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	agentstest.AssertIdempotent(t, New(), env)
}

func TestApplySkipsUndetected(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	// No ~/.zcode and (hermetically) no zcode on PATH inside the test
	// sandbox home; Detect must return false and Apply must no-op.
	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Detected {
		t.Fatal("detected without ~/.zcode or zcode on PATH")
	}
	if res.Configured {
		t.Fatal("configured despite not being detected")
	}
	if _, err := os.Stat(filepath.Join(env.Root, ".zcode")); !os.IsNotExist(err) {
		t.Fatal("wrote workspace config despite not being detected")
	}
}

func TestApplyGlobalWritesUserSurfaces(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env = detectEnv(t, env)
	env.Mode = agents.ModeGlobal
	env.InstallGlobalInstructions = true

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true")
	}

	// MCP entry in the user config, not the workspace one.
	servers := readServers(t, globalConfigPath(env.Home))
	if _, ok := servers["gortex"]; !ok {
		t.Fatalf("gortex missing from user config: %v", servers)
	}
	if _, err := os.Stat(filepath.Join(env.Root, ".zcode")); !os.IsNotExist(err) {
		t.Fatal("global mode must not write the workspace config")
	}

	// Rule block in ~/.zcode/AGENTS.md, marker-fenced so a re-install
	// refreshes it in place.
	ins, err := os.ReadFile(GlobalInstructionsPath(env.Home))
	if err != nil || !strings.Contains(string(ins), agents.GlobalRulesStartMarker) {
		t.Fatalf("user AGENTS.md rule block missing (err: %v)", err)
	}

	// Curated pack under ~/.zcode/{skills,commands}.
	if _, err := os.Stat(globalSkillsDir(env.Home)); err != nil {
		t.Fatalf("curated skills missing: %v", err)
	}
	if _, err := os.Stat(globalCommandsDir(env.Home)); err != nil {
		t.Fatalf("curated commands missing: %v", err)
	}
}

func TestApplyNoHooksSkipsHookInstall(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env = detectEnv(t, env)
	env.InstallHooks = false

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cfg := agentstest.ReadJSON(t, filepath.Join(env.Root, ".zcode", "config.json"))
	if _, exists := cfg["hooks"]; exists {
		t.Fatalf("hooks written despite InstallHooks=false: %v", cfg["hooks"])
	}
}

func TestUpsertMCPServerNeverClobbersCustomEntry(t *testing.T) {
	root := map[string]any{
		"mcp": map[string]any{"servers": map[string]any{
			"gortex": map[string]any{"command": "my-wrapped-gortex", "args": []any{"mcp", "--flag"}},
		}},
	}
	if upsertMCPServer(root, agents.ApplyOpts{}) {
		t.Fatal("rewrote a user-customized gortex entry")
	}
	if !upsertMCPServer(root, agents.ApplyOpts{Force: true}) {
		t.Fatal("Force must rewrite the entry")
	}
}

func TestRemoveGlobalRoundTrip(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env = detectEnv(t, env)
	env.Mode = agents.ModeGlobal
	env.InstallGlobalInstructions = true

	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Seed a foreign server + hook alongside ours, then remove.
	path := globalConfigPath(env.Home)
	agentstest.WriteJSON(t, path, func() map[string]any {
		cfg := agentstest.ReadJSON(t, path)
		mcp := cfg["mcp"].(map[string]any)
		mcp["servers"].(map[string]any)["other"] = map[string]any{"command": "npx"}
		events := cfg["hooks"].(map[string]any)["events"].(map[string]any)
		events["Stop"] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo user"}}}}
		return cfg
	}())

	if got := len(GlobalArtifacts(env.Home)); got == 0 {
		t.Fatal("GlobalArtifacts empty after install")
	}
	removed, failures := a.RemoveGlobal(env, agents.ApplyOpts{})
	if len(failures) > 0 {
		t.Fatalf("failures: %v", failures)
	}
	if removed == 0 {
		t.Fatal("removed nothing")
	}

	cfg := agentstest.ReadJSON(t, path)
	servers := cfg["mcp"].(map[string]any)["servers"].(map[string]any)
	if _, ok := servers["gortex"]; ok {
		t.Fatal("gortex server survived RemoveGlobal")
	}
	if _, ok := servers["other"]; !ok {
		t.Fatal("foreign server removed by RemoveGlobal")
	}
	events := cfg["hooks"].(map[string]any)["events"].(map[string]any)
	if _, ok := events["PostToolUse"]; ok {
		t.Fatal("gortex PostToolUse hook survived RemoveGlobal")
	}
	if _, ok := events["Stop"]; !ok {
		t.Fatal("user Stop hook removed by RemoveGlobal")
	}
	if ins, err := os.ReadFile(GlobalInstructionsPath(env.Home)); err != nil || strings.Contains(string(ins), agents.GlobalRulesStartMarker) {
		t.Fatal("rule block survived RemoveGlobal")
	}
	if got := GlobalArtifacts(env.Home); len(got) != 0 {
		t.Fatalf("GlobalArtifacts after removal: %v", got)
	}
}
