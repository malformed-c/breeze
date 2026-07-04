package engine

import (
	"fmt"
	"strings"
	"time"
)

// SetBriefFn wires the callback fired (best-effort) whenever a stage instance
// resolves in a pipeline with BriefsDir configured. All stages for the same
// (pipeline, commit, environment) share ONE file — a running changelog of the
// pipeline's whole journey for that commit — so the daemon APPENDS a per-stage
// section to it (writing the one-time document header first if the file doesn't
// exist yet), rather than creating a new file per stage. Engine computes the
// filename/header/section (pure, no file I/O here, matching the split used for
// onChange/audit); if unset, recordBrief is simply a no-op. Briefs are a convenience
// artifact, never load-bearing — a write failure must never block or fail the stage
// resolution that triggered it (that's the audit log's job).
func (e *Engine) SetBriefFn(fn func(dir, filename, header, section string)) {
	e.mu.Lock()
	e.briefFn = fn
	e.mu.Unlock()
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// recordBrief must be called WITHOUT e.mu held (the callback does file I/O). No-op if
// briefsDir is empty (feature disabled for this pipeline) or no callback is wired.
func (e *Engine) recordBrief(briefsDir string, inst *StageInstance) {
	if briefsDir == "" {
		return
	}
	e.mu.Lock()
	fn := e.briefFn
	e.mu.Unlock()
	if fn == nil {
		return
	}

	date := inst.FinishedAt
	if date.IsZero() {
		date = inst.StartedAt
	}
	envSuffix := ""
	title := inst.Key.Commit
	if inst.Key.Environment != "" {
		envSuffix = "-" + inst.Key.Environment
		title = fmt.Sprintf("%s (%s)", inst.Key.Commit, inst.Key.Environment)
	}
	// One file per (pipeline, commit, environment) — NOT per stage: every stage that
	// touches this commit (build, review, deploy, test, ...) appends its own section
	// to the same file, so the file reads as the commit's whole pipeline story.
	filename := fmt.Sprintf("%s-%s-%s%s.md", date.Format("2006-01-02"), inst.Pipeline, shortCommit(inst.Key.Commit), envSuffix)

	var header strings.Builder
	fmt.Fprintf(&header, "# %s — %s\n", inst.Pipeline, title)

	var section strings.Builder
	fmt.Fprintf(&section, "\n## %s — %s\n\n", inst.Stage, inst.Status)
	if inst.Actor != "" {
		fmt.Fprintf(&section, "- **Actor**: %s\n", inst.Actor)
	}
	if !inst.StartedAt.IsZero() {
		fmt.Fprintf(&section, "- **Started**: %s", inst.StartedAt.Format(time.RFC3339))
		if !inst.FinishedAt.IsZero() {
			fmt.Fprintf(&section, " — **Finished**: %s", inst.FinishedAt.Format(time.RFC3339))
		}
		section.WriteString("\n")
	}
	if inst.Status == StageSucceeded || inst.Status == StageFailed {
		fmt.Fprintf(&section, "- **Exit code**: %d\n", inst.ExitCode)
	}

	if len(inst.Approvals) > 0 {
		section.WriteString("\n### Approvals\n")
		for _, a := range inst.Approvals {
			fmt.Fprintf(&section, "- **%s** (%s) at %s", a.Identity, a.Role, a.At.Format(time.RFC3339))
			if a.Brief != "" {
				fmt.Fprintf(&section, ": %s", a.Brief)
			}
			section.WriteString("\n")
		}
	}

	if inst.Brief != "" {
		fmt.Fprintf(&section, "\n### Brief\n%s\n", inst.Brief)
	}
	if inst.Error != "" {
		fmt.Fprintf(&section, "\n### Error\n%s\n", inst.Error)
	}
	if tail := string(inst.Stdout) + string(inst.Stderr); tail != "" {
		if len(tail) > 2048 {
			tail = tail[len(tail)-2048:]
		}
		fmt.Fprintf(&section, "\n### Output (tail)\n```\n%s\n```\n", tail)
	}

	callBriefFnSafely(fn, briefsDir, filename, header.String(), section.String())
}

// callBriefFnSafely recovers from a panic in the brief callback — briefs are
// explicitly documented as a convenience artifact, never load-bearing, so a bug in
// the file-writing side must not be allowed to crash the daemon or abort the stage
// resolution that triggered it (unlike a stage's own main command, whose failure is
// legitimate data the caller needs to see).
func callBriefFnSafely(fn func(dir, filename, header, section string), dir, filename, header, section string) {
	defer func() { recover() }()
	fn(dir, filename, header, section)
}
