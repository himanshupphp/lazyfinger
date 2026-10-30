package patch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

type Edit struct {
	Path, Search, Replace string
}

type Proposal struct {
	Explanation string
	Edits       []Edit
	Commands    []string
}

// Change is a fully validated, ready-to-write file change.
type Change struct {
	Path    string
	Old     string
	New     string
	Created bool
}

// Parse reads the model's reply. It's forgiving about code fences and stray
// text, because small local models add them constantly.
func Parse(reply string) (*Proposal, error) {
	const (
		outside = iota
		inSearch
		inReplace
	)
	p := &Proposal{}
	state, file, inExpl := outside, "", false
	var expl, search, replace []string

	for _, line := range strings.Split(strings.ReplaceAll(reply, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch state {
		case outside:
			switch {
			case strings.HasPrefix(t, "```"):
			case strings.HasPrefix(t, "EXPLANATION:"):
				inExpl = true
				if rest := strings.TrimSpace(strings.TrimPrefix(t, "EXPLANATION:")); rest != "" {
					expl = append(expl, rest)
				}
			case strings.HasPrefix(t, "FILE:"):
				inExpl = false
				file = strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, "FILE:")), "`\"' ")
			case strings.HasPrefix(t, "COMMAND:"):
				inExpl = false
				if c := strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, "COMMAND:")), "`"); c != "" {
					p.Commands = append(p.Commands, c)
				}
			case strings.HasPrefix(t, "<<<<<<<"):
				if file == "" {
					return nil, errors.New("edit block without a FILE: line")
				}
				inExpl, state, search = false, inSearch, nil
			case inExpl && t != "":
				expl = append(expl, t)
			}
		case inSearch:
			if strings.HasPrefix(t, "=======") {
				state, replace = inReplace, nil
				continue
			}
			search = append(search, line)
		case inReplace:
			if strings.HasPrefix(t, ">>>>>>>") {
				p.Edits = append(p.Edits, Edit{Path: file, Search: strings.Join(search, "\n"), Replace: strings.Join(replace, "\n")})
				state = outside
				continue
			}
			replace = append(replace, line)
		}
	}
	if state != outside {
		return nil, errors.New("unterminated edit block")
	}
	p.Explanation = strings.Join(expl, " ")
	return p, nil
}

// Prepare validates every edit against the files on disk without writing anything.
func Prepare(root string, edits []Edit) ([]Change, error) {
	byPath := map[string]*Change{}
	var order []string
	for _, e := range edits {
		rel, err := SafePath(root, e.Path)
		if err != nil {
			return nil, err
		}
		ch, ok := byPath[rel]
		if !ok {
			data, err := os.ReadFile(filepath.Join(root, rel))
			switch {
			case err == nil:
				ch = &Change{Path: rel, Old: string(data), New: string(data)}
			case errors.Is(err, os.ErrNotExist):
				ch = &Change{Path: rel, Created: true}
			default:
				return nil, err
			}
			byPath[rel] = ch
			order = append(order, rel)
		}
		updated, err := applyOne(ch.New, e, ch.Created)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		ch.New = updated
	}
	out := make([]Change, 0, len(order))
	for _, p := range order {
		if c := byPath[p]; c.New != c.Old || c.Created {
			out = append(out, *c)
		}
	}
	return out, nil
}

func applyOne(content string, e Edit, created bool) (string, error) {
	if e.Search == "" {
		if !created || content != "" {
			return "", errors.New("empty SEARCH is only allowed when creating a new file")
		}
		if !strings.HasSuffix(e.Replace, "\n") {
			e.Replace += "\n"
		}
		return e.Replace, nil
	}
	if created && content == "" {
		return "", errors.New("file does not exist")
	}

	crlf := strings.Contains(content, "\r\n")
	search, repl := e.Search, e.Replace
	if crlf {
		search = strings.ReplaceAll(search, "\n", "\r\n")
		repl = strings.ReplaceAll(repl, "\n", "\r\n")
	}
	switch n := strings.Count(content, search); {
	case n == 1:
		return strings.Replace(content, search, repl, 1), nil
	case n > 1:
		return "", fmt.Errorf("SEARCH text matches %d places; include more surrounding lines", n)
	}
	if out, ok := looseReplace(content, e.Search, e.Replace, crlf); ok {
		return out, nil
	}
	return "", errors.New("SEARCH text not found in file")
}

// looseReplace retries ignoring trailing whitespace, a common local-model slip.
func looseReplace(content, search, repl string, crlf bool) (string, bool) {
	nl := "\n"
	if crlf {
		nl = "\r\n"
	}
	lines := strings.Split(content, nl)
	want := strings.Split(search, "\n")
	norm := func(s string) string { return strings.TrimRight(s, " \t\r") }
	found := -1
	for i := 0; i+len(want) <= len(lines); i++ {
		match := true
		for j := range want {
			if norm(lines[i+j]) != norm(want[j]) {
				match = false
				break
			}
		}
		if match {
			if found >= 0 {
				return "", false // ambiguous
			}
			found = i
		}
	}
	if found < 0 {
		return "", false
	}
	out := append([]string{}, lines[:found]...)
	out = append(out, strings.Split(repl, "\n")...)
	out = append(out, lines[found+len(want):]...)
	return strings.Join(out, nl), true
}

// Within returns p relative to root when p is inside the project. It compares
// real paths, because tools often print resolved paths (/private/tmp/x on
// macOS) while the shell's working directory uses a symlink (/tmp/x). It also
// rejects symlinks inside the project that point outside it.
func Within(root, p string) (string, bool) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	p = filepath.Clean(p)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	realRel, ok := relInside(realRoot, realPath(p))
	if !ok {
		return "", false
	}
	if rel, ok := relInside(root, p); ok {
		return rel, true // prefer the path as the project sees it
	}
	return realRel, true
}

// SafePath validates a path the model wants to edit.
func SafePath(root, p string) (string, error) {
	p = filepath.FromSlash(strings.TrimSpace(p))
	rel, ok := Within(root, p)
	if !ok || rel == "." {
		return "", fmt.Errorf("path %q is outside the project", p)
	}
	slash := filepath.ToSlash(rel)
	first := strings.SplitN(slash, "/", 2)[0]
	switch first {
	case ".git", ".lazyfinger", "node_modules", "vendor":
		return "", fmt.Errorf("refusing to edit %q", p)
	}
	if strings.HasPrefix(slash, "storage/framework/") || strings.HasPrefix(slash, "bootstrap/cache/") {
		return "", fmt.Errorf("refusing to edit generated file %q", p)
	}
	if strings.HasPrefix(filepath.Base(rel), ".env") {
		return "", fmt.Errorf("refusing to edit %q: environment files hold secrets, so set values there yourself", p)
	}
	return rel, nil
}

func relInside(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// realPath resolves symlinks in the deepest part of p that exists, so it also
// works for files the model wants to create.
func realPath(p string) string {
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func (c Change) Diff() string {
	var a []string
	if !c.Created {
		a = difflib.SplitLines(c.Old)
	}
	s, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: a, B: difflib.SplitLines(c.New),
		FromFile: "a/" + filepath.ToSlash(c.Path), ToFile: "b/" + filepath.ToSlash(c.Path),
		Context: 3,
	})
	return s
}

func Write(root string, changes []Change) error {
	for _, c := range changes {
		full := filepath.Join(root, c.Path)
		if c.Created {
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
		}
		// WriteFile keeps the existing permissions of files that already exist.
		if err := os.WriteFile(full, []byte(c.New), 0o644); err != nil {
			return err
		}
	}
	return nil
}
