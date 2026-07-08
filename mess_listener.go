package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"breeze/internal/engine"
)

// messInboundMessage mirrors the fields of mess's own Message type (proto.go)
// that breeze's chat-command listener actually needs.
type messInboundMessage struct {
	ID    string `json:"id"`
	From  string `json:"from"`
	Topic string `json:"topic"`
	Kind  string `json:"kind"`
	Body  string `json:"body"`
}

// commandPrefix is deliberately exact and mention-shaped — a strong, hard-to-
// trigger-by-accident marker, not a bare "approve" that might appear in normal
// chat. It also happens to be mess's own @-mention syntax, so an
// "@breeze approve ..." message also wakes any human/agent watching the topic
// for @breeze mentions — appropriate, since it's about to take a real action.
const commandPrefix = "@breeze approve "

// runMessCommandListener subscribes to every registered pipeline's CommandTopic
// (computed ONCE, at daemon startup — see Pipeline.CommandTopic's doc comment:
// this deliberately does not react to a CommandTopic added/changed after the
// daemon is already running) and processes "@breeze approve ..." messages on
// those topics as chat-triggered stage approvals — see engine.md's design notes
// for the authorization model (the mess sender identity is trusted, mapped back
// to a breeze identity via MessTarget, and normal RBAC still applies). Runs
// until ctx is cancelled (daemon shutdown); reconnects on any `mess listen`
// disconnect (e.g. the mess daemon itself restarting) rather than giving up.
// Best-effort like the rest of breeze's mess integration: if `mess` isn't
// installed, or no pipeline has a CommandTopic, this silently does nothing.
func runMessCommandListener(ctx context.Context, eng *engine.Engine, stateDir string) {
	messPath, err := exec.LookPath("mess")
	if err != nil {
		return
	}
	topics := commandTopics(eng)
	if len(topics) == 0 {
		return
	}
	identity := daemonMessIdentity(stateDir)
	for _, topic := range topics {
		subCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := exec.CommandContext(subCtx, messPath, "sub", topic, "--as", identity).Run(); err != nil {
			log.Printf("mess command listener: failed to subscribe to %s: %v", topic, err)
		}
		cancel()
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := messListenOnce(ctx, messPath, identity, eng); err != nil && ctx.Err() == nil {
			log.Printf("mess command listener: %v; reconnecting in 5s", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// daemonMessIdentity derives a stable, dedicated mess identity for THIS repo's
// breeze daemon — deliberately NOT ambient/inherited (unlike the rest of
// breeze's mess integration, e.g. notifyViaMess). A real collision was found
// live: an ambient identity resolution landed on the exact same mess identity as
// the interactive session that happened to auto-start the daemon, silently
// triggering mess's own "two listeners on one identity — a message wakes only
// one of them" race (see mess's own warnIfAlreadyListening) between the daemon's
// listener and that session's own auto-wake hook listener. A name derived from
// the state directory's own path is stable across restarts and unique per repo,
// with no risk of colliding with any human/agent's own identity.
func daemonMessIdentity(stateDir string) string {
	sum := sha256.Sum256([]byte(stateDir))
	return "breeze-daemon-" + hex.EncodeToString(sum[:4])
}

// messListenOnce runs one `mess listen --json` subprocess to completion (until it
// exits, is killed by ctx cancellation, or the mess daemon it's talking to
// disconnects) — the caller loops this for reconnect-on-disconnect resilience.
func messListenOnce(ctx context.Context, messPath, identity string, eng *engine.Engine) error {
	cmd := exec.CommandContext(ctx, messPath, "listen", "--as", identity, "--json")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("opening stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting `mess listen`: %w", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		var m messInboundMessage
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			continue // not a message line we understand — ignore, never crash the listener
		}
		handleMessCommand(eng, messPath, identity, m)
	}
	if err := scanner.Err(); err != nil {
		cmd.Wait()
		return fmt.Errorf("reading `mess listen` output: %w", err)
	}
	return cmd.Wait()
}

// commandTopics returns the distinct, non-empty CommandTopic values across every
// currently-registered pipeline.
func commandTopics(eng *engine.Engine) []string {
	seen := map[string]bool{}
	var topics []string
	for _, p := range eng.Pipelines() {
		if p.CommandTopic != "" && !seen[p.CommandTopic] {
			seen[p.CommandTopic] = true
			topics = append(topics, p.CommandTopic)
		}
	}
	return topics
}

// normalizeMessTopic strips a leading "#" — mess's own daemon does the same
// server-side (see mess/daemon.go) before storing/delivering a topic message, so
// a Message.Topic value never has one regardless of whether "#name" or "name"
// was passed to `mess sub`/`mess pub`. CommandTopic follows notify_topic's
// existing "#name" authoring convention, so this normalization is needed
// wherever a configured CommandTopic is compared against an inbound m.Topic.
func normalizeMessTopic(topic string) string {
	return strings.TrimPrefix(topic, "#")
}

// handleMessCommand processes one inbound mess message — a no-op unless it's a
// topic message whose body starts with the exact commandPrefix. Every rejection
// path replies in the topic (threaded off the triggering message) explaining
// why, rather than silently dropping a command someone deliberately issued.
// identity is the daemon's own dedicated mess identity (see daemonMessIdentity) —
// used for the reply so it's never sent under some other ambient identity.
func handleMessCommand(eng *engine.Engine, messPath, identity string, m messInboundMessage) {
	if m.Kind != "topic" || !strings.HasPrefix(m.Body, commandPrefix) {
		return
	}
	reply := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		runMessBestEffort(messPath, "pub", m.Topic, msg, "--thread", m.ID, "--as", identity)
	}

	pipelineName, stageName, commit, environment, brief, err := parseApproveCommand(m.Body)
	if err != nil {
		reply("breeze: %v", err)
		return
	}

	p, ok := eng.Pipeline(pipelineName)
	if !ok {
		reply("breeze: pipeline %q not found", pipelineName)
		return
	}
	// Defense in depth: the command names its own target pipeline explicitly, but
	// still must be issued on THAT pipeline's own configured CommandTopic, not
	// merely some topic breeze happens to be subscribed to for a DIFFERENT
	// pipeline — otherwise pipeline A's topic could be used to approve pipeline
	// B's stage. normalizeMessTopic strips a leading "#": mess's daemon strips it
	// server-side before storing/delivering a message (m.Topic never has one),
	// but CommandTopic follows notify_topic's existing "#name" authoring
	// convention, so the two must be normalized the same way before comparing.
	if normalizeMessTopic(p.CommandTopic) != m.Topic {
		reply("breeze: pipeline %q is not configured to accept commands on this topic", pipelineName)
		return
	}

	actor, ok := identityForMessSender(eng, m.From)
	if !ok {
		reply("breeze: no breeze identity is mapped to mess agent %q (see --mess-agent on `identity register`) — command ignored", m.From)
		return
	}

	// The brief is annotated, not the audit event itself — Approval.Brief is
	// already the human-readable place operators look for context, and this
	// keeps the chat-triggered path from needing any new engine-level plumbing:
	// it's authorized and recorded through the exact same ApproveStage RBAC/audit
	// path a CLI-issued `stage approve` goes through, just with a marked actor.
	fullBrief := fmt.Sprintf("(via mess from %s) %s", m.From, brief)
	inst, err := eng.ApproveStage(pipelineName, stageName, commit, environment, actor, fullBrief)
	if err != nil {
		reply("breeze: approval by %s rejected: %v", actor, err)
		return
	}
	reply("breeze: %s/%s (%s) approved by %s -> %s", pipelineName, stageName, shortCommitForDisplay(commit), actor, inst.Status)
}

// identityForMessSender reverses Identity.MessTarget() (breeze identity -> mess
// agent name, used for OUTBOUND notifications) to answer the INBOUND question:
// which breeze identity, if any, claims this mess sender as its own mapped agent
// name (or raw identity name, when no explicit --mess-agent mapping was set).
func identityForMessSender(eng *engine.Engine, sender string) (string, bool) {
	for _, id := range eng.Identities() {
		if id.MessTarget() == sender {
			return id.Name, true
		}
	}
	return "", false
}

// parseApproveCommand parses the text after commandPrefix:
// "<pipeline>/<stage> <commit> [--env NAME] [--brief free text to end of line]".
// Deliberately simple (whitespace-split, no quoting) — this is a chat command,
// not a shell: --brief, if present, must be the LAST token and consumes
// everything after it verbatim, so a reviewer can write a normal free-text
// reason without needing to quote it.
func parseApproveCommand(body string) (pipelineName, stageName, commit, environment, brief string, err error) {
	fields := strings.Fields(strings.TrimPrefix(body, commandPrefix))
	if len(fields) < 2 {
		return "", "", "", "", "", fmt.Errorf(`usage: @breeze approve <pipeline>/<stage> <commit> [--env NAME] [--brief text...]`)
	}
	parts := strings.SplitN(fields[0], "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", "", "", fmt.Errorf("invalid <pipeline>/<stage> %q — expected \"pipeline/stage\"", fields[0])
	}
	pipelineName, stageName = parts[0], parts[1]
	commit = fields[1]

	i := 2
	for i < len(fields) {
		switch fields[i] {
		case "--env":
			i++
			if i >= len(fields) {
				return "", "", "", "", "", fmt.Errorf("--env requires a value")
			}
			environment = fields[i]
			i++
		case "--brief":
			brief = strings.Join(fields[i+1:], " ")
			i = len(fields)
		default:
			return "", "", "", "", "", fmt.Errorf("unrecognized argument %q", fields[i])
		}
	}
	return pipelineName, stageName, commit, environment, brief, nil
}
