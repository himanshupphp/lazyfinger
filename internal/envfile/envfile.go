// Package envfile fills in a project's .env from its example file.
package envfile

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Var struct {
	Key     string
	Default string
	Comment string
	Secret  bool
}

type Plan struct {
	Example   string // e.g. ".env.example"
	EnvExists bool
	Ask       []Var // values to ask for: empty in the example and in .env
	Copy      []Var // missing from .env; written with the example's value
}

var (
	assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
	secretRe = regexp.MustCompile(`(?i)(SECRET|TOKEN|PASSWORD|PASSWD|_PASS$|API_?KEY|PRIVATE|_KEY$|CREDENTIAL)`)
)

// Inspect compares .env with the example file. It returns nil when the
// project has no example file. Keys in skip are filled by a setup step.
func Inspect(root string, skip []string) (*Plan, error) {
	example := ""
	for _, n := range []string{".env.example", ".env.sample", ".env.dist", ".env.template"} {
		if _, err := os.Stat(filepath.Join(root, n)); err == nil {
			example = n
			break
		}
	}
	if example == "" {
		return nil, nil
	}
	vars, err := parse(filepath.Join(root, example))
	if err != nil {
		return nil, err
	}
	env, envErr := values(filepath.Join(root, ".env"))
	plan := &Plan{Example: example, EnvExists: envErr == nil}
	skipped := map[string]bool{}
	for _, k := range skip {
		skipped[k] = true
	}
	for _, v := range vars {
		cur, inEnv := env[v.Key]
		hasDefault := v.Default != "" && !placeholder(v.Default)
		switch {
		case inEnv && cur != "":
			continue
		case skipped[v.Key]:
			if !inEnv {
				plan.Copy = append(plan.Copy, v) // keep the line; a setup step fills it
			}
		case !inEnv && hasDefault:
			plan.Copy = append(plan.Copy, v)
		case !hasDefault:
			v.Default = ""
			plan.Ask = append(plan.Ask, v)
		}
	}
	return plan, nil
}

func placeholder(v string) bool {
	l := strings.ToLower(v)
	for _, p := range []string{"your_", "your-", "changeme", "change_me", "change-me", "xxx", "<", "replace", "example.com", "todo"} {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

func parse(path string) ([]Var, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Var
	var comment []string
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			comment = nil
		case strings.HasPrefix(t, "#"):
			c := strings.TrimSpace(strings.TrimLeft(t, "#"))
			if _, _, isVar := cut(c); isVar {
				comment = nil // a commented-out variable, not a description
			} else if c != "" {
				comment = append(comment, c)
			}
		default:
			if k, v, ok := cut(t); ok {
				out = append(out, Var{Key: k, Default: v, Comment: strings.Join(comment, " "), Secret: secretRe.MatchString(k)})
			}
			comment = nil
		}
	}
	return out, nil
}

func values(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if k, v, ok := cut(t); ok {
			out[k] = v
		}
	}
	return out, nil
}

func cut(t string) (string, string, bool) {
	t = strings.TrimPrefix(t, "export ")
	k, v, ok := strings.Cut(t, "=")
	k = strings.TrimSpace(k)
	if !ok || !assignRe.MatchString(k) {
		return "", "", false
	}
	return k, unquote(strings.TrimSpace(v)), true
}

func unquote(v string) string {
	if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
		return v[1 : len(v)-1]
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

func quote(v string) string {
	if v == "" || !strings.ContainsAny(v, " #\"'$\\`") {
		return v
	}
	if !strings.Contains(v, "'") {
		return "'" + v + "'" // single quotes are literal in dotenv files
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`).Replace(v) + `"`
}

// Write creates or completes .env: filled values replace empty ones, and
// keys missing from an existing .env are appended with the example's value.
func Write(root string, plan *Plan, filled map[string]string) error {
	envPath := filepath.Join(root, ".env")
	src := envPath
	if !plan.EnvExists {
		src = filepath.Join(root, plan.Example)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	done := map[string]bool{}
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, _, ok := cut(t)
		if !ok {
			continue
		}
		if v, ok := filled[k]; ok {
			lines[i] = k + "=" + quote(v)
		}
		done[k] = true
	}
	var extra []string
	for _, v := range plan.Copy {
		if !done[v.Key] {
			extra = append(extra, v.Key+"="+quote(v.Default))
			done[v.Key] = true
		}
	}
	for _, v := range plan.Ask {
		if val, ok := filled[v.Key]; ok && !done[v.Key] {
			extra = append(extra, v.Key+"="+quote(val))
			done[v.Key] = true
		}
	}
	if len(extra) > 0 {
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "", "# Added by lazy run from "+plan.Example)
		lines = append(lines, extra...)
	}
	out := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	return os.WriteFile(envPath, []byte(out), 0o600) // keeps existing permissions if .env exists
}

// NeedsIgnore reports whether this is a git repo whose .gitignore doesn't cover .env.
func NeedsIgnore(root string) bool {
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return false
	}
	data, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
	for _, line := range strings.Split(string(data), "\n") {
		switch strings.TrimSpace(line) {
		case ".env", "/.env", ".env*", "/.env*", "*.env":
			return false
		}
	}
	return true
}

func AddIgnore(root string) error {
	p := filepath.Join(root, ".gitignore")
	prefix := ""
	if data, err := os.ReadFile(p); err == nil && len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		prefix = "\n"
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(prefix + "\n# Local environment (added by LazyFinger)\n.env\n")
	return err
}
