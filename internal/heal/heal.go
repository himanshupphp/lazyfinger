package heal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/himanshupphp/lazyfinger/internal/config"
	"github.com/himanshupphp/lazyfinger/internal/editor"
	"github.com/himanshupphp/lazyfinger/internal/gather"
	"github.com/himanshupphp/lazyfinger/internal/journal"
	"github.com/himanshupphp/lazyfinger/internal/patch"
	"github.com/himanshupphp/lazyfinger/internal/provider"
	"github.com/himanshupphp/lazyfinger/internal/runner"
	"github.com/himanshupphp/lazyfinger/internal/snapshot"
	"github.com/himanshupphp/lazyfinger/internal/ui"
)

type Options struct {
	Root         string
	Args         []string
	Mode         config.Mode
	MaxAttempts  int
	Provider     provider.Provider
	AutoFallback bool   // switch providers without asking (a fallback order is configured)
	NoRerun      bool   // after applying a fix, return instead of re-running (the caller restarts)
	Headline     string // title for the first attempt, instead of "Command failed"
}

const systemPrompt = `You are LazyFinger, a tool that diagnoses and fixes failing commands in software projects.

You receive a failing command, its output, relevant project files, and sometimes framework notes, installed tool versions, the names of environment variables, and a record of earlier attempts at this same error. Find the root cause and propose the smallest fix that makes the command succeed.

Reply in exactly this format and nothing else:

EXPLANATION: <one to three plain-English sentences that a non-programmer could follow>

Then zero or more edits, each like this:

FILE: path/relative/to/project
<<<<<<< SEARCH
exact lines copied from the file
=======
replacement lines
>>>>>>> REPLACE

Rules for edits:
- Read the file contents shown before editing. Fix the root cause, and don't add declarations, imports or functions that already exist.
- Only edit files whose contents are shown. If you need a file that isn't shown, say so in the EXPLANATION instead of guessing its contents.
- SEARCH must be copied exactly from the file contents shown, including indentation, and must match exactly one place. Include a few surrounding lines if needed to make it unique.
- Keep SEARCH short: only the lines that change plus minimal context.
- To create a new file, leave SEARCH empty.
- Never edit .env files, lockfiles, or anything in node_modules, vendor, storage/framework or bootstrap/cache.

If the fix is a shell command instead (for example installing a missing dependency), add a line:
COMMAND: <the command>

Rules for commands:
- Use the project's existing package manager (see the lockfiles) and only suggest well-known, real packages.
- Never propose commands that can delete data, such as migrate:fresh, db:wipe, dropping tables, or rm -rf.
- Follow the framework notes when they are present.

When earlier attempts are listed, don't repeat a fix that was rejected or didn't work; use what they revealed.

If the fix needs something only the user can provide (a database password, an API key, a running service, a newer runtime version), say exactly what in the EXPLANATION and propose no edit. If you cannot find a confident fix, reply with only the EXPLANATION describing what you found.`

// Run runs the command and, if it fails, tries to fix it.
func Run(ctx context.Context, o Options) int {
	res, err := runner.Run(o.Root, o.Args)
	if err != nil {
		ui.Error("Could not start command: " + err.Error())
		return 127
	}
	if !res.Failed() {
		return res.ExitCode
	}
	return Fix(ctx, o, res)
}

// Fix starts from a failure that already happened. It returns 0 when the
// command was fixed, or (with NoRerun) when a fix was applied.
func Fix(ctx context.Context, o Options, res *runner.Result) int {
	j := journal.Open(o.Root)
	sess := journal.NewSession()
	cmdline := strings.Join(o.Args, " ")
	j.Add(journal.Event{Session: sess, Kind: "run", Command: cmdline})
	issue := ""

	if ch, ok := o.Provider.(*provider.Chain); ok {
		ch.OnSwitch = func(from, to provider.Provider, reason string) bool {
			if o.AutoFallback {
				ui.Warn(fmt.Sprintf("%s %s. Continuing with %s; everything tried so far carries over.", from.Name(), reason, to.Name()))
			} else {
				ui.Warn(fmt.Sprintf("%s %s.", from.Name(), reason))
				if !ui.Confirm(fmt.Sprintf("Continue with %s? Everything tried so far carries over", to.Name())) {
					return false
				}
			}
			if to.Remote() && !ConfirmRemote(to) {
				return false
			}
			j.Add(journal.Event{Session: sess, Issue: issue, Kind: "switch", Provider: to.Name(),
				Detail: fmt.Sprintf("%s → %s (%s)", from.Name(), to.Name(), reason)})
			return true
		}
	}
	if o.Provider.Remote() && !ConfirmRemote(o.Provider) {
		return nonZero(res.ExitCode)
	}

	mode := o.Mode
	var applied []string
	lastFP := ""
	var err error

	for attempt := 1; attempt <= o.MaxAttempts; attempt++ {
		log := gather.Clean(res.Output)
		fp := gather.Fingerprint(log)
		if attempt > 1 && fp == lastFP && mode == config.Autofix {
			ui.Warn("Same error as before the fix. Switching to analyze mode.")
			mode = config.Analyze
		}
		lastFP = fp
		issue = fp
		j.Add(journal.Event{Session: sess, Issue: fp, Kind: "failure", Command: cmdline, Detail: excerpt(log)})

		title := fmt.Sprintf("Command failed (exit %d)", res.ExitCode)
		if o.Headline != "" && attempt == 1 {
			title = o.Headline
		}
		ui.Header(fmt.Sprintf("%s · attempt %d of %d", title, attempt, o.MaxAttempts))
		memory := j.Recall(fp, 8)
		if len(memory) > 0 {
			ui.Muted(fmt.Sprintf("LazyFinger has seen this error before (%d earlier attempt(s)); sharing them with the model.", len(memory)))
		}
		gctx := gather.Collect(o.Root, o.Args, res.ExitCode, log)
		msgs := []provider.Message{{Role: "user", Content: userPrompt(gctx, applied, memory)}}

		var (
			prop    *patch.Proposal
			changes []patch.Change
			cmdRisk risk
			notes   []note
		)
		fresh := true
		for {
			if fresh {
				prop, changes, err = propose(ctx, o.Provider, o.Root, &msgs)
				if err != nil {
					ui.Error(err.Error())
					listApplied(applied)
					return nonZero(res.ExitCode)
				}
				cmdRisk, notes = assessCommands(ctx, o.Root, prop.Commands)
				fresh = false
				j.Add(journal.Event{Session: sess, Issue: fp, Kind: "proposal", Provider: o.Provider.Name(),
					Summary: summarize(prop, changes), Files: paths(changes), Commands: prop.Commands})
			}
			show(prop, changes, notes)
			if len(changes) == 0 && len(prop.Commands) == 0 {
				ui.Warn("No fix to apply. The diagnosis above may still help.")
				j.Add(journal.Event{Session: sess, Issue: fp, Kind: "decision", Outcome: "nothing to apply"})
				listApplied(applied)
				return nonZero(res.ExitCode)
			}
			if !needsApproval(mode, max(cmdRisk, editRisk(changes))) {
				ui.Muted("Autofix mode: applying.")
				break
			}
			if mode == config.Autofix {
				ui.Muted("This fix needs your approval even in autofix mode.")
			}

			choice := ui.Choose("Apply this fix?", approvalOptions(len(changes) > 0)...)
			if choice == 'a' {
				break
			}
			if choice == 's' {
				ui.Muted("Skipped. Nothing was changed.")
				j.Add(journal.Event{Session: sess, Issue: fp, Kind: "decision", Outcome: "skipped"})
				listApplied(applied)
				return nonZero(res.ExitCode)
			}
			if choice == 'e' {
				edited, err := editChanges(o.Root, changes)
				if err != nil {
					ui.Error(err.Error())
					continue
				}
				changes = edited
				j.Add(journal.Event{Session: sess, Issue: fp, Kind: "decision", Outcome: "edited by you", Files: paths(changes)})
				continue
			}
			hint := ui.Ask("What should the model know?")
			j.Add(journal.Event{Session: sess, Issue: fp, Kind: "decision", Outcome: "rejected", Detail: "your hint: " + hint})
			msgs = append(msgs, provider.Message{Role: "user",
				Content: "That fix isn't right. Hint from the user: " + hint + "\nPropose a new fix in the same format."})
			fresh = true
		}

		snapID, err := apply(o.Root, prop, changes)
		if err != nil {
			ui.Error(err.Error())
			j.Add(journal.Event{Session: sess, Issue: fp, Kind: "result", Outcome: "failed to apply", Detail: err.Error()})
			listApplied(applied)
			return nonZero(res.ExitCode)
		}
		j.Add(journal.Event{Session: sess, Issue: fp, Kind: "applied", Outcome: "applied", Snapshot: snapID,
			Files: paths(changes), Commands: prop.Commands})
		applied = append(applied, summarize(prop, changes))

		if o.NoRerun {
			j.Add(journal.Event{Session: sess, Issue: fp, Kind: "result", Outcome: "applied, restarting"})
			ui.Success("Fix applied.")
			listApplied(applied)
			return 0
		}

		ui.Header("Re-running " + cmdline)
		res, err = runner.Run(o.Root, o.Args)
		if err != nil {
			ui.Error(err.Error())
			return 1
		}
		if res.Interrupted {
			return nonZero(res.ExitCode)
		}
		if !res.Failed() {
			j.Add(journal.Event{Session: sess, Issue: fp, Kind: "result", Outcome: "fixed"})
			ui.Success("Fixed. The command now succeeds.")
			listApplied(applied)
			return 0
		}
		outcome := "still failing with a new error"
		if gather.Fingerprint(gather.Clean(res.Output)) == fp {
			outcome = "still failing with the same error"
		}
		j.Add(journal.Event{Session: sess, Issue: fp, Kind: "result", Outcome: outcome})
	}

	ui.Warn(fmt.Sprintf("Still failing after %d attempts.", o.MaxAttempts))
	listApplied(applied)
	return nonZero(res.ExitCode)
}

func nonZero(code int) int {
	if code == 0 {
		return 1
	}
	return code
}

func approvalOptions(canEdit bool) []ui.Option {
	opts := []ui.Option{{Key: 'a', Label: "apply"}}
	if canEdit {
		opts = append(opts, ui.Option{Key: 'e', Label: "edit"})
	}
	// Skip stays last: it's what Choose returns if input ends.
	return append(opts, ui.Option{Key: 'r', Label: "retry with a hint"}, ui.Option{Key: 's', Label: "skip"})
}

// propose asks the model for a fix and validates it, giving the model one
// chance to correct an edit that doesn't match the files.
func propose(ctx context.Context, p provider.Provider, root string, msgs *[]provider.Message) (*patch.Proposal, []patch.Change, error) {
	for try := 0; ; try++ {
		stop := ui.Spinner("Asking " + p.Name() + " for a fix...")
		reply, err := p.Complete(ctx, systemPrompt, *msgs)
		stop()
		if err != nil {
			return nil, nil, err
		}
		*msgs = append(*msgs, provider.Message{Role: "assistant", Content: reply})

		prop, err := patch.Parse(reply)
		var changes []patch.Change
		if err == nil {
			changes, err = patch.Prepare(root, prop.Edits)
		}
		if err == nil {
			return prop, changes, nil
		}
		if try == 1 {
			return nil, nil, fmt.Errorf("the model's fix couldn't be applied: %v", err)
		}
		ui.Muted("The proposed edit didn't match the files (" + err.Error() + "). Asking for a correction...")
		correction := "Your edit could not be applied: " + err.Error() + ".\nReply again in the required format, copying SEARCH text exactly from the current file contents below."
		if prop != nil {
			correction += currentFiles(root, prop)
		}
		*msgs = append(*msgs, provider.Message{Role: "user", Content: correction})
	}
}

// currentFiles shows the model the real contents of the files it tried to
// edit, so a failed edit can be corrected instead of guessed again.
func currentFiles(root string, prop *patch.Proposal) string {
	var b strings.Builder
	seen := map[string]bool{}
	for _, e := range prop.Edits {
		rel, err := patch.SafePath(root, e.Path)
		if err != nil || seen[rel] {
			continue
		}
		seen[rel] = true
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || len(data) > 40000 {
			continue
		}
		fmt.Fprintf(&b, "\n\n## Current contents of %s\n```\n%s\n```", filepath.ToSlash(rel),
			strings.ReplaceAll(string(data), "\r\n", "\n"))
	}
	return b.String()
}

// editChanges opens the proposed version of each file in the user's editor
// and returns the changes with whatever they saved.
func editChanges(root string, changes []patch.Change) ([]patch.Change, error) {
	tmp, err := os.MkdirTemp("", "lazy-edit-")
	if err != nil {
		return changes, err
	}
	defer os.RemoveAll(tmp)

	ed := editor.Command()
	out := make([]patch.Change, 0, len(changes))
	for i, c := range changes {
		p := filepath.Join(tmp, fmt.Sprintf("%d-%s", i+1, filepath.Base(c.Path)))
		if err := os.WriteFile(p, []byte(c.New), 0o600); err != nil {
			return changes, err
		}
		ui.Muted(fmt.Sprintf("Editing %s in %s. Save and close the editor to continue.", c.Path, ed[0]))
		r, err := runner.Run(root, append(append([]string{}, ed...), p))
		if err != nil {
			return changes, fmt.Errorf("could not open %s: %w (set $EDITOR to choose another editor)", ed[0], err)
		}
		if r.Failed() {
			return changes, fmt.Errorf("%s exited with code %d; keeping the original proposal", ed[0], r.ExitCode)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return changes, err
		}
		c.New = string(data)
		if !c.Created && c.New == c.Old {
			ui.Muted(c.Path + ": no changes left, dropping it.")
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// apply writes the fix and runs its commands, returning the snapshot id.
func apply(root string, prop *patch.Proposal, changes []patch.Change) (string, error) {
	id := ""
	if len(changes) > 0 {
		var err error
		if id, err = snapshot.Save(root, changes, summarize(prop, changes)); err != nil {
			return "", fmt.Errorf("could not save undo snapshot, nothing changed: %w", err)
		}
		if err := patch.Write(root, changes); err != nil {
			return id, err
		}
		ui.Success(fmt.Sprintf("Updated %d file(s)", len(changes)))
	}
	for _, c := range prop.Commands {
		ui.Header("Running " + c)
		r, err := runner.Run(root, []string{c})
		if err != nil {
			return id, err
		}
		if r.Failed() {
			return id, fmt.Errorf("%q failed with exit code %d", c, r.ExitCode)
		}
	}
	return id, nil
}

func show(p *patch.Proposal, changes []patch.Change, notes []note) {
	if p.Explanation != "" {
		ui.Explanation(p.Explanation)
	}
	for _, c := range changes {
		ui.Diff(c.Diff())
	}
	for _, c := range p.Commands {
		ui.Command(c)
	}
	for _, n := range notes {
		ui.Check(n.ok, n.text)
	}
}

func userPrompt(c *gather.Context, applied, memory []string) string {
	s := c.Prompt()
	if len(memory) > 0 {
		s += "\n## Earlier attempts at this same error (oldest first)\n- " + strings.Join(memory, "\n- ") + "\n"
	}
	if len(applied) > 0 {
		s += "\n## Fixes already applied this session (they did not fully solve it)\n- " + strings.Join(applied, "\n- ") + "\n"
	}
	return s
}

func summarize(p *patch.Proposal, changes []patch.Change) string {
	if s := p.Explanation; s != "" {
		if len(s) > 120 {
			s = s[:120] + "..."
		}
		return s
	}
	return fmt.Sprintf("edited %d file(s), ran %d command(s)", len(changes), len(p.Commands))
}

func paths(changes []patch.Change) []string {
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = filepath.ToSlash(c.Path)
	}
	return out
}

// excerpt keeps the end of the log, redacted, as the journal's record of the error.
func excerpt(log string) string {
	lines := strings.Split(strings.TrimSpace(gather.Redact(log)), "\n")
	var kept []string
	for i := len(lines) - 1; i >= 0 && len(kept) < 12; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			kept = append([]string{t}, kept...)
		}
	}
	s := strings.Join(kept, "\n")
	if len(s) > 800 {
		s = s[len(s)-800:]
	}
	return s
}

func listApplied(applied []string) {
	if len(applied) == 0 {
		return
	}
	ui.Muted("Applied this session:")
	for _, a := range applied {
		ui.Muted("  • " + a)
	}
	ui.Muted("Run `lazy undo` to revert the most recent file change, or `lazy history` to see everything tried.")
}

// ConfirmRemote shows a one-time notice per cloud provider.
func ConfirmRemote(p provider.Provider) bool {
	dir, err := config.Dir()
	marker := filepath.Join(dir, "acknowledged-"+p.Name())
	if err == nil {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	ui.Warn(p.Name() + " is a cloud provider. LazyFinger will send the error output and relevant source files to it. " +
		"Obvious secrets are redacted and .env files are never sent.")
	if !ui.Confirm("Continue?") {
		return false
	}
	if err == nil {
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(marker, nil, 0o600)
	}
	return true
}
