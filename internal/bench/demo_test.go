package bench

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDemoRunsTrustedFixtureThroughOfflineGateway(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("trusted offline checker requires host python3")
	}
	store, _ := cliStore(t)
	result, err := RunDemo(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if result["simulation"] != true || result["status"] != "completed" {
		t.Fatalf("demo is not a completed simulation: %#v", result)
	}
	evaluation := object(result["evaluation"])
	if number(evaluation["earned_score"]) != 5 || number(evaluation["score_percent"]) != 100 || number(evaluation["passed_percent"]) != 100 {
		t.Fatalf("trusted checker did not produce full score: %#v", evaluation)
	}
	metrics := object(result["metrics"])
	if number(metrics["request_count"]) != 1 || number(metrics["cost_usd"]) != 0 {
		t.Fatalf("demo must make exactly one zero-cost mock request: %#v", metrics)
	}
	warnings, ok := result["warnings"].([]any)
	if !ok || len(warnings) == 0 || warnings[len(warnings)-1] != "Offline simulation: no Docker, real agent, harness, or paid provider was exercised." {
		t.Fatalf("offline warning changed: %#v", result["warnings"])
	}
	stored, err := store.Get(text(result["id"]))
	if err != nil || stored["simulation"] != true {
		t.Fatalf("simulation marker not retained: %#v err=%v", stored, err)
	}
	runDir, err := store.RunDir(text(result["id"]))
	if err != nil {
		t.Fatal(err)
	}
	definitionPath := filepath.Join(runDir, "definition.json")
	definitionBytes, err := os.ReadFile(definitionPath)
	if err != nil {
		t.Fatalf("demo definition was not retained: %v", err)
	}
	var definition Record
	if err := json.Unmarshal(definitionBytes, &definition); err != nil || object(definition["task"]) == nil {
		t.Fatalf("invalid retained definition %q (%v)", definitionBytes, err)
	}
}

func TestDemoRuntimeRejectsNonFixtureEvaluationCommands(t *testing.T) {
	runtime := &demoRuntime{containers: map[string]demoContainer{}}
	if _, err := runtime.Start(context.Background(), "placeholder", t.TempDir(), StartOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := runtime.Exec(context.Background(), "demo-1", []string{"sh", "-c", "echo arbitrary"}, 0)
	if err == nil || !strings.Contains(err.Error(), "only its fixed trusted evaluator") {
		t.Fatalf("demo runtime accepted an unsupported command: %v", err)
	}
}
