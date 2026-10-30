// Package journal keeps LazyFinger's memory of a project: every failure, every fix a
// model proposed, what you decided, and what happened next. It lives in
// .lazyfinger/journal.jsonl, so it survives switching models (for example when one
// runs out of quota), and later runs can avoid repeating fixes that failed.
package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Event struct {
	Time     time.Time `json:"time"`
	Session  string    `json:"session"`
	Issue    string    `json:"issue,omitempty"` // error fingerprint
	Kind     string    `json:"kind"`            // run, failure, proposal, decision, applied, result, switch
	Command  string    `json:"command,omitempty"`
	Provider string    `json:"provider,omitempty"`
	Summary  string    `json:"summary,omitempty"`
	Files    []string  `json:"files,omitempty"`
	Commands []string  `json:"commands,omitempty"`
	Snapshot string    `json:"snapshot,omitempty"`
	Outcome  string    `json:"outcome,omitempty"`
	Detail   string    `json:"detail,omitempty"` // redacted error excerpt, user hint, or switch reason
}

type Journal struct {
	mu   sync.Mutex
	path string
}

const maxSize = 5 << 20

// Open prepares the journal for writing, creating .lazyfinger/ if needed.
func Open(root string) *Journal {
	dir := filepath.Join(root, ".lazyfinger")
	_ = os.MkdirAll(dir, 0o755)
	// A .gitignore containing "*" makes git ignore the whole folder, itself included.
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n"), 0o644)
	p := filepath.Join(dir, "journal.jsonl")
	if fi, err := os.Stat(p); err == nil && fi.Size() > maxSize {
		_ = os.Rename(p, filepath.Join(dir, "journal.old.jsonl"))
	}
	return &Journal{path: p}
}

// Load opens the journal for reading only, without creating anything.
func Load(root string) *Journal {
	return &Journal{path: filepath.Join(root, ".lazyfinger", "journal.jsonl")}
}

func NewSession() string { return time.Now().Format("20060102-150405.000") }

func (j *Journal) Add(e Event) {
	if j == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

func (j *Journal) Read() []Event {
	f, err := os.Open(j.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// Attempt is one proposed fix and everything that happened to it.
type Attempt struct {
	Time     time.Time
	Session  string
	Issue    string
	Provider string
	Summary  string
	Files    []string
	Commands []string
	Outcome  []string
}

func (a Attempt) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", a.Provider, a.Summary)
	if len(a.Files) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(a.Files, ", "))
	}
	if len(a.Commands) > 0 {
		fmt.Fprintf(&b, " [ran: %s]", strings.Join(a.Commands, "; "))
	}
	if len(a.Outcome) > 0 {
		b.WriteString(" → " + strings.Join(a.Outcome, " → "))
	} else {
		b.WriteString(" → no decision recorded")
	}
	return b.String()
}

func attempts(events []Event) []Attempt {
	var out []Attempt
	open := map[string]int{} // session|issue -> latest attempt
	for _, e := range events {
		key := e.Session + "|" + e.Issue
		switch e.Kind {
		case "proposal":
			out = append(out, Attempt{Time: e.Time, Session: e.Session, Issue: e.Issue, Provider: e.Provider,
				Summary: e.Summary, Files: e.Files, Commands: e.Commands})
			open[key] = len(out) - 1
		case "decision", "applied", "result":
			if i, ok := open[key]; ok {
				o := e.Outcome
				if o == "" {
					o = e.Kind
				}
				if e.Detail != "" {
					o += " (" + e.Detail + ")"
				}
				out[i].Outcome = append(out[i].Outcome, o)
			}
		}
	}
	return out
}

// Recall lists earlier attempts at the same error, oldest first, so a model
// (possibly a different one) can learn from them.
func (j *Journal) Recall(issue string, limit int) []string {
	var lines []string
	for _, a := range attempts(j.Read()) {
		if a.Issue == issue {
			lines = append(lines, a.Time.Local().Format("Jan 2 15:04")+", "+a.String())
		}
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines
}

// Tree renders the most recent sessions as a readable tree.
func (j *Journal) Tree(sessions int) string {
	events := j.Read()
	if len(events) == 0 {
		return ""
	}
	type item struct {
		t    time.Time
		text string
	}
	type issueInfo struct {
		detail string
		items  []item
	}
	type session struct {
		command string
		t       time.Time
		order   []string
		issues  map[string]*issueInfo
	}
	byID := map[string]*session{}
	var ids []string
	get := func(id string, t time.Time) *session {
		s, ok := byID[id]
		if !ok {
			s = &session{t: t, issues: map[string]*issueInfo{}}
			byID[id] = s
			ids = append(ids, id)
		}
		return s
	}
	issueOf := func(s *session, id string) *issueInfo {
		iss, ok := s.issues[id]
		if !ok {
			iss = &issueInfo{}
			s.issues[id] = iss
			s.order = append(s.order, id)
		}
		return iss
	}
	for _, e := range events {
		s := get(e.Session, e.Time)
		switch e.Kind {
		case "run":
			s.command = e.Command
		case "failure":
			if iss := issueOf(s, e.Issue); iss.detail == "" {
				iss.detail = headline(e.Detail)
			}
		case "switch":
			iss := issueOf(s, e.Issue)
			iss.items = append(iss.items, item{e.Time, "switched: " + e.Detail})
		}
	}
	for _, a := range attempts(events) {
		if s, ok := byID[a.Session]; ok {
			iss := issueOf(s, a.Issue)
			iss.items = append(iss.items, item{a.Time, a.String()})
		}
	}

	if len(ids) > sessions {
		ids = ids[len(ids)-sessions:]
	}
	var b strings.Builder
	for _, id := range ids {
		s := byID[id]
		fmt.Fprintf(&b, "● %s  lazy %s\n", s.t.Local().Format("Jan 2 15:04"), s.command)
		for i, issID := range s.order {
			iss := s.issues[issID]
			lastIssue := i == len(s.order)-1
			branch, pad := "├─", "│  "
			if lastIssue {
				branch, pad = "└─", "   "
			}
			detail := iss.detail
			if detail == "" {
				detail = "error " + issID
			}
			fmt.Fprintf(&b, "  %s %s\n", branch, detail)
			sort.SliceStable(iss.items, func(a, c int) bool { return iss.items[a].t.Before(iss.items[c].t) })
			for k, it := range iss.items {
				sub := "├─"
				if k == len(iss.items)-1 {
					sub = "└─"
				}
				fmt.Fprintf(&b, "  %s %s %s\n", pad, sub, it.text)
			}
		}
	}
	return b.String()
}

// headline picks the most error-like line from an excerpt.
func headline(detail string) string {
	lines := strings.Split(strings.TrimSpace(detail), "\n")
	pick := ""
	for _, l := range lines {
		t := strings.TrimSpace(l)
		lt := strings.ToLower(t)
		if strings.Contains(lt, "error") || strings.Contains(lt, "exception") || strings.Contains(lt, "failed") {
			pick = t
			break
		}
	}
	if pick == "" && len(lines) > 0 {
		pick = strings.TrimSpace(lines[len(lines)-1])
	}
	if len(pick) > 100 {
		pick = pick[:100] + "..."
	}
	return pick
}
