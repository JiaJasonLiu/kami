package main

import (
	"strings"
	"testing"
)

// resetTopics gives each test a fresh $KAMI_HOME with the default agent
// active, no topic context, and empty bindings.
func resetTopics(t *testing.T) {
	t.Helper()
	home = t.TempDir()
	activeAgent = defaultAgent
	dmAgent = defaultAgent
	currentTopic = 0
	topicBindings = map[int64]string{}
	if err := ensureScaffold(); err != nil {
		t.Fatal(err)
	}
}

func TestSlugifyTopicName(t *testing.T) {
	cases := map[string]string{
		"Coding Help":   "coding-help",
		"  Notes!!  ":   "notes",
		"Deep_Thought":  "deep_thought",
		"UPPER CASE":    "upper-case",
		"a/b\\c":        "abc",
		"résumé":        "rsum",
		"multi   space": "multi-space",
	}
	for in, want := range cases {
		if got := slugifyTopicName(in, 7); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
	// A title with nothing usable falls back to a thread-scoped name.
	if got := slugifyTopicName("!!!", 42); got != "topic-42" {
		t.Errorf("slugify fallback = %q, want topic-42", got)
	}
	// Every slug this produces must itself be a valid agent name.
	for _, in := range []string{"Coding Help", "!!!", "x", "résumé", strings.Repeat("z", 80)} {
		if err := validAgentName(slugifyTopicName(in, 1)); err != nil {
			t.Errorf("slugify(%q) produced invalid agent name: %v", in, err)
		}
	}
}

func TestAgentForThreadRouting(t *testing.T) {
	resetTopics(t)
	// Thread 0 (DM / General) always follows the gateway-wide active agent.
	if got := agentForThread(0); got != defaultAgent {
		t.Errorf("thread 0 = %q, want %q", got, defaultAgent)
	}
	// An unbound topic falls back to the default agent.
	if got := agentForThread(99); got != defaultAgent {
		t.Errorf("unbound topic = %q, want %q", got, defaultAgent)
	}
	// A bound topic routes to its agent.
	if _, _, err := onForumTopicCreated(99, "Coding Help"); err != nil {
		t.Fatal(err)
	}
	if got := agentForThread(99); got != "coding-help" {
		t.Errorf("bound topic = %q, want coding-help", got)
	}
}

func TestOnForumTopicCreated(t *testing.T) {
	resetTopics(t)
	name, created, err := onForumTopicCreated(5, "Scribe")
	if err != nil {
		t.Fatal(err)
	}
	if name != "scribe" || !created {
		t.Fatalf("got (%q,%v), want (scribe,true)", name, created)
	}
	if !agentExists("scribe") {
		t.Error("agent was not created")
	}
	// A second topic reusing the same slug binds to the existing agent
	// instead of erroring on a duplicate create.
	name2, created2, err := onForumTopicCreated(6, "scribe")
	if err != nil {
		t.Fatal(err)
	}
	if name2 != "scribe" || created2 {
		t.Fatalf("got (%q,%v), want (scribe,false)", name2, created2)
	}
	if topicBindings[6] != "scribe" {
		t.Errorf("thread 6 not bound to scribe: %v", topicBindings)
	}
}

func TestTopicBindingsPersist(t *testing.T) {
	resetTopics(t)
	if _, _, err := onForumTopicCreated(11, "Coder"); err != nil {
		t.Fatal(err)
	}
	// Simulate a restart: drop the in-memory map and reload from disk.
	topicBindings = map[int64]string{}
	loadTopicBindings()
	if topicBindings[11] != "coder" {
		t.Errorf("binding did not persist: %v", topicBindings)
	}
	// A binding to a since-deleted agent must be dropped on reload.
	topicBindings[12] = "ghost"
	saveTopicBindings()
	topicBindings = map[int64]string{}
	loadTopicBindings()
	if _, ok := topicBindings[12]; ok {
		t.Error("binding to nonexistent agent survived reload")
	}
}

func TestUseCommandBindsTopicNotGlobal(t *testing.T) {
	resetTopics(t)
	if err := createAgent("coder", ""); err != nil {
		t.Fatal(err)
	}
	// Inside a topic, /agent use binds THAT topic and must not change the
	// gateway-wide active agent or the persisted agent.txt.
	currentTopic = 21
	out := handleUserMessage("/agent use coder")
	if !strings.Contains(out, "this topic") {
		t.Errorf("expected topic-scoped reply, got %q", out)
	}
	if topicBindings[21] != "coder" {
		t.Errorf("topic 21 not bound to coder: %v", topicBindings)
	}
	// Back in a DM, the active agent is still the default.
	currentTopic = 0
	if agentForThread(0) != defaultAgent {
		t.Errorf("global active agent was clobbered: %q", agentForThread(0))
	}
}

func TestDeleteUnbindsTopics(t *testing.T) {
	resetTopics(t)
	if _, _, err := onForumTopicCreated(31, "Victim"); err != nil {
		t.Fatal(err)
	}
	// The topic's own agent is active in-topic, so deleting it must be done
	// from a context where it isn't active. Point the topic elsewhere first.
	if err := createAgent("keeper", ""); err != nil {
		t.Fatal(err)
	}
	currentTopic = 31
	if _, err := assignActiveAgent("keeper"); err != nil {
		t.Fatal(err)
	}
	currentTopic = 0
	if err := deleteAgent("victim"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// keeper remains bound; victim's stale bindings are gone.
	for id, name := range topicBindings {
		if name == "victim" {
			t.Errorf("thread %d still bound to deleted agent", id)
		}
	}
}
