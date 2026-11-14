package bench

import (
	"fmt"
	"sync"
	"time"
)

type Timeline struct {
	mu                 sync.Mutex
	clock              func() float64
	last, active, wait float64
	agents             map[string]Record
}

func NewTimeline(clock func() float64) *Timeline {
	if clock == nil {
		start := time.Now()
		clock = func() float64 { return time.Since(start).Seconds() }
	}
	return &Timeline{clock: clock, last: clock(), agents: map[string]Record{}}
}
func (t *Timeline) tick(now float64) error {
	if now < t.last {
		return fmt.Errorf("Timeline must be monotonic")
	}
	delta := now - t.last
	active, waiting := false, false
	for _, agent := range t.agents {
		switch agent["state"] {
		case "active":
			active = true
			agent["agent_seconds"] = number(agent["agent_seconds"]) + delta
		case "waiting":
			waiting = true
			agent["human_wait_seconds"] = number(agent["human_wait_seconds"]) + delta
		}
	}
	if active {
		t.active += delta
	} else if waiting {
		t.wait += delta
	}
	t.last = now
	return nil
}
func (t *Timeline) Event(event Record) error {
	state, ok := map[string]string{"agent_start": "active", "agent_resume": "active", "agent_wait": "waiting", "agent_end": "ended"}[text(event["type"])]
	if !ok {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.tick(t.clock()); err != nil {
		return err
	}
	id := defaultText(event, "agent_id", "main")
	if t.agents[id] == nil {
		t.agents[id] = Record{"parent_id": event["parent_id"], "state": "ended", "agent_seconds": 0.0, "human_wait_seconds": 0.0}
	}
	t.agents[id]["state"] = state
	return nil
}
func (t *Timeline) Snapshot() Record {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.tick(t.clock()); err != nil {
		return Record{"error": err.Error()}
	}
	agents := Record{}
	total := 0.0
	for id, agent := range t.agents {
		agents[id] = cloneRecord(agent)
		total += number(agent["agent_seconds"])
	}
	return Record{"agent_seconds": t.active, "human_wait_seconds": t.wait, "cumulative_agent_seconds": total, "agents": agents}
}
func (t *Timeline) Finish() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.tick(t.clock()); err != nil {
		return err
	}
	for _, a := range t.agents {
		a["state"] = "ended"
	}
	return nil
}
func SummarizeAttempts(attempts []Record) Record {
	unique := map[string]Record{}
	for _, a := range attempts {
		unique[text(a["attempt_id"])] = a
	}
	values := []Record{}
	groups := map[string][]Record{}
	for _, a := range unique {
		values = append(values, a)
		id := defaultText(a, "agent_id", "unattributed")
		groups[id] = append(groups[id], a)
	}
	total := func(rows []Record) Record {
		r := Record{"request_count": len(rows), "accounting_complete": true}
		for _, key := range []string{"input_tokens", "output_tokens", "cost_usd"} {
			sum := 0.0
			complete := true
			for _, row := range rows {
				if row[key] == nil {
					complete = false
				} else {
					sum += number(row[key])
				}
			}
			r["known_"+key] = sum
			if complete {
				r[key] = sum
			} else {
				r[key] = nil
				r["accounting_complete"] = false
			}
		}
		failures := 0
		for _, row := range rows {
			if row["error_kind"] == "network" {
				failures++
			}
		}
		r["network_failures"] = failures
		return r
	}
	r := total(values)
	agents := Record{}
	for id, rows := range groups {
		agents[id] = total(rows)
	}
	r["agents"] = agents
	_, unknown := groups["unattributed"]
	r["attribution_complete"] = !unknown
	return r
}
