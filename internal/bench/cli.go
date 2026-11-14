package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const cliProgram = "aqp-bench"

var cliCommands = []string{"validate", "run", "demo", "list", "show", "stop", "questions", "answer", "rerun", "dashboard"}

// Match argparse's negative-number positional syntax, not arbitrary dash-prefixed text.
var cliNegativeNumber = regexp.MustCompile(`^-(?:[0-9]+|[0-9]*\.[0-9]+)$`)

type cliArgs struct {
	command, artifacts, manifest, runID, requestID, reply string
	apiKeyEnv                                             string
	port                                                  int
}

type cliParseError struct {
	usage, program, message string
}

func (e *cliParseError) Error() string { return e.message }

func rootUsage() string {
	return "aqp-bench [-h] [--artifacts ARTIFACTS] {" + strings.Join(cliCommands, ",") + "} ..."
}

func commandUsage(command string) string {
	switch command {
	case "validate":
		return "aqp-bench validate [-h] manifest"
	case "run":
		return "aqp-bench run [-h] [--api-key-env API_KEY_ENV] manifest"
	case "show":
		return "aqp-bench show [-h] run_id"
	case "stop":
		return "aqp-bench stop [-h] run_id"
	case "questions":
		return "aqp-bench questions [-h] [--run-id RUN_ID]"
	case "answer":
		return "aqp-bench answer [-h] run_id request_id reply"
	case "rerun":
		return "aqp-bench rerun [-h] [--api-key-env API_KEY_ENV] run_id"
	case "dashboard":
		return "aqp-bench dashboard [-h] [--port PORT]"
	default:
		return "aqp-bench " + command + " [-h]"
	}
}

func rootHelp() string {
	return "usage: " + cliProgram + " [-h] [--artifacts ARTIFACTS] {" + strings.Join(cliCommands, ",") + "} ...\n\n" +
		"Local coding-agent benchmarks; Linux x86_64\n\n" +
		"positional arguments:\n" +
		"  {validate,run,demo,list,show,stop,questions,answer,rerun,dashboard}\n" +
		"    validate            Validate a campaign without model calls or Docker\n" +
		"    run                 Run a campaign; requires prepared Docker images and an explicit API key\n" +
		"    demo                Offline simulation: mock provider and trusted local execution, no Docker or paid calls\n" +
		"    list                List retained run results\n" +
		"    show                Print a retained run result\n" +
		"    stop                Request agent stop; current workspace will still be evaluated\n" +
		"    questions           Show pending human requests and complete choice metadata\n" +
		"    answer              Submit one human reply; structured question replies use JSON\n" +
		"    rerun               Explicitly rerun a retained definition as a new linked run\n" +
		"    dashboard           Serve loopback-only live monitoring; no mutation controls\n\n" +
		"options:\n" +
		"  -h, --help            show this help message and exit\n" +
		"  --artifacts ARTIFACTS Local persistent artifact directory\n"
}

func commandHelp(command string) string {
	usage := commandUsage(command)
	common := "usage: " + usage + "\n\n"
	options := "  -h, --help            show this help message and exit\n"
	switch command {
	case "validate":
		return common + "Validate a campaign without model calls or Docker\n\npositional arguments:\n  manifest\n\noptions:\n" + options
	case "run":
		return common + "Run a campaign; requires prepared Docker images and an explicit API key\n\npositional arguments:\n  manifest\n\noptions:\n" + options + "  --api-key-env API_KEY_ENV\n                        Explicit environment variable containing the provider key\n"
	case "demo":
		return common + "Offline simulation: mock provider and trusted local execution, no Docker or paid calls\n\noptions:\n" + options
	case "list":
		return common + "List retained run results\n\noptions:\n" + options
	case "show":
		return common + "Print a retained run result\n\npositional arguments:\n  run_id\n\noptions:\n" + options
	case "stop":
		return common + "Request agent stop; current workspace will still be evaluated\n\npositional arguments:\n  run_id\n\noptions:\n" + options
	case "questions":
		return common + "Show pending human requests and complete choice metadata\n\noptions:\n" + options + "  --run-id RUN_ID\n"
	case "answer":
		return common + "Submit one human reply; structured question replies use JSON\n\npositional arguments:\n  run_id\n  request_id\n  reply                 Text, yes/no for confirmation, JSON string[][] for OpenCode questions, or null to cancel\n\noptions:\n" + options
	case "rerun":
		return common + "Explicitly rerun a retained definition as a new linked run\n\npositional arguments:\n  run_id\n\noptions:\n" + options + "  --api-key-env API_KEY_ENV\n"
	case "dashboard":
		return common + "Serve loopback-only live monitoring; no mutation controls\n\noptions:\n" + options + "  --port PORT\n"
	default:
		return rootHelp()
	}
}

func canonicalCLIOption(token string, names ...string) string {
	flag, value, hasValue := strings.Cut(token, "=")
	if !strings.HasPrefix(flag, "--") || len(flag) <= 2 {
		return token
	}
	match := ""
	for _, name := range names {
		if strings.HasPrefix(name, flag) {
			if match != "" {
				return token
			}
			match = name
		}
	}
	if match == "" {
		return token
	}
	if hasValue {
		return match + "=" + value
	}
	return match
}

func optionValue(args []string, index *int, name string) (string, error) {
	token := args[*index]
	if strings.HasPrefix(token, name+"=") {
		return strings.TrimPrefix(token, name+"="), nil
	}
	if *index+1 >= len(args) || args[*index+1] == "--" || strings.HasPrefix(args[*index+1], "-") && !cliNegativeNumber.MatchString(args[*index+1]) {
		return "", fmt.Errorf("argument %s: expected one argument", name)
	}
	*index++
	return args[*index], nil
}

func parseCLIArgs(args []string) (cliArgs, string, error) {
	args = append([]string(nil), args...)
	parsed := cliArgs{artifacts: ".aqp-bench", apiKeyEnv: "OPENROUTER_API_KEY", port: 8765}
	if len(args) == 0 {
		return parsed, "", &cliParseError{usage: rootUsage(), program: cliProgram, message: "the following arguments are required: command"}
	}
	commandIndex := -1
	for i := 0; i < len(args); i++ {
		token := canonicalCLIOption(args[i], "--help", "--artifacts")
		args[i] = token
		if token == "-h" || token == "--help" {
			return parsed, rootHelp(), nil
		}
		if token == "--artifacts" || strings.HasPrefix(token, "--artifacts=") {
			value, err := optionValue(args, &i, "--artifacts")
			if err != nil {
				return parsed, "", &cliParseError{usage: rootUsage(), program: cliProgram, message: err.Error()}
			}
			parsed.artifacts = value
			continue
		}
		if strings.HasPrefix(token, "-") {
			return parsed, "", &cliParseError{usage: rootUsage(), program: cliProgram, message: "unrecognized arguments: " + token}
		}
		parsed.command, commandIndex = token, i
		break
	}
	if commandIndex < 0 {
		return parsed, "", &cliParseError{usage: rootUsage(), program: cliProgram, message: "the following arguments are required: command"}
	}
	if !containsString(cliCommands, parsed.command) {
		return parsed, "", &cliParseError{usage: rootUsage(), program: cliProgram, message: fmt.Sprintf("invalid choice: %q (choose from %s)", parsed.command, strings.Join(cliCommands, ", "))}
	}
	remaining := args[commandIndex+1:]
	optionsEnded := false
	for _, token := range remaining {
		if token == "--" {
			optionsEnded = true
			continue
		}
		if !optionsEnded && (token == "-h" || canonicalCLIOption(token, "--help") == "--help") {
			return parsed, commandHelp(parsed.command), nil
		}
	}
	positionals := []string{}
	optionsEnded = false
	for i := 0; i < len(remaining); i++ {
		token := remaining[i]
		if token == "--" && !optionsEnded {
			optionsEnded = true
			continue
		}
		if !optionsEnded && strings.HasPrefix(token, "-") && !cliNegativeNumber.MatchString(token) {
			var option string
			switch parsed.command {
			case "run", "rerun":
				option = "--api-key-env"
			case "questions":
				option = "--run-id"
			case "dashboard":
				option = "--port"
			}
			token = canonicalCLIOption(token, option)
			remaining[i] = token
			if option == "" || token != option && !strings.HasPrefix(token, option+"=") {
				return parsed, "", &cliParseError{usage: commandUsage(parsed.command), program: cliProgram + " " + parsed.command, message: "unrecognized arguments: " + token}
			}
			value, err := optionValue(remaining, &i, option)
			if err != nil {
				return parsed, "", &cliParseError{usage: commandUsage(parsed.command), program: cliProgram + " " + parsed.command, message: err.Error()}
			}
			switch option {
			case "--api-key-env":
				parsed.apiKeyEnv = value
			case "--run-id":
				parsed.runID = value
			case "--port":
				port, parseErr := strconv.Atoi(value)
				if parseErr != nil {
					return parsed, "", &cliParseError{usage: commandUsage(parsed.command), program: cliProgram + " " + parsed.command, message: fmt.Sprintf("argument --port: invalid int value: %q", value)}
				}
				parsed.port = port
			}
			continue
		}
		positionals = append(positionals, token)
	}
	minimum, maximum := 0, 0
	switch parsed.command {
	case "validate", "run", "show", "stop", "rerun":
		minimum, maximum = 1, 1
	case "answer":
		minimum, maximum = 3, 3
	}
	if len(positionals) < minimum || len(positionals) > maximum {
		extraStart := min(maximum, len(positionals))
		message := "unrecognized arguments: " + strings.Join(positionals[extraStart:], " ")
		if len(positionals) < minimum {
			missing := map[string]string{
				"validate": "manifest", "run": "manifest", "show": "run_id", "stop": "run_id", "rerun": "run_id",
				"answer": "run_id request_id reply",
			}[parsed.command]
			got := min(len(positionals), minimum)
			parts := strings.Fields(missing)
			message = "the following arguments are required: " + strings.Join(parts[got:], ", ")
		}
		return parsed, "", &cliParseError{usage: commandUsage(parsed.command), program: cliProgram + " " + parsed.command, message: message}
	}
	if len(positionals) > 0 {
		switch parsed.command {
		case "validate", "run":
			parsed.manifest = positionals[0]
		case "show", "stop", "rerun":
			parsed.runID = positionals[0]
		case "answer":
			parsed.runID, parsed.requestID, parsed.reply = positionals[0], positionals[1], positionals[2]
		}
	}
	return parsed, "", nil
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// CLI is the public command boundary. The artifact store is opened only after
// parse-time checks, manifest validation, and live-key selection succeed.
func CLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	parsed, help, err := parseCLIArgs(args)
	if help != "" {
		_, _ = io.WriteString(stdout, help)
		return 0
	}
	if err != nil {
		var parseErr *cliParseError
		if errors.As(err, &parseErr) {
			fmt.Fprintf(stderr, "usage: %s\n%s: error: %s\n", parseErr.usage, parseErr.program, parseErr.message)
		} else {
			fmt.Fprintf(stderr, "Error: %v\n", err)
		}
		return 2
	}
	if err := ctx.Err(); err != nil {
		return 130
	}
	writeJSON := func(value any) error { return json.NewEncoder(stdout).Encode(value) }
	writeError := func(err error) int {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return 130
		}
		fmt.Fprintf(stderr, "Error: %v\n", cliError(err))
		return 2
	}
	switch parsed.command {
	case "validate":
		campaign, loadErr := LoadCampaign(parsed.manifest)
		if loadErr != nil {
			return writeError(loadErr)
		}
		warnings := []any{}
		for _, task := range records(campaign["tasks"]) {
			if repository := object(task["repository"]); len(repository) > 0 {
				_, messages, checkErr := preflightRepository(ctx, repository)
				if checkErr != nil {
					return writeError(checkErr)
				}
				for _, message := range messages {
					warnings = append(warnings, message)
				}
			}
		}
		if err := writeJSON(Record{"valid": true, "runs": len(ExpandRuns(campaign)), "warnings": warnings}); err != nil {
			return writeError(err)
		}
		return 0
	case "run":
		campaign, loadErr := LoadCampaign(parsed.manifest)
		if loadErr != nil {
			return writeError(loadErr)
		}
		key, keyErr := selectedAPIKey(parsed.apiKeyEnv)
		if keyErr != nil {
			return writeError(keyErr)
		}
		store, storeErr := NewStore(parsed.artifacts)
		if storeErr != nil {
			return writeError(storeErr)
		}
		defer store.Close()
		results, runErr := RunCampaign(ctx, campaign, store, RunOptions{APIKey: key, Notify: func(message string) { fmt.Fprintln(stderr, message) }})
		if runErr != nil {
			return writeError(runErr)
		}
		if err := writeJSON(Record{"runs": results, "aggregate": Aggregate(results)}); err != nil {
			return writeError(err)
		}
		for _, result := range results {
			if text(result["status"]) != "completed" {
				return 1
			}
		}
		return 0
	case "demo":
		store, storeErr := NewStore(parsed.artifacts)
		if storeErr != nil {
			return writeError(storeErr)
		}
		defer store.Close()
		result, demoErr := RunDemo(ctx, store)
		if demoErr != nil {
			return writeError(demoErr)
		}
		if err := writeJSON(result); err != nil {
			return writeError(err)
		}
		if result["status"] == "completed" && number(object(result["evaluation"])["score_percent"]) == 100 {
			return 0
		}
		return 1
	case "list", "questions", "dashboard":
		store, storeErr := NewStore(parsed.artifacts)
		if storeErr != nil {
			return writeError(storeErr)
		}
		defer store.Close()
		switch parsed.command {
		case "list":
			runs, listErr := store.List()
			if listErr != nil {
				return writeError(listErr)
			}
			if err := writeJSON(runs); err != nil {
				return writeError(err)
			}
			return 0
		case "questions":
			questions, listErr := cliPendingQuestions(store, parsed.runID)
			if listErr != nil {
				return writeError(listErr)
			}
			if err := writeJSON(questions); err != nil {
				return writeError(err)
			}
			return 0
		case "dashboard":
			dashboard, dashboardErr := NewDashboard(store, parsed.port)
			if dashboardErr != nil {
				return writeError(dashboardErr)
			}
			defer dashboard.Close()
			fmt.Fprintln(stdout, "Live monitoring: "+dashboard.URL)
			<-ctx.Done()
			if errors.Is(ctx.Err(), context.Canceled) {
				return 130
			}
			return 130
		}
	case "show", "stop", "answer", "rerun":
		database := filepath.Join(parsed.artifacts, "bench.sqlite3")
		if info, statErr := os.Stat(database); statErr != nil || !info.Mode().IsRegular() {
			if statErr != nil && !os.IsNotExist(statErr) {
				return writeError(statErr)
			}
			return writeError(errors.New("No retained benchmark store at the selected artifact directory"))
		}
		store, storeErr := NewStore(parsed.artifacts)
		if storeErr != nil {
			return writeError(storeErr)
		}
		defer store.Close()
		switch parsed.command {
		case "show":
			result, getErr := store.Get(parsed.runID)
			if getErr != nil {
				return writeError(getErr)
			}
			if err := writeJSON(result); err != nil {
				return writeError(err)
			}
			return 0
		case "stop":
			result, getErr := store.Get(parsed.runID)
			if getErr != nil {
				return writeError(getErr)
			}
			if status := text(result["status"]); status != "preparing" && status != "running" {
				return writeError(errors.New("Run is not currently executing an agent"))
			}
			if stopErr := store.Stop(parsed.runID); stopErr != nil {
				return writeError(stopErr)
			}
			if err := writeJSON(Record{"stop_requested": parsed.runID}); err != nil {
				return writeError(err)
			}
			return 0
		case "answer":
			return cliAnswer(store, parsed, stdout, stderr)
		case "rerun":
			return cliRerun(ctx, store, parsed, stdout, stderr)
		}
	}
	return 0
}

func selectedAPIKey(name string) (string, error) {
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("Set the explicitly selected API-key environment variable %s", name)
	}
	return value, nil
}

func cliPendingQuestions(store *Store, runID string) ([]Record, error) {
	result := []Record{}
	questions, err := store.Questions(runID)
	if err != nil {
		return nil, err
	}
	for _, question := range questions {
		run, err := store.Get(text(question["run_id"]))
		if err != nil {
			return nil, err
		}
		if run["status"] == "running" {
			result = append(result, question)
		}
	}
	return result, nil
}

func cliAnswer(store *Store, args cliArgs, stdout, stderr io.Writer) int {
	questions, err := store.Questions(args.runID)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", cliError(err))
		return 2
	}
	var question Record
	for _, candidate := range questions {
		if candidate["id"] == args.requestID {
			question = candidate
			break
		}
	}
	if question == nil {
		fmt.Fprintln(stderr, "Error: Unknown or already answered request")
		return 2
	}
	run, err := store.Get(args.runID)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", cliError(err))
		return 2
	}
	if run["status"] != "running" {
		fmt.Fprintln(stderr, "Error: Run is no longer waiting for agent replies")
		return 2
	}
	if _, err := CoerceAnswer(args.reply, object(question["metadata"])); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 2
	}
	answered, err := store.Answer(args.runID, args.requestID, args.reply)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 2
	}
	if !answered {
		fmt.Fprintln(stderr, "Error: Request was already answered")
		return 2
	}
	if err := json.NewEncoder(stdout).Encode(Record{"answered": args.requestID}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 2
	}
	return 0
}

func cliRerun(ctx context.Context, store *Store, args cliArgs, stdout, stderr io.Writer) int {
	key, err := selectedAPIKey(args.apiKeyEnv)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 2
	}
	old, err := store.Get(args.runID)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", cliError(err))
		return 2
	}
	if old["simulation"] == true {
		fmt.Fprintln(stderr, "Error: Use demo for offline simulation; rerun is for live definitions")
		return 2
	}
	runDir, err := store.RunDir(args.runID)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 2
	}
	definitionBytes, err := os.ReadFile(filepath.Join(runDir, "definition.json"))
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 2
	}
	var definition Record
	if err := decodeJSON(definitionBytes, &definition); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 2
	}
	campaign := Record{"version": 1, "tasks": []any{definition["task"]}, "agents": []any{definition["agent"]},
		"models": []any{definition["model"]}, "harnesses": []any{definition["harness"]}, "trials": 1,
		"concurrency": 1, "limits": definition["limits"]}
	results, err := RunCampaign(ctx, campaign, store, RunOptions{APIKey: key, Notify: func(message string) { fmt.Fprintln(stderr, message) }})
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", cliError(err))
		if errors.Is(ctx.Err(), context.Canceled) {
			return 130
		}
		return 2
	}
	for _, result := range results {
		result["rerun_of"] = args.runID
		if err := store.Save(result); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 2
		}
	}
	if err := json.NewEncoder(stdout).Encode(Record{"runs": results, "aggregate": Aggregate(results)}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 2
	}
	for _, result := range results {
		if text(result["status"]) != "completed" {
			return 1
		}
	}
	return 0
}

func cliError(err error) error {
	const prefix, suffix = "run ", " not found"
	message := err.Error()
	if strings.HasPrefix(message, prefix) && strings.HasSuffix(message, suffix) {
		quoted := strings.TrimSuffix(strings.TrimPrefix(message, prefix), suffix)
		if missing, parseErr := strconv.Unquote(quoted); parseErr == nil {
			return fmt.Errorf("'%s'", missing)
		}
	}
	return err
}
