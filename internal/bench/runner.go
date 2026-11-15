package bench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RunGateway is the small lifecycle surface the runner needs from a run-scoped
// model gateway. Keeping it injectable makes the entire run path testable
// without a provider or a listening socket.
type RunGateway struct {
	// Token is the local capability passed to isolated agent configuration.
	Token string
	// URL is the model endpoint reachable from the agent runtime.
	URL string
	// Attempts returns accounting snapshots; nil means no observed attempts.
	Attempts func() []Record
	// LimitReached reports whether known spend reached the cap; nil means false.
	LimitReached func() bool
	// InfrastructureFailed reports whether a gateway failure occurred; nil means false.
	InfrastructureFailed func() bool
	// Close releases gateway resources; nil means no cleanup is needed.
	Close func() error
}

// RunOptions configures campaign execution and its replaceable external edges.
type RunOptions struct {
	// APIKey is the explicitly selected provider credential, used only by the
	// gateway and redacted from persisted records, never passed into the container.
	APIKey string
	// RuntimeFactory defaults to NewDockerRuntime when nil.
	RuntimeFactory func() (Runtime, error)
	// GatewayFactory defaults to the real fixed-upstream gateway when nil.
	GatewayFactory func(string, *float64) (RunGateway, error)
	// AdapterFactory defaults to the configured Pi or OpenCode adapter when nil.
	AdapterFactory func(Runtime, string, []string, string) AgentAdapter
	// Notify emits operator messages; nil discards them.
	Notify func(string)
}

type runControl struct {
	mu       sync.Mutex
	cause    string
	stopping atomic.Bool
}

func (c *runControl) stop(reason string) bool {
	c.mu.Lock()
	if c.cause == "" && reason != "" {
		c.cause = reason
	}
	c.mu.Unlock()
	return c.stopping.CompareAndSwap(false, true)
}

func (c *runControl) reason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cause
}

// RunCampaign freezes campaign inputs once, executes bounded concurrent trials,
// and returns records in stable task/agent/model/harness/trial order.
func RunCampaign(ctx context.Context, campaign Record, store *Store, options RunOptions) ([]Record, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return nil, fmt.Errorf("The initial release requires Linux x86_64")
	}
	if store == nil {
		return nil, errors.New("A campaign store is required")
	}
	if options.APIKey == "" {
		return nil, errors.New("Supply an explicit OpenRouter API key via the selected environment variable")
	}
	if options.Notify == nil {
		options.Notify = func(string) {}
	}
	if options.RuntimeFactory == nil {
		options.RuntimeFactory = func() (Runtime, error) { return NewDockerRuntime() }
	}
	if options.GatewayFactory == nil {
		options.GatewayFactory = func(key string, cost *float64) (RunGateway, error) {
			gateway, err := NewGateway(key, GatewayOptions{Host: "0.0.0.0", CostLimit: cost})
			if err != nil {
				return RunGateway{}, err
			}
			return RunGateway{Token: gateway.Token, URL: gateway.ContainerURL(),
				Attempts: gateway.Attempts, LimitReached: gateway.LimitReached,
				InfrastructureFailed: gateway.InfrastructureFailed, Close: gateway.Close}, nil
		}
	}
	campaign = cloneRecord(campaign)
	for _, task := range records(campaign["tasks"]) {
		if repository := object(task["repository"]); len(repository) > 0 {
			resolved, warnings, err := preflightRepository(ctx, repository)
			if err != nil {
				return nil, err
			}
			task["repository"] = resolved
			for _, warning := range warnings {
				options.Notify("WARNING: " + text(warning))
			}
		}
	}
	campaignID := newID()
	inputs := filepath.Join(store.Root, "campaigns", campaignID)
	for _, task := range records(campaign["tasks"]) {
		for _, role := range []struct {
			key  string
			name string
		}{{"attachments", "attachments"}, {"evaluation", "evaluation"}} {
			var entries []Record
			if role.key == "evaluation" {
				entries = records(object(task[role.key])["files"])
			} else {
				entries = records(task[role.key])
			}
			destination := filepath.Join(inputs, "tasks", text(task["id"]), role.name)
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return nil, err
			}
			if err := CopyFiles(entries, destination); err != nil {
				return nil, err
			}
			for _, entry := range entries {
				entry["source"] = filepath.Join(destination, filepath.FromSlash(text(entry["target"])))
			}
		}
	}
	for _, selected := range records(campaign["harnesses"]) {
		entries := records(selected["files"])
		destination := filepath.Join(inputs, "harnesses", text(selected["id"]))
		if err := StageHarness(entries, destination); err != nil {
			return nil, err
		}
		for _, entry := range entries {
			entry["source"] = filepath.Join(destination, filepath.FromSlash(text(entry["target"])))
		}
	}
	specs := ExpandRuns(campaign)
	workers := int(number(campaign["concurrency"]))
	if workers < 1 {
		return nil, errors.New("Campaign concurrency must be positive")
	}
	if workers > len(specs) {
		workers = len(specs)
	}
	results := make([]Record, len(specs))
	jobs := make(chan int)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				results[index] = runCampaignTrial(ctx, specs[index], campaignID, store, options)
			}
		}()
	}
	for index := range specs {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	sort.Slice(results, func(i, j int) bool { return runResultLess(results[i], results[j]) })
	return results, nil
}

func runCampaignTrial(ctx context.Context, spec Record, campaignID string, store *Store, options RunOptions) Record {
	identifier := newID()
	directory, dirErr := store.RunDir(identifier)
	result := Record{"id": identifier, "campaign_id": campaignID, "task": text(object(spec["task"])["id"]),
		"agent": text(object(spec["agent"])["id"]), "model": spec["model"], "harness": text(object(spec["harness"])["id"]),
		"trial": spec["trial"], "status": "preparing", "started_at": unixTime(),
		"human_interventions": 0, "warnings": []any{}}
	secrets := []string{options.APIKey}
	var resultMu sync.Mutex
	set := func(key string, value any) {
		resultMu.Lock()
		result[key] = value
		resultMu.Unlock()
	}
	appendWarning := func(value any) {
		resultMu.Lock()
		result["warnings"] = append(array(result["warnings"]), value)
		resultMu.Unlock()
	}
	timeline := NewTimeline(nil)
	control := &runControl{}
	finished := make(chan struct{})
	var finishOnce sync.Once
	finish := func() { finishOnce.Do(func() { close(finished) }) }
	var runtimeHandle Runtime
	var container string
	var gateway *RunGateway
	var gatewayCloseOnce sync.Once
	var gatewayCloseErr error
	closeGateway := func() error {
		gatewayCloseOnce.Do(func() {
			if gateway != nil && gateway.Close != nil {
				gatewayCloseErr = gateway.Close()
			}
		})
		return gatewayCloseErr
	}
	var attempts []Record
	var watcherDone chan struct{}
	var agentCancel context.CancelFunc
	started := time.Now()
	if dirErr != nil {
		set("status", "infrastructure_failed")
		set("error", dirErr.Error())
	} else if err := store.Save(result); err != nil {
		set("status", "infrastructure_failed")
		set("error", err.Error())
	} else {
		runtimeValue, err := options.RuntimeFactory()
		if err != nil {
			set("status", "infrastructure_failed")
			set("error", err.Error())
		} else {
			runtimeHandle = runtimeValue
			if runtimeHandle == nil {
				set("status", "infrastructure_failed")
				set("error", "Runtime factory returned no runtime")
			}
		}
	}
	if text(result["status"]) != "infrastructure_failed" {
		secrets = append(secrets, options.APIKey)
		workspace := filepath.Join(directory, "workspace")
		var before map[string]Record
		metadata, err := PrepareWorkspace(ctx, object(spec["task"]), workspace)
		if err != nil {
			set("status", "infrastructure_failed")
			set("error", err.Error())
		} else {
			set("input", metadata)
			for _, warning := range array(metadata["warnings"]) {
				appendWarning(warning)
			}
			before, err = Inventory(workspace)
			if err == nil {
				baseline := filepath.Join(directory, "baseline")
				err = copySnapshot(workspace, baseline)
				if err == nil {
					err = writeJSONFile(filepath.Join(directory, "initial-files.json"), before)
				}
				if err == nil {
					err = writeJSONFile(filepath.Join(directory, "definition.json"), Redact(spec, secrets))
				}
			}
			if err == nil {
				identity := Record{"prompt": object(spec["task"])["prompt"], "commit": metadata["commit"], "files": before}
				hash, hashErr := pythonJSONHash(identity)
				if hashErr != nil {
					err = hashErr
				} else {
					set("input_sha256", hash)
				}
			}
			if err == nil {
				err = ValidateWorkspace(workspace, text(object(spec["agent"])["kind"]), text(spec["model"]))
			}
			if err != nil {
				set("status", "infrastructure_failed")
				set("error", err.Error())
			}
		}
		if text(result["status"]) != "infrastructure_failed" {
			home, homeErr := os.MkdirTemp("", "aqp-home-")
			if homeErr != nil {
				set("status", "infrastructure_failed")
				set("error", homeErr.Error())
			} else {
				defer os.RemoveAll(home)
				harness := object(spec["harness"])
				if err = StageHarness(records(harness["files"]), home); err != nil {
					set("status", "infrastructure_failed")
					set("error", err.Error())
				} else {
					cost, hasCost := object(spec["limits"])["cost_usd"]
					var costLimit *float64
					if hasCost && cost != nil {
						value := number(cost)
						costLimit = &value
					}
					opened, openErr := options.GatewayFactory(options.APIKey, costLimit)
					if openErr != nil {
						set("status", "infrastructure_failed")
						set("error", openErr.Error())
					} else {
						gateway = &opened
						secrets = append(secrets, opened.Token)
						gatewayURL := strings.ReplaceAll(opened.URL, "0.0.0.0", "host.docker.internal")
						gatewayURL = strings.ReplaceAll(gatewayURL, "127.0.0.1", "host.docker.internal")
						environment, configureErr := ConfigureHarness(home, text(object(spec["agent"])["kind"]), text(spec["model"]), gatewayURL, opened.Token)
						if configureErr != nil {
							set("status", "infrastructure_failed")
							set("error", configureErr.Error())
						} else {
							container, err = runtimeHandle.Start(ctx, text(object(spec["task"])["image"]), workspace,
								StartOptions{Home: home, Environment: environment, CopyBack: true})
							if err != nil {
								set("status", "infrastructure_failed")
								set("error", err.Error())
							} else {
								if transport := runtimeHandle.TransportFor(container); transport != "" {
									set("workspace_transport", transport)
								}
								if text(result["workspace_transport"]) == "docker_cp" {
									appendWarning("Docker bind mounts could not read the workspace; used Docker API copy transport.")
								}
								var agentCtx context.Context
								agentCtx, agentCancel = context.WithCancel(ctx)
								watcherDone = make(chan struct{})
								go watchRun(agentCtx, agentCancel, ctx, runtimeHandle, container, identifier, store,
									timeline, spec, gateway, control, finished, watcherDone, result, &resultMu, secrets)
								setupFailed := false
								for _, commandValue := range array(object(spec["task"])["setup"]) {
									if control.stopping.Load() {
										break
									}
									command := stringsOf(commandValue)
									setup, setupErr := runtimeHandle.Exec(agentCtx, container, command, 300*time.Second)
									if setupErr != nil && !control.stopping.Load() {
										err = setupErr
										break
									}
									if setup.ReturnCode != 0 && !control.stopping.Load() {
										err = fmt.Errorf("Task setup failed: %s", setup.Stderr)
										break
									}
								}
								set("setup_seconds", time.Since(started).Seconds())
								if ctx.Err() != nil || stopRequested(store, identifier) {
									if control.stop("manual_stop") {
										agentCancel()
									}
								}
								if err != nil {
									setupFailed = true
									set("status", "infrastructure_failed")
									set("error", err.Error())
								} else if control.stopping.Load() {
									set("status", "incomplete")
								} else {
									set("status", "running")
								}
								_ = saveRunSnapshot(result, &resultMu, timeline, gateway, store, identifier, secrets)
								if err == nil && !control.stopping.Load() {
									adapter := options.AdapterFactory
									if adapter == nil {
										adapter = defaultAdapterFactory
									}
									adapterValue := adapter(runtimeHandle, container, stringsOf(object(spec["agent"])["command"]), text(object(spec["agent"])["kind"]))
									runErr := adapterValue.Run(agentCtx, text(object(spec["task"])["prompt"]), text(spec["model"]),
										func(event Record) { recordRunEvent(identifier, event, timeline, store, secrets) },
										func(askCtx context.Context, question string, metadata Record) (any, error) {
											return waitForHuman(askCtx, identifier, question, metadata, store, options.Notify, control, secrets)
										})
									if ctx.Err() != nil || stopRequested(store, identifier) {
										control.stop("manual_stop")
									}
									if runErr != nil {
										set("error", runErr.Error())
										switch {
										case control.stopping.Load():
											set("status", "incomplete")
										case gatewayFailed(gateway):
											set("status", "infrastructure_failed")
										case hasProviderRejection(runGatewayAttempts(gateway)):
											set("status", "agent_failed")
										default:
											set("status", "integration_failed")
										}
									} else if control.stopping.Load() {
										set("status", "incomplete")
									} else {
										set("status", "completed")
									}
								}
								finish()
								if agentCancel != nil {
									agentCancel()
								}
								if watcherDone != nil {
									<-watcherDone
								}
								_ = timeline.Finish()
								var exportErr error
								var cleanupErr error
								if container != "" {
									// Quiesce agent writes before copying the final workspace snapshot.
									if err := runtimeHandle.Stop(context.Background(), container); err != nil {
										cleanupErr = err
									} else if err := runtimeHandle.ExportWorkspace(context.Background(), container, workspace); err != nil {
										exportErr = err
										set("infrastructure_incidents", true)
									}
									if err := runtimeHandle.Remove(context.Background(), container); err != nil && cleanupErr == nil {
										cleanupErr = err
									}
								}
								if err := closeGateway(); err != nil && cleanupErr == nil {
									cleanupErr = err
								}
								if err := os.RemoveAll(home); err != nil && cleanupErr == nil {
									cleanupErr = err
								}
								attempts = runGatewayAttempts(gateway)
								set("infrastructure_incidents", gatewayFailed(gateway) || result["infrastructure_incidents"] == true)
								if gatewayLimit(gateway) {
									set("status", "incomplete")
									control.stop("cost_limit")
								}
								reason := control.reason()
								if reason == "" && control.stopping.Load() {
									reason = "manual_stop"
								}
								if reason == "" {
									set("stop_reason", nil)
								} else {
									set("stop_reason", reason)
								}
								if exportErr != nil {
									set("status", "infrastructure_failed")
									set("error", "Workspace copy-back failed: "+exportErr.Error())
								} else if cleanupErr != nil {
									set("status", "infrastructure_failed")
									set("cleanup_error", cleanupErr.Error())
								}
								if exportErr == nil && cleanupErr == nil && !setupFailed {
									previousStatus := text(result["status"])
									set("status_before_evaluation", previousStatus)
									set("status", "evaluating")
									_ = saveRunSnapshot(result, &resultMu, timeline, gateway, store, identifier, secrets)
									evalStarted := time.Now()
									evaluation, evaluationErr := Evaluate(context.Background(), runtimeHandle, workspace,
										object(object(spec["task"])["evaluation"]), directory)
									if evaluationErr != nil {
										set("status", "evaluation_failed")
										set("evaluation_error", evaluationErr.Error())
									} else {
										set("evaluation", evaluation)
										set("status", previousStatus)
										resultMu.Lock()
										delete(result, "status_before_evaluation")
										resultMu.Unlock()
									}
									set("evaluation_seconds", time.Since(evalStarted).Seconds())
									if _, diffErr := WriteDiff(before, workspace, filepath.Join(directory, "changes.diff"), filepath.Join(directory, "baseline")); diffErr != nil {
										set("diff_error", diffErr.Error())
									}
								}
							}
						}
					}
				}
			}
		}
	}
	finish()
	if agentCancel != nil {
		agentCancel()
	}
	if watcherDone != nil {
		<-watcherDone
	}
	_ = timeline.Finish()
	if closeErr := closeGateway(); closeErr != nil {
		set("cleanup_error", closeErr.Error())
		if text(result["status"]) != "evaluation_failed" {
			set("status", "infrastructure_failed")
		}
	}
	if runtimeCloser, ok := runtimeHandle.(interface{ Close() error }); ok {
		if closeErr := runtimeCloser.Close(); closeErr != nil {
			set("cleanup_error", closeErr.Error())
			if text(result["status"]) != "evaluation_failed" {
				set("status", "infrastructure_failed")
			}
		}
	}
	finalAttempts := attempts
	if gateway != nil {
		finalAttempts = runGatewayAttempts(gateway)
	}
	set("timing", timeline.Snapshot())
	set("metrics", Redact(SummarizeAttempts(finalAttempts), secrets))
	providerErrors := []Record{}
	for _, attempt := range finalAttempts {
		if body, ok := attempt["provider_error_body"]; ok && body != nil {
			providerErrors = append(providerErrors, Record{"attempt_id": attempt["attempt_id"], "status": attempt["status"],
				"model": attempt["model"], "body": body, "truncated": attempt["provider_error_truncated"] == true})
		}
	}
	if len(providerErrors) > 0 {
		set("provider_errors", Redact(providerErrors, secrets))
	}
	if dirErr == nil {
		_ = writeJSONFile(filepath.Join(directory, "requests.json"), Redact(finalAttempts, secrets))
	}
	if text(result["status"]) == "completed" && number(object(result["metrics"])["request_count"]) == 0 {
		appendWarning("No model requests observed; accounting and gateway coverage are unverified.")
		metrics := cloneRecord(object(result["metrics"]))
		metrics["cost_usd"], metrics["input_tokens"], metrics["output_tokens"], metrics["accounting_complete"] = nil, nil, nil, false
		set("metrics", metrics)
	}
	interventions, _ := store.Interventions(identifier)
	set("human_interventions", interventions)
	set("pending_questions", []any{})
	kind := text(object(spec["agent"])["kind"])
	subagentTiming := "unavailable_without_harness_instrumentation"
	if kind == "opencode" {
		subagentTiming = "observed_session_events_only"
	}
	set("measurement_capabilities", Record{"request_attribution": "observed_headers_only", "subagent_timing": subagentTiming})
	set("duration_seconds", time.Since(started).Seconds())
	set("finished_at", unixTime())
	_ = store.Save(Redact(result, secrets).(Record))
	return Redact(result, secrets).(Record)
}

func watchRun(agentCtx context.Context, cancel context.CancelFunc, campaignCtx context.Context, runtimeHandle Runtime, container, identifier string,
	store *Store, timeline *Timeline, spec Record, gateway *RunGateway, control *runControl, finished <-chan struct{}, done chan<- struct{}, result Record, resultMu *sync.Mutex, secrets []string) {
	defer close(done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	stopSent := false
	for {
		select {
		case <-finished:
			return
		case <-ticker.C:
			stop := false
			reason := ""
			if campaignCtx.Err() != nil {
				stop, reason = true, "manual_stop"
			} else if requested, err := store.StopRequested(identifier); err == nil && requested {
				stop, reason = true, "manual_stop"
			}
			limit := object(spec["limits"])["agent_seconds"]
			if !stop && limit != nil && number(timeline.Snapshot()["agent_seconds"]) >= number(limit) {
				stop, reason = true, "agent_time_limit"
			}
			if !stop && gatewayLimit(gateway) {
				stop, reason = true, "cost_limit"
			}
			if stop && !stopSent {
				control.stop(reason)
				cancel()
				stopSent = true
				if err := runtimeHandle.Stop(context.Background(), container); err != nil {
					resultMu.Lock()
					result["stop_error"] = err.Error()
					resultMu.Unlock()
				}
			}
			_ = saveRunSnapshot(result, resultMu, timeline, gateway, store, identifier, secrets)
		}
		_ = agentCtx
	}
}

func saveRunSnapshot(result Record, resultMu *sync.Mutex, timeline *Timeline, gateway *RunGateway, store *Store, identifier string, secrets []string) error {
	resultMu.Lock()
	defer resultMu.Unlock()
	result["timing"] = timeline.Snapshot()
	if gateway != nil {
		result["metrics"] = SummarizeAttempts(runGatewayAttempts(gateway))
	}
	interventions, err := store.Interventions(identifier)
	if err == nil {
		result["human_interventions"] = interventions
	}
	questions, questionsErr := store.Questions(identifier)
	if questionsErr == nil {
		result["pending_questions"] = questions
	}
	if secrets == nil {
		secrets = []string{}
	}
	return store.Save(Redact(result, secrets).(Record))
}

func recordRunEvent(identifier string, event Record, timeline *Timeline, store *Store, secrets []string) {
	_ = timeline.Event(event)
	_ = store.Event(identifier, Redact(event, secrets).(Record))
}

func waitForHuman(ctx context.Context, identifier, question string, metadata Record, store *Store, notify func(string), control *runControl, secrets []string) (any, error) {
	requestID, err := store.Question(identifier, text(Redact(question, secrets)), Redact(metadata, secrets).(Record))
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(Record{"run_id": identifier, "request_id": requestID,
		"question": Redact(question, secrets), "metadata": Redact(metadata, secrets)})
	notify(string(encoded))
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, errors.New("Run stopped while awaiting human input")
		case <-ticker.C:
			if control.stopping.Load() {
				return nil, errors.New("Run stopped while awaiting human input")
			}
			answer, exists, err := store.Response(identifier, requestID)
			if err != nil {
				return nil, err
			}
			if exists && answer != nil {
				value, ok := answer.(string)
				if !ok {
					return nil, errors.New("Human reply must be text")
				}
				return CoerceAnswer(value, metadata)
			}
		}
	}
}

func defaultAdapterFactory(runtimeHandle Runtime, container string, command []string, kind string) AgentAdapter {
	switch kind {
	case "pi":
		return NewPiAdapter(runtimeHandle, container, command)
	case "opencode":
		return NewOpenCodeAdapter(runtimeHandle, container, command)
	default:
		return nil
	}
}

func runGatewayAttempts(gateway *RunGateway) []Record {
	if gateway == nil || gateway.Attempts == nil {
		return nil
	}
	return gateway.Attempts()
}

func gatewayLimit(gateway *RunGateway) bool {
	return gateway != nil && gateway.LimitReached != nil && gateway.LimitReached()
}

func gatewayFailed(gateway *RunGateway) bool {
	return gateway != nil && gateway.InfrastructureFailed != nil && gateway.InfrastructureFailed()
}

func hasProviderRejection(attempts []Record) bool {
	for _, attempt := range attempts {
		status := number(attempt["status"])
		if status >= 400 && status < 500 && status != 429 {
			return true
		}
	}
	return false
}

func stopRequested(store *Store, identifier string) bool {
	requested, err := store.StopRequested(identifier)
	return err == nil && requested
}

func runResultLess(left, right Record) bool {
	leftKeys := []string{text(left["task"]), text(left["agent"]), text(left["model"]), text(left["harness"])}
	rightKeys := []string{text(right["task"]), text(right["agent"]), text(right["model"]), text(right["harness"])}
	for index := range leftKeys {
		if leftKeys[index] != rightKeys[index] {
			return leftKeys[index] < rightKeys[index]
		}
	}
	return number(left["trial"]) < number(right["trial"])
}

func writeJSONFile(filename string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filename, append(data, '\n'), 0o644)
}

func pythonJSONHash(value any) (string, error) {
	var buffer strings.Builder
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	compact := strings.TrimSuffix(buffer.String(), "\n")
	var spaced strings.Builder
	inString, escaped := false, false
	for index := 0; index < len(compact); index++ {
		character := compact[index]
		spaced.WriteByte(character)
		if inString {
			if escaped {
				escaped = false
			} else if character == '\\' {
				escaped = true
			} else if character == '"' {
				inString = false
			}
			continue
		}
		if character == '"' {
			inString = true
		} else if character == ',' || character == ':' {
			spaced.WriteByte(' ')
		}
	}
	var ascii strings.Builder
	for _, char := range spaced.String() {
		if char <= 0x7f {
			ascii.WriteRune(char)
		} else if char <= 0xffff {
			fmt.Fprintf(&ascii, "\\u%04x", char)
		} else {
			code := char - 0x10000
			fmt.Fprintf(&ascii, "\\u%04x\\u%04x", 0xd800+(code>>10), 0xdc00+(code&0x3ff))
		}
	}
	sum := sha256.Sum256([]byte(ascii.String()))
	return hex.EncodeToString(sum[:]), nil
}
