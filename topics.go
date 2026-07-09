package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Forum topics: when the gateway is talked to inside a Telegram forum group
// (a supergroup with Topics enabled), each topic becomes a persistent
// conversation bound to its own agent profile. A message's
// message_thread_id identifies its topic; the binding below maps that thread
// to an agent, and everything downstream (SOUL.md, tools.json, history and
// the sandboxed workspace) already resolves through the active agent.
//
// Thread id 0 means "no topic" — a private chat, or the group's General
// topic. It is deliberately never stored here: it always routes to the
// gateway-wide active agent (agent.txt), so ordinary DMs behave exactly as
// they did before topics existed.

const topicsFile = "topics.json"

// currentTopic is the thread id of the message currently being handled. The
// bot loop sets it before each call into handleUserMessage; it stays 0 in
// DMs and in tests, so /agent commands keep their original global-switch
// behaviour there. Like activeAgent, it is only touched on the single
// bot-loop goroutine.
var currentTopic int64

// topicBindings maps a forum thread id to the agent that owns that topic.
// It is loaded once at startup and kept in sync with state/topics.json.
var topicBindings = map[int64]string{}

// loadTopicBindings restores the thread→agent map from disk, dropping any
// entry whose agent no longer exists so a deleted agent can't strand a topic.
func loadTopicBindings() {
	topicBindings = map[int64]string{}
	b, err := os.ReadFile(statePath(topicsFile))
	if err != nil {
		return
	}
	raw := map[string]string{}
	if json.Unmarshal(b, &raw) != nil {
		return
	}
	for k, name := range raw {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil || validAgentName(name) != nil || !agentExists(name) {
			continue
		}
		topicBindings[id] = name
	}
}

// saveTopicBindings persists the thread→agent map. Keys are stringified
// because JSON object keys must be strings. Errors are non-fatal: a failed
// write just means bindings won't survive a restart.
func saveTopicBindings() {
	raw := make(map[string]string, len(topicBindings))
	for id, name := range topicBindings {
		raw[strconv.FormatInt(id, 10)] = name
	}
	b, _ := json.MarshalIndent(raw, "", "  ")
	_ = os.WriteFile(statePath(topicsFile), b, 0o600)
}

// bindTopic points a forum thread at an agent, scaffolding the agent's files
// and persisting the mapping. Thread 0 is rejected — the General topic and
// DMs are handled through the global active agent, not a stored binding.
func bindTopic(thread int64, name string) error {
	if thread == 0 {
		return switchAgent(name)
	}
	if err := validAgentName(name); err != nil {
		return err
	}
	if err := scaffoldAgent(name); err != nil {
		return err
	}
	topicBindings[thread] = name
	saveTopicBindings()
	return nil
}

// unbindAgentTopics removes every topic binding pointing at name. It is
// called when an agent is deleted so no topic is left aimed at a ghost.
func unbindAgentTopics(name string) {
	changed := false
	for id, bound := range topicBindings {
		if bound == name {
			delete(topicBindings, id)
			changed = true
		}
	}
	if changed {
		saveTopicBindings()
	}
}

// agentForThread resolves which agent should handle a message on a given
// thread. Thread 0 (DM / General) uses the gateway-wide active agent; a
// bound topic uses its agent; an unrecognised topic falls back to the
// default agent until it is explicitly bound.
func agentForThread(thread int64) string {
	if thread == 0 {
		return dmAgent
	}
	if name, ok := topicBindings[thread]; ok && agentExists(name) {
		return name
	}
	return defaultAgent
}

// slugifyTopicName turns a human topic title ("Coding Help!") into a valid
// agent name ("coding-help"). It lowercases, maps any run of unsupported
// characters to a single hyphen, trims stray hyphens, and enforces the
// leading-alphanumeric / 32-char rules. When nothing usable survives it
// falls back to a thread-scoped name so a topic always gets a distinct agent.
func slugifyTopicName(name string, thread int64) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case r == '_' || r == '-':
			// Underscore and hyphen are valid in agent names — keep them.
			b.WriteRune(r)
			lastDash = false
		case r == ' ':
			// Collapse runs of whitespace into a single separating hyphen.
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		default:
			// Any other character (punctuation, accents, …) is dropped.
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 32 {
		slug = strings.Trim(slug[:32], "-")
	}
	if validAgentName(slug) != nil {
		return fmt.Sprintf("topic-%d", thread)
	}
	return slug
}

// onForumTopicCreated auto-provisions an agent for a freshly created topic:
// it derives an agent name from the topic title, creates that agent if it is
// new, and binds the topic to it. Returns the agent name and whether it was
// newly created, so the caller can greet the topic appropriately.
func onForumTopicCreated(thread int64, title string) (string, bool, error) {
	name := slugifyTopicName(title, thread)
	created := false
	if !agentExists(name) {
		if err := createAgent(name, fmt.Sprintf("You are the assistant for the %q topic.", title)); err != nil {
			return "", false, err
		}
		created = true
	}
	if err := bindTopic(thread, name); err != nil {
		return "", false, err
	}
	return name, created, nil
}
