package bench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func testCampaign(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "input.csv"), []byte("a,b\r\n1,2\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	evaluation := map[string]any{"tests": []any{map[string]any{"id": "answer", "score": 3, "command": []any{"python", "-c", "pass"}}}}
	task := map[string]any{"id": "sum", "prompt": "Add numbers", "image": "python:3.13", "evaluation": evaluation,
		"attachments": []any{map[string]any{"source": "input.csv", "target": "data/input.csv"}}}
	data := map[string]any{"version": 1, "models": []any{"vendor/a", "vendor/b"}, "trials": 2,
		"agents": []any{map[string]any{"id": "pi", "kind": "pi"}}, "tasks": []any{task}}
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "campaign.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCampaignDefaultsAndMatrix(t *testing.T) {
	path := testCampaign(t)
	campaign, err := LoadCampaign(path)
	if err != nil {
		t.Fatal(err)
	}
	runs := ExpandRuns(campaign)
	if len(runs) != 4 {
		t.Fatalf("runs=%d", len(runs))
	}
	task := object(runs[0]["task"])
	eval := object(task["evaluation"])
	if text(eval["image"]) != "python:3.13" || number(eval["timeout_seconds"]) != 60 {
		t.Fatal(eval)
	}
	source := text(object(array(task["attachments"])[0])["source"])
	if source != filepath.Join(filepath.Dir(path), "input.csv") {
		t.Fatal(source)
	}
	object(runs[0]["task"])["prompt"] = "changed"
	if text(object(runs[1]["task"])["prompt"]) != "Add numbers" {
		t.Fatal("run specs share mutable state")
	}
}

func TestCampaignRejectsInvalidInputs(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value any
	}{
		{"version", true}, {"version", 1.0}, {"concurrency", 0}, {"trials", true}, {"models", []any{}}, {"extra", 1},
	} {
		t.Run(tt.name+string(mustJSON(tt.value)), func(t *testing.T) {
			path := testCampaign(t)
			b, _ := os.ReadFile(path)
			data := Record{}
			_ = json.Unmarshal(b, &data)
			data[tt.name] = tt.value
			// An integer JSON token is distinct from a decimal token in version/trial fields.
			b = mustJSON(data)
			if tt.name == "version" {
				if v, ok := tt.value.(float64); ok && v == 1 {
					b = []byte(`{"version":1.0,"models":[],"tasks":[],"agents":[]}`)
				}
			}
			_ = os.WriteFile(path, b, 0600)
			if _, err := LoadCampaign(path); err == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
	for _, target := range []string{"../escape", "/absolute", ".git/config", "a/../../b", "a\\b"} {
		if _, err := RelativePath(target); err == nil {
			t.Errorf("accepted %q", target)
		}
	}
}

func TestWeightedScoringAndUnknownUsage(t *testing.T) {
	scored, err := ScoreTests([]Record{{"id": "a", "score": 2}, {"id": "b", "score": 8}}, []Record{{"id": "a", "status": "passed"}, {"id": "b", "status": "failed"}})
	if err != nil {
		t.Fatal(err)
	}
	if number(scored["score_percent"]) != 20 || number(scored["passed_percent"]) != 50 {
		t.Fatal(scored)
	}
	agg := Aggregate([]Record{{"status": "completed", "evaluation": scored}, {"status": "infrastructure_failed", "evaluation": Record{"score_percent": 100}}})
	if number(agg["mean_score_percent"]) != 20 {
		t.Fatal(agg)
	}
	if _, err := ScoreTests([]Record{{"id": "a", "score": 1}}, []Record{{"id": "a"}, {"id": "a"}}); err == nil {
		t.Fatal("duplicate accepted")
	}
	attempt := Record{"attempt_id": "a", "agent_id": "main", "input_tokens": 2, "output_tokens": 3, "cost_usd": 0.1}
	metrics := SummarizeAttempts([]Record{attempt, attempt, {"attempt_id": "b", "agent_id": "child", "error_kind": "network"}})
	if number(metrics["request_count"]) != 2 || metrics["cost_usd"] != nil || number(metrics["known_cost_usd"]) != 0.1 || metrics["accounting_complete"] != false {
		t.Fatal(metrics)
	}
}

func TestTimelineCountsActiveUnionAndChildren(t *testing.T) {
	clock := 0.0
	timeline := NewTimeline(func() float64 { return clock })
	for _, e := range []struct {
		at               float64
		kind, id, parent string
	}{
		{0, "agent_start", "main", ""}, {1, "agent_start", "child", "main"}, {2, "agent_wait", "main", ""}, {4, "agent_wait", "child", ""},
		{7, "agent_resume", "main", ""}, {8, "agent_end", "child", ""}, {10, "agent_end", "main", ""},
	} {
		clock = e.at
		if err := timeline.Event(Record{"type": e.kind, "agent_id": e.id, "parent_id": e.parent}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := timeline.Snapshot()
	if number(snapshot["agent_seconds"]) != 7 || number(snapshot["human_wait_seconds"]) != 3 || number(snapshot["cumulative_agent_seconds"]) != 8 {
		t.Fatal(snapshot)
	}
	clock = 9
	if err := timeline.Event(Record{"type": "agent_start"}); err == nil {
		t.Fatal("accepted non-monotonic event")
	}
}

func TestAttemptTotalsPreserveRequestOrder(t *testing.T) {
	// Python's ordered dict keeps the first position when an attempt is updated.
	// Retaining that order also avoids randomized floating-point addition in Go.
	values := []float64{0.1, 0.2, 0.3}
	expected := 0.0
	attempts := []Record{}
	for i, v := range values {
		expected += v
		attempts = append(attempts, Record{"attempt_id": string(rune('a' + i)), "agent_id": "main", "cost_usd": v, "input_tokens": 1, "output_tokens": 1})
	}
	for i := 0; i < 100; i++ {
		got := SummarizeAttempts(attempts)
		if number(got["cost_usd"]) != expected {
			t.Fatalf("non-reference summation order: got %.17g, want %.17g", number(got["cost_usd"]), expected)
		}
	}
}

func TestManifestErrorKeepsEmptyRequiredList(t *testing.T) {
	err := checkKeys(Record{"unexpected": true}, []string{"allowed"}, nil)
	if err == nil || err.Error() != "Expected object with keys ['allowed']; required []" {
		t.Fatalf("unexpected contract error: %v", err)
	}
}
