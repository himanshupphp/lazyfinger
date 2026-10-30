package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/himanshupphp/lazyfinger/internal/provider"
)

const suggestPrompt = `You work out how to run a software project locally on a developer's computer.

Reply in exactly this format and nothing else:

NAME: <short project type, e.g. "Hugo site" or "Elixir Phoenix">
STEP install: <command>
STEP setup: <command>
STEP start: <command>

Rules:
- Use only the steps that are needed, at most 6, in the order to run them. Exactly one start step, and it comes last.
- install steps install dependencies; setup steps prepare things like databases; start runs the app.
- Commands must work on the operating system given, from the project folder.
- Never include sudo, commands that delete files, or commands that download and execute scripts (like curl ... | sh).
- If you can't tell how to run the project, reply with only: NAME: unknown`

// Suggest asks the model how to run a project the built-in detectors don't know.
func Suggest(ctx context.Context, p provider.Provider, root string) (*Project, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Operating system: %s/%s\n\n## Files\n%s\n", runtime.GOOS, runtime.GOARCH, listFiles(root, 150))
	for _, f := range []string{"README.md", "README", "readme.md", "README.rst", "Makefile", "justfile", "Procfile",
		"package.json", "composer.json", "pyproject.toml", "requirements.txt", "Cargo.toml", "go.mod", "Gemfile",
		"pom.xml", "build.gradle", "mix.exs", "deno.json", "compose.yaml", "docker-compose.yml"} {
		if s := read(root, f); s != "" {
			if len(s) > 6000 {
				s = s[:6000] + "\n... (truncated)"
			}
			fmt.Fprintf(&b, "\n## %s\n```\n%s\n```\n", f, s)
		}
	}
	reply, err := p.Complete(ctx, suggestPrompt, []provider.Message{{Role: "user", Content: b.String()}})
	if err != nil {
		return nil, err
	}
	return parseSuggestion(reply)
}

var stepRe = regexp.MustCompile(`(?i)^STEP\s+(install|setup|start)\s*:\s*(.+)$`)

func parseSuggestion(reply string) (*Project, error) {
	p := &Project{Name: "Project"}
	kinds := map[string]StepKind{"install": Install, "setup": Setup, "start": Start}
	titles := map[string]string{"install": "install dependencies", "setup": "set up", "start": "start the app"}
	for _, line := range strings.Split(reply, "\n") {
		t := strings.Trim(strings.TrimSpace(line), "`*- ")
		if strings.HasPrefix(strings.ToUpper(t), "NAME:") {
			p.Name = strings.TrimSpace(t[5:])
			continue
		}
		m := stepRe.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		cmd := strings.Trim(strings.TrimSpace(m[2]), "`")
		if unsafeCommand(cmd) {
			continue
		}
		kind := strings.ToLower(m[1])
		p.Steps = append(p.Steps, Step{Title: titles[kind], Args: []string{cmd}, Kind: kinds[kind], Ask: kinds[kind] == Setup})
	}
	if strings.EqualFold(p.Name, "unknown") || p.StartStep() == nil {
		return nil, errors.New("the model couldn't tell how to start it")
	}
	for _, s := range p.Steps {
		if f := strings.Fields(s.Args[0]); len(f) > 0 && !strings.ContainsAny(f[0], `/\.`) {
			p.Needs = append(p.Needs, Need{Tool: f[0], Label: f[0]})
		}
	}
	return p, nil
}

func unsafeCommand(c string) bool {
	l := strings.ToLower(c)
	for _, bad := range []string{"sudo ", "rm -rf", "rm -r ", "| sh", "|sh", "| bash", "|bash", "iex ", "invoke-expression",
		"curl ", "wget ", "mkfs", "dd if=", "format c:", "del /s", "rmdir /s"} {
		if strings.Contains(l, bad) {
			return true
		}
	}
	return false
}

var skipListing = map[string]bool{".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"target": true, "build": true, "dist": true, ".next": true, "__pycache__": true, ".lazyfinger": true, "storage": true}

func listFiles(root string, limit int) string {
	var lines []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == root {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		depth := strings.Count(filepath.ToSlash(rel), "/")
		if d.IsDir() {
			if skipListing[d.Name()] || depth >= 2 {
				return filepath.SkipDir
			}
			lines = append(lines, filepath.ToSlash(rel)+"/")
		} else {
			lines = append(lines, filepath.ToSlash(rel))
		}
		if len(lines) >= limit {
			return filepath.SkipAll
		}
		return nil
	})
	return strings.Join(lines, "\n")
}
