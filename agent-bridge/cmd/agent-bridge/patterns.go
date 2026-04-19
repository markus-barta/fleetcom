package main

import (
	"encoding/json"
	"regexp"
	"time"
)

// Parsed line from OpenClaw logs. Not all events have a turnID
// (typing events, config changes).
type parsedEvent struct {
	ts      string
	agent   string
	kind    string
	turnID  string
	payload json.RawMessage
}

// NOTE TO OPERATOR: OpenClaw's log format is not stable across
// releases. The patterns below are the reference set the bridge was
// designed against; adjust them for your installation if needed.
// Each pattern has its own purpose:
//
//   turnStartPat    — user message received, turn begins
//   toolInvokedPat  — subprocess or LLM tool started
//   toolDonePat     — subprocess/tool finished
//   repliedPat      — message successfully sent back to the user
//   erroredPat      — turn failed
//   typingPat       — typing indicator refreshed
//
// Each pattern must capture: agent name (named group "agent"), turn
// id ("turn"), and any payload fields the corresponding event kind
// declares in docs/AGENT-OBSERVABILITY.md.
//
// If a log line doesn't match any pattern, it's silently ignored.

var (
	turnStartPat = regexp.MustCompile(
		`turn\.started\s+agent=(?P<agent>\w+)\s+turn=(?P<turn>\S+)\s+chat=(?P<chat>\S+)(?:\s+name="(?P<name>[^"]*)")?`)
	toolInvokedPat = regexp.MustCompile(
		`turn\.tool_invoked\s+agent=(?P<agent>\w+)\s+turn=(?P<turn>\S+)\s+tool=(?P<tool>\S+)\s+name=(?P<name>\S+)(?:\s+target=(?P<target>\S+))?`)
	toolDonePat = regexp.MustCompile(
		`turn\.tool_completed\s+agent=(?P<agent>\w+)\s+turn=(?P<turn>\S+)\s+tool=(?P<tool>\S+)\s+exit=(?P<exit>-?\d+)\s+dur=(?P<dur>\d+)ms`)
	repliedPat = regexp.MustCompile(
		`turn\.replied\s+agent=(?P<agent>\w+)\s+turn=(?P<turn>\S+)\s+dur=(?P<dur>\d+)ms(?:\s+tok=(?P<tokin>\d+)/(?P<tokout>\d+))?`)
	erroredPat = regexp.MustCompile(
		`turn\.errored\s+agent=(?P<agent>\w+)\s+turn=(?P<turn>\S+)\s+class=(?P<class>\S+)`)
	typingPat = regexp.MustCompile(
		`typing\.refreshed\s+agent=(?P<agent>\w+)\s+chat=(?P<chat>\S+)\s+exp=(?P<exp>\S+)`)
)

// parseLine runs all patterns against a log line and returns the first
// match converted to a parsedEvent. Returns ok=false when nothing
// matches. Timestamp is always "now" — the bridge trusts its wall clock
// over whatever timestamp the log line might carry.
func parseLine(line string) (parsedEvent, bool) {
	now := time.Now().UTC().Format(time.RFC3339)

	if m := namedGroups(turnStartPat, line); m != nil {
		pb, _ := json.Marshal(map[string]any{
			"chat_id":   m["chat"],
			"chat_name": m["name"],
		})
		return parsedEvent{ts: now, agent: m["agent"], kind: "turn.started", turnID: m["turn"], payload: pb}, true
	}
	if m := namedGroups(toolInvokedPat, line); m != nil {
		pb, _ := json.Marshal(map[string]any{
			"tool_id": m["tool"],
			"name":    m["name"],
			"target":  m["target"],
		})
		return parsedEvent{ts: now, agent: m["agent"], kind: "turn.tool-invoked", turnID: m["turn"], payload: pb}, true
	}
	if m := namedGroups(toolDonePat, line); m != nil {
		pb, _ := json.Marshal(map[string]any{
			"tool_id":     m["tool"],
			"exit_code":   atoi(m["exit"]),
			"duration_ms": atoi(m["dur"]),
		})
		return parsedEvent{ts: now, agent: m["agent"], kind: "turn.tool-completed", turnID: m["turn"], payload: pb}, true
	}
	if m := namedGroups(repliedPat, line); m != nil {
		pb, _ := json.Marshal(map[string]any{
			"duration_ms":       atoi(m["dur"]),
			"tokens_prompt":     atoi(m["tokin"]),
			"tokens_completion": atoi(m["tokout"]),
		})
		return parsedEvent{ts: now, agent: m["agent"], kind: "turn.replied", turnID: m["turn"], payload: pb}, true
	}
	if m := namedGroups(erroredPat, line); m != nil {
		pb, _ := json.Marshal(map[string]any{"class": m["class"]})
		return parsedEvent{ts: now, agent: m["agent"], kind: "turn.errored", turnID: m["turn"], payload: pb}, true
	}
	if m := namedGroups(typingPat, line); m != nil {
		pb, _ := json.Marshal(map[string]any{
			"chat_id":    m["chat"],
			"expires_at": m["exp"],
		})
		return parsedEvent{ts: now, agent: m["agent"], kind: "typing.refreshed", payload: pb}, true
	}
	return parsedEvent{}, false
}

func namedGroups(re *regexp.Regexp, s string) map[string]string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(re.SubexpNames()))
	for i, n := range re.SubexpNames() {
		if n == "" || i >= len(m) {
			continue
		}
		out[n] = m[i]
	}
	return out
}

func atoi(s string) int {
	n := 0
	neg := false
	for i, c := range s {
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		return -n
	}
	return n
}
