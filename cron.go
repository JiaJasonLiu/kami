package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cron jobs: scheduled prompts that run periodically without anyone chatting.
// A job binds a standard 5-field cron expression to (agent, thread, prompt).
// Once a minute a background loop (cronLoop) wakes, finds every enabled job
// whose schedule matches the current minute, runs it as if the owner had typed
// the prompt to that agent, and posts the reply into the job's Telegram topic.
//
// Everything is in-process and standard-library only: there is no external
// scheduler, no os/exec, no privileged timer. Jobs persist in state/cron.json.

const cronFile = "cron.json"

// cronJob is one scheduled task. Agent + Thread capture the context in which it
// was created so the run reproduces it: the same agent's soul/tools/workspace,
// posting back into the same forum topic (0 = DM / General).
type cronJob struct {
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	Thread  int64  `json:"thread"`
	Spec    string `json:"spec"`
	Prompt  string `json:"prompt"`
	Enabled bool   `json:"enabled"`
}

var (
	cronMu   sync.Mutex
	cronJobs []cronJob
)

// loadCronJobs restores persisted jobs at startup. A missing or unreadable file
// simply means "no jobs yet".
func loadCronJobs() {
	cronMu.Lock()
	defer cronMu.Unlock()
	cronJobs = nil
	b, err := os.ReadFile(statePath(cronFile))
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &cronJobs)
}

// saveCronJobs persists the current job list. The caller must hold cronMu.
func saveCronJobs() error {
	b, _ := json.MarshalIndent(cronJobs, "", "  ")
	return os.WriteFile(statePath(cronFile), b, 0o600)
}

// cronSchedule is a parsed 5-field cron expression. Each field is a set of the
// values it matches. domStar/dowStar record whether day-of-month / day-of-week
// were "*", because standard cron treats a restriction on both as an OR.
type cronSchedule struct {
	min, hour, dom, month, dow map[int]bool
	domStar, dowStar           bool
}

// parseCronField parses a single cron field into the set of integers it matches,
// supporting "*", "a", "a-b", "*/n", "a-b/n" and comma-separated lists of those.
func parseCronField(field string, lo, hi int) (map[int]bool, bool, error) {
	set := map[int]bool{}
	star := false
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, false, fmt.Errorf("empty term in %q", field)
		}
		step := 1
		rng := part
		if i := strings.IndexByte(part, '/'); i >= 0 {
			rng = part[:i]
			s, err := strconv.Atoi(part[i+1:])
			if err != nil || s < 1 {
				return nil, false, fmt.Errorf("bad step in %q", part)
			}
			step = s
		}
		start, end := lo, hi
		switch {
		case rng == "*":
			star = true
		case strings.IndexByte(rng, '-') >= 0:
			bounds := strings.SplitN(rng, "-", 2)
			a, err1 := strconv.Atoi(bounds[0])
			b, err2 := strconv.Atoi(bounds[1])
			if err1 != nil || err2 != nil {
				return nil, false, fmt.Errorf("bad range in %q", part)
			}
			start, end = a, b
		default:
			v, err := strconv.Atoi(rng)
			if err != nil {
				return nil, false, fmt.Errorf("bad value in %q", part)
			}
			start, end = v, v
		}
		if start < lo || end > hi || start > end {
			return nil, false, fmt.Errorf("value out of range %d-%d in %q", lo, hi, part)
		}
		for v := start; v <= end; v += step {
			set[v] = true
		}
	}
	return set, star, nil
}

// parseCron parses a whole 5-field cron expression (minute hour dom month dow).
// Day-of-week accepts 0-7 with both 0 and 7 meaning Sunday.
func parseCron(spec string) (*cronSchedule, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron needs 5 fields (min hour day-of-month month day-of-week), got %d", len(fields))
	}
	var s cronSchedule
	var err error
	if s.min, _, err = parseCronField(fields[0], 0, 59); err != nil {
		return nil, err
	}
	if s.hour, _, err = parseCronField(fields[1], 0, 23); err != nil {
		return nil, err
	}
	if s.dom, s.domStar, err = parseCronField(fields[2], 1, 31); err != nil {
		return nil, err
	}
	if s.month, _, err = parseCronField(fields[3], 1, 12); err != nil {
		return nil, err
	}
	if s.dow, s.dowStar, err = parseCronField(fields[4], 0, 7); err != nil {
		return nil, err
	}
	if s.dow[7] {
		s.dow[0] = true
	}
	return &s, nil
}

// matches reports whether the schedule fires at time t (minute resolution).
// When both day-of-month and day-of-week are restricted, the job runs if
// EITHER matches — the standard Vixie-cron rule.
func (s *cronSchedule) matches(t time.Time) bool {
	if !s.min[t.Minute()] || !s.hour[t.Hour()] || !s.month[int(t.Month())] {
		return false
	}
	domOK := s.dom[t.Day()]
	dowOK := s.dow[int(t.Weekday())]
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowOK
	case s.dowStar:
		return domOK
	default:
		return domOK || dowOK
	}
}

// addCronJob validates the schedule, creates a job in the current chat context
// (active agent + current topic), persists it, and returns the new job.
func addCronJob(spec, prompt string) (cronJob, error) {
	spec = strings.TrimSpace(spec)
	if _, err := parseCron(spec); err != nil {
		return cronJob{}, err
	}
	if strings.TrimSpace(prompt) == "" {
		return cronJob{}, fmt.Errorf("missing prompt for the scheduled task")
	}
	job := cronJob{
		ID:      strconv.FormatInt(time.Now().UnixNano(), 36),
		Agent:   activeAgent,
		Thread:  currentTopic,
		Spec:    spec,
		Prompt:  prompt,
		Enabled: true,
	}
	cronMu.Lock()
	defer cronMu.Unlock()
	cronJobs = append(cronJobs, job)
	if err := saveCronJobs(); err != nil {
		return cronJob{}, err
	}
	return job, nil
}

// removeCronJob deletes the job with the given id, reporting whether it existed.
func removeCronJob(id string) (bool, error) {
	cronMu.Lock()
	defer cronMu.Unlock()
	out := cronJobs[:0]
	found := false
	for _, j := range cronJobs {
		if j.ID == id {
			found = true
			continue
		}
		out = append(out, j)
	}
	if !found {
		return false, nil
	}
	cronJobs = out
	return true, saveCronJobs()
}

// dueCronJobs returns a snapshot of the enabled jobs that fire at time t. It
// copies under the lock so the scheduler can run jobs without holding it.
func dueCronJobs(t time.Time) []cronJob {
	cronMu.Lock()
	defer cronMu.Unlock()
	var due []cronJob
	for _, j := range cronJobs {
		if !j.Enabled {
			continue
		}
		s, err := parseCron(j.Spec)
		if err != nil {
			continue
		}
		if s.matches(t) {
			due = append(due, j)
		}
	}
	return due
}

// runCronJob executes one job's prompt against its agent and posts the reply
// into its topic. It shares turnMu with the bot loop so a scheduled run never
// races a live message for the same agent's history/workspace.
func runCronJob(j cronJob) {
	log.Printf("cron %s -> agent %q (thread %d): %s", j.ID, j.Agent, j.Thread, truncate(j.Prompt, 80))
	reply := runAgentTurn(j.Agent, j.Thread, j.Prompt)
	tgSendToThread(cfg.TelegramChatID, j.Thread, "⏰ "+reply)
}

// cronLoop is the scheduler. It sleeps until the next minute boundary, runs any
// due jobs, and repeats forever. Jobs run in goroutines so one slow task can't
// delay the next tick, but turnMu still serialises their agent turns.
func cronLoop() {
	for {
		now := time.Now()
		next := now.Truncate(time.Minute).Add(time.Minute)
		time.Sleep(time.Until(next))
		for _, j := range dueCronJobs(time.Now()) {
			go runCronJob(j)
		}
	}
}

// tCronAdd is the tool handler for cron_add.
func tCronAdd(args map[string]interface{}) (string, error) {
	spec, err := argStr(args, "schedule")
	if err != nil {
		return "", err
	}
	prompt, err := argStr(args, "prompt")
	if err != nil {
		return "", err
	}
	job, err := addCronJob(spec, prompt)
	if err != nil {
		return "", err
	}
	where := "here"
	if job.Thread != 0 {
		where = fmt.Sprintf("topic %d", job.Thread)
	}
	return fmt.Sprintf("scheduled job %s: %q runs on cron %q as agent %q in %s", job.ID, truncate(job.Prompt, 60), job.Spec, job.Agent, where), nil
}

// tCronList is the tool handler for cron_list.
func tCronList(_ map[string]interface{}) (string, error) {
	cronMu.Lock()
	jobs := make([]cronJob, len(cronJobs))
	copy(jobs, cronJobs)
	cronMu.Unlock()
	if len(jobs) == 0 {
		return "no scheduled jobs", nil
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	var b strings.Builder
	for _, j := range jobs {
		state := "on"
		if !j.Enabled {
			state = "off"
		}
		fmt.Fprintf(&b, "%s [%s] cron %q agent=%s thread=%d: %s\n", j.ID, state, j.Spec, j.Agent, j.Thread, j.Prompt)
	}
	return strings.TrimSpace(b.String()), nil
}

// tCronRemove is the tool handler for cron_remove.
func tCronRemove(args map[string]interface{}) (string, error) {
	id, err := argStr(args, "id")
	if err != nil {
		return "", err
	}
	found, err := removeCronJob(strings.TrimSpace(id))
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("no job with id %q", id)
	}
	return "removed job " + id, nil
}
