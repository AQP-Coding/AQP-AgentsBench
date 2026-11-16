package bench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStorePersistsRunsEventsAndQuestions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.Save(Record{"id": "run1", "status": "running"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Event("run1", Record{"type": "message", "text": "hello"}); err != nil {
		t.Fatal(err)
	}
	requestID, err := store.Question("run1", "Continue?", Record{"agent_id": "main"})
	if err != nil {
		t.Fatal(err)
	}
	questions, err := store.Questions("run1")
	if err != nil || len(questions) != 1 || questions[0]["question"] != "Continue?" {
		t.Fatalf("pending questions = %#v, err = %v", questions, err)
	}
	if ok, err := store.Answer("run1", requestID, "yes"); err != nil || !ok {
		t.Fatalf("first answer = %v, %v", ok, err)
	}
	if ok, err := store.Answer("run1", requestID, "yes"); err != nil || ok {
		t.Fatalf("duplicate answer = %v, %v", ok, err)
	}
	response, found, err := store.Response("run1", requestID)
	if err != nil || !found || response != "yes" {
		t.Fatalf("response = %#v, found = %v, err = %v", response, found, err)
	}
	interventions, err := store.Interventions("run1")
	if err != nil || interventions != 1 {
		t.Fatalf("interventions = %d, err = %v", interventions, err)
	}
	events, err := store.Events("run1")
	if err != nil || len(events) != 1 || events[0]["text"] != "hello" || events[0]["timestamp"] == nil {
		t.Fatalf("events = %#v, err = %v", events, err)
	}
	transcript, err := os.ReadFile(filepath.Join(root, "runs", "run1", "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var transcriptEvent Record
	if err := json.Unmarshal(transcript, &transcriptEvent); err != nil || transcriptEvent["text"] != "hello" {
		t.Fatalf("transcript event = %#v, err = %v", transcriptEvent, err)
	}
	if err := store.Stop("run1"); err != nil {
		t.Fatal(err)
	}
	if stopped, err := store.StopRequested("run1"); err != nil || !stopped {
		t.Fatalf("stop requested = %v, err = %v", stopped, err)
	}

	reopened, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Get("run1")
	if err != nil || got["status"] != "running" {
		t.Fatalf("persisted run = %#v, err = %v", got, err)
	}
	listed, err := reopened.List()
	if err != nil || len(listed) != 1 || listed[0]["id"] != "run1" {
		t.Fatalf("runs = %#v, err = %v", listed, err)
	}
}

func TestStoreAnswersQuestionAtomicallyAcrossInstances(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	first, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := first.Save(Record{"id": "run1"}); err != nil {
		t.Fatal(err)
	}
	requestID, err := first.Question("run1", "Continue?", Record{})
	if err != nil {
		t.Fatal(err)
	}
	stores := []*Store{first, second}
	answers := []string{"left", "right"}
	results := make([]bool, 2)
	errors := make([]error, 2)
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errors[i] = stores[i].Answer("run1", requestID, answers[i])
		}(i)
	}
	wg.Wait()
	if errors[0] != nil || errors[1] != nil || results[0] == results[1] {
		t.Fatalf("answer results = %v, errors = %v", results, errors)
	}
}

func TestStoreRunDirectoryRejectsInvalidIDsAndSymlinkParents(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.RunDir("../../outside"); err == nil {
		t.Fatal("invalid run identifier was accepted")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "runs")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := store.RunDir("run1"); err == nil {
		t.Fatal("symlink artifact directory was accepted")
	}
}
