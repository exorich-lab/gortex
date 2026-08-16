package zcode

// skills.go installs the Gortex playbooks as ZCode skills and the
// Gortex slash-command pack as ZCode commands.
//
// # Roots
//
// ZCode scans ~/.zcode/skills and <repo>/.zcode/skills (plus the
// vendor-neutral ~/.agents/skills). This adapter writes its own copy
// under .zcode rather than leaning on the shared .agents root: that
// pack only exists when the codex adapter also ran, so relying on it
// would make a ZCode install silently depend on an unrelated agent
// being present — the same independence rule the opencode adapter
// follows for its own root.
//
// # Frontmatter
//
// Skills carry `name` + `description`. `name` is taken from the ID,
// not Skill.Name: the ID is also the directory the SKILL.md lands in,
// and deriving both from one value makes a name/dir mismatch — the
// failure mode that silently hides a skill — unrepresentable.
//
// # Commands
//
// A command file's base name is the command (`gortex-explore.md` →
// /gortex-explore); `description` is the only frontmatter key set.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/agents/internalutil"
	"github.com/zzet/gortex/internal/agents/skillpack"
)

// skillFileName is the file ZCode loads inside each skill directory.
const skillFileName = "SKILL.md"

// commandFileExt is the extension ZCode globs for commands. The file's
// base name *is* the command name, so nothing inside the file names it.
const commandFileExt = ".md"

func globalSkillsDir(home string) string   { return filepath.Join(home, ".zcode", "skills") }
func globalCommandsDir(home string) string { return filepath.Join(home, ".zcode", "commands") }

// projectSkillsDir is the repo-local skills root. Generated community
// skills land here because they describe *this* repo and are worthless
// machine-wide.
func projectSkillsDir(root string) string {
	return filepath.Join(root, ".zcode", "skills")
}

// Skills returns the curated Gortex playbook pack in host-neutral form.
// The bodies are single-sourced in internal/agents/claudecode and each
// host re-wraps them in its own frontmatter dialect; skillpack does the
// stripping. Reading claudecode's map directly from an adapter is the
// established direction (codex, opencode and hermes do the same).
func Skills() ([]skillpack.Skill, error) {
	return skillpack.ParseAll(claudecode.GlobalSkills)
}

// Commands returns the slash-command pack in host-neutral form, keyed
// by the command name (the file's base name, minus the extension).
// The source is claudecode.GlobalSkills rather than
// claudecode.SlashCommands wherever both carry the same id: only the
// skill map wraps the body in frontmatter, and that frontmatter is
// where the description lives.
func Commands() ([]skillpack.Skill, error) {
	raw := make(map[string]string, len(claudecode.SlashCommands))
	for file, body := range claudecode.SlashCommands {
		id := trimExt(file)
		if described, ok := claudecode.GlobalSkills[id]; ok {
			body = described
		}
		raw[id] = body
	}
	return skillpack.ParseAll(raw)
}

func trimExt(file string) string { return file[:len(file)-len(commandFileExt)] }

// renderSkill puts ZCode's frontmatter envelope in front of a neutral
// body. `name` comes from the ID — see the package note above.
func renderSkill(s skillpack.Skill) string {
	return skillpack.RenderFrontmatter([][2]string{
		{"name", s.ID},
		{"description", s.Description},
	}) + "\n" + s.Body
}

// renderCommand puts the command frontmatter in front of a neutral
// body. A command without a description gets no frontmatter at all
// rather than an empty `description:` line.
func renderCommand(c skillpack.Skill) string {
	if c.Description == "" {
		return c.Body
	}
	return skillpack.RenderFrontmatter([][2]string{
		{"description", c.Description},
	}) + "\n" + c.Body
}

// applySkills installs whichever artifact set belongs to this Env's
// mode. The sets never overlap: the curated pack is machine-wide, the
// generated community pack is repo-specific.
func applySkills(env agents.Env, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	if env.Mode == agents.ModeGlobal {
		return installCuratedPack(env, opts)
	}
	return installGeneratedSkills(env, opts), nil
}

// installCuratedPack writes the playbooks to
// ~/.zcode/skills/<id>/SKILL.md and the slash commands to
// ~/.zcode/commands/<id>.md. ModeGlobal only: the playbooks are
// codebase-agnostic, so a `gortex init` that dropped them into a repo
// would hand the user's teammates a pile of unrelated files to review.
// An existing file that differs from the shipped body is the user's
// and is left alone ("customised").
func installCuratedPack(env agents.Env, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	if env.Home == "" {
		return nil, nil
	}
	out, err := syncCuratedSkills(env.Stderr, globalSkillsDir(env.Home), nil, opts)
	if err != nil {
		return out, err
	}
	commands, err := Commands()
	if err != nil {
		return out, err
	}
	commandsRoot := globalCommandsDir(env.Home)
	for _, c := range commands {
		path := filepath.Join(commandsRoot, c.ID+commandFileExt)
		action, err := writeCuratedFile(env.Stderr, path, renderCommand(c), opts)
		if err != nil {
			return out, fmt.Errorf("command %s: %w", c.ID, err)
		}
		out = append(out, action)
	}
	return out, nil
}

// SyncSkills reshapes an ALREADY-INSTALLED ~/.zcode/skills tree to the
// allowed subset (nil = every shipped skill). This is the entry point
// `gortex instructions switch` drives. A missing skills root is a
// silent no-op: switching a profile must never conjure a ZCode surface
// on a machine that never ran `gortex install`. Only skills are
// reconciled, not the command pack — a slash command is invoked
// deliberately by name; a skill is auto-selected into context, which
// is the cost an instruction profile exists to control.
func SyncSkills(w io.Writer, home string, allowed []string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	if home == "" {
		return nil, nil
	}
	root := globalSkillsDir(home)
	if _, err := os.Stat(root); err != nil {
		return nil, nil
	}
	return syncCuratedSkills(w, root, allowed, opts)
}

// syncCuratedSkills is the one reconciler both entry points reach. The
// deletion rule (never remove a customised file) lives in skillpack,
// so every skill-aware host enforces it identically.
func syncCuratedSkills(w io.Writer, root string, allowed []string, opts agents.ApplyOpts) ([]agents.FileAction, error) {
	skills, err := Skills()
	if err != nil {
		return nil, err
	}
	rendered := make(map[string]string, len(skills))
	for _, s := range skills {
		rendered[s.ID] = renderSkill(s)
	}
	return skillpack.Sync(w, skillpack.SyncSpec{
		Dir:      root,
		FileName: skillFileName,
		Rendered: rendered,
	}, allowed, opts)
}

// installGeneratedSkills materialises the per-community skills the
// skills generator produced, at <repo>/.zcode/skills/<DirName>/SKILL.md.
// WriteOwnedFile (not the never-clobber writer) because these track
// the current graph and are regenerated on every init; byte-identical
// content reports ActionSkip "unchanged", keeping re-runs idempotent.
func installGeneratedSkills(env agents.Env, opts agents.ApplyOpts) []agents.FileAction {
	root := projectSkillsDir(env.Root)
	out := make([]agents.FileAction, 0, len(env.GeneratedSkills))
	for _, s := range env.GeneratedSkills {
		if !skillpack.ValidID(s.DirName) {
			internalutil.Warnf(env.Stderr, "skipping generated skill %q: ZCode skill directories must be kebab-case", s.DirName)
			continue
		}
		path := filepath.Join(root, s.DirName, skillFileName)
		action, err := agents.WriteOwnedFile(env.Stderr, path, s.Content, opts)
		if err != nil {
			internalutil.Warnf(env.Stderr, "could not write generated skill %s: %v", s.DirName, err)
			continue
		}
		out = append(out, action)
	}
	return out
}

// planSkills enumerates every skill / command file this adapter would
// write for env. Plan is what `gortex doctor` and --print-config read
// — they never call Apply — so a path Apply writes but Plan omits is
// invisible to both.
func planSkills(env agents.Env) ([]agents.FileAction, error) {
	if env.Mode == agents.ModeGlobal {
		if env.Home == "" {
			return nil, nil
		}
		skills, err := Skills()
		if err != nil {
			return nil, err
		}
		commands, err := Commands()
		if err != nil {
			return nil, err
		}
		out := make([]agents.FileAction, 0, len(skills)+len(commands))
		for _, s := range skills {
			out = append(out, agents.FileAction{
				Path:   filepath.Join(globalSkillsDir(env.Home), s.ID, skillFileName),
				Action: agents.ActionWouldCreate,
			})
		}
		for _, c := range commands {
			out = append(out, agents.FileAction{
				Path:   filepath.Join(globalCommandsDir(env.Home), c.ID+commandFileExt),
				Action: agents.ActionWouldCreate,
			})
		}
		return out, nil
	}
	if env.Root == "" {
		return nil, nil
	}
	out := make([]agents.FileAction, 0, len(env.GeneratedSkills))
	for _, s := range env.GeneratedSkills {
		if !skillpack.ValidID(s.DirName) {
			continue
		}
		out = append(out, agents.FileAction{
			Path:   filepath.Join(projectSkillsDir(env.Root), s.DirName, skillFileName),
			Action: agents.ActionWouldCreate,
		})
	}
	return out, nil
}

// writeCuratedFile installs content at path only when nothing is there,
// and never overwrites a file the user has edited. Replicated per
// adapter rather than shared, matching the codex/opencode precedent.
func writeCuratedFile(w io.Writer, path, content string, opts agents.ApplyOpts) (agents.FileAction, error) {
	existing, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return agents.WriteIfNotExists(w, path, content, opts)
	}
	if err != nil {
		return agents.FileAction{}, fmt.Errorf("read %s: %w", path, err)
	}
	if string(existing) == content {
		return agents.FileAction{Path: path, Action: agents.ActionSkip, Reason: "unchanged"}, nil
	}
	internalutil.Warnf(w, "keeping customised %s", path)
	return agents.FileAction{Path: path, Action: agents.ActionSkip, Reason: "customised"}, nil
}
