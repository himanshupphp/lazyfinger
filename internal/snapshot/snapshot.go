package snapshot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/himanshupphp/lazyfinger/internal/patch"
)

type manifest struct {
	Summary  string   `json:"summary"`
	Created  []string `json:"created"`
	Modified []string `json:"modified"`
}

// Save backs up original files to .lazyfinger/snapshots/<id>/ before a fix is
// written, and returns the id.
func Save(root string, changes []patch.Change, summary string) (string, error) {
	base := filepath.Join(root, ".lazyfinger")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	_ = os.WriteFile(filepath.Join(base, ".gitignore"), []byte("*\n"), 0o644)

	id := time.Now().Format("20060102-150405.000000")
	dir := filepath.Join(base, "snapshots", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	m := manifest{Summary: summary}
	for _, c := range changes {
		if c.Created {
			m.Created = append(m.Created, c.Path)
			continue
		}
		dst := filepath.Join(dir, "files", c.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(dst, []byte(c.Old), 0o644); err != nil {
			return "", err
		}
		m.Modified = append(m.Modified, c.Path)
	}
	data, _ := json.MarshalIndent(m, "", "  ")
	return id, os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644)
}

// Undo reverts the most recent snapshot. Call it repeatedly to step further back.
func Undo(root string) (string, error) {
	snaps := filepath.Join(root, ".lazyfinger", "snapshots")
	entries, err := os.ReadDir(snaps) // sorted by name, so the last is the newest
	if err != nil || len(entries) == 0 {
		return "", errors.New("nothing to undo")
	}
	latest := filepath.Join(snaps, entries[len(entries)-1].Name())
	data, err := os.ReadFile(filepath.Join(latest, "manifest.json"))
	if err != nil {
		return "", err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return "", err
	}
	for _, p := range m.Modified {
		orig, err := os.ReadFile(filepath.Join(latest, "files", p))
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(root, p), orig, 0o644); err != nil {
			return "", err
		}
	}
	for _, p := range m.Created {
		_ = os.Remove(filepath.Join(root, p))
	}
	return m.Summary, os.RemoveAll(latest)
}
