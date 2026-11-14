package bench

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func RelativePath(value string) (string, error) {
	if value == "" || strings.Contains(value, "\\") {
		return "", fmt.Errorf("Expected a nonempty relative POSIX path")
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == ".git" {
			return "", fmt.Errorf("Unsafe relative path: %s", value)
		}
	}
	clean := path.Clean(value)
	if path.IsAbs(value) || clean == "." {
		return "", fmt.Errorf("Unsafe relative path: %s", value)
	}
	return clean, nil
}
func pythonKeys(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	v = append([]string{}, v...)
	sort.Strings(v)
	return "['" + strings.Join(v, "', '") + "']"
}
func checkKeys(value any, allowed, required []string) error {
	m, ok := value.(map[string]any)
	valid := ok
	for k := range m {
		found := false
		for _, a := range allowed {
			if a == k {
				found = true
				break
			}
		}
		valid = valid && found
	}
	for _, k := range required {
		_, ok := m[k]
		valid = valid && ok
	}
	if !valid {
		return fmt.Errorf("Expected object with keys %s; required %s", pythonKeys(allowed), pythonKeys(required))
	}
	return nil
}
func checkText(v any, name string) error {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" || strings.ContainsRune(s, 0) {
		return fmt.Errorf("%s must be a nonempty string", name)
	}
	return nil
}
func checkNumber(v any, name string, whole, positive bool) error {
	if !numeric(v) {
		return fmt.Errorf("%s must be numeric", name)
	}
	n := number(v)
	if math.IsNaN(n) || math.IsInf(n, 0) || (positive && n <= 0) || (!positive && n < 0) {
		kind := "nonnegative"
		if positive {
			kind = "positive"
		}
		return fmt.Errorf("%s must be finite and %s", name, kind)
	}
	if whole && !integer(v) {
		return fmt.Errorf("%s must be an integer", name)
	}
	return nil
}
func checkCommand(v any) error {
	a := array(v)
	if a == nil || len(a) == 0 {
		return fmt.Errorf("Commands must be nonempty argv arrays, not shell strings")
	}
	for _, arg := range a {
		if err := checkText(arg, "command argument"); err != nil {
			return err
		}
	}
	return nil
}

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func checkItems(v any, name string) error {
	a := array(v)
	if a == nil || len(a) == 0 {
		return fmt.Errorf("%s must be a nonempty array", name)
	}
	seen := map[string]bool{}
	for _, item := range a {
		id := text(object(item)["id"])
		if !identifierPattern.MatchString(id) {
			return fmt.Errorf("Invalid %s id", name)
		}
		if seen[id] {
			return fmt.Errorf("Duplicate %s id: %s", name, id)
		}
		seen[id] = true
	}
	return nil
}
func checkFiles(v any, base string) error {
	files := array(v)
	if files == nil {
		return fmt.Errorf("files must be an array")
	}
	targets := []string{}
	for _, item := range files {
		if err := checkKeys(item, []string{"source", "target"}, []string{"source", "target"}); err != nil {
			return err
		}
		entry := object(item)
		if _, ok := entry["target"].(string); !ok {
			return fmt.Errorf("Expected a nonempty relative POSIX path")
		}
		target, err := RelativePath(text(entry["target"]))
		if err != nil {
			return err
		}
		for _, old := range targets {
			if target == old || strings.HasPrefix(target, old+"/") || strings.HasPrefix(old, target+"/") {
				return fmt.Errorf("File targets must not overlap")
			}
		}
		targets = append(targets, target)
		if err := checkText(entry["source"], "source"); err != nil {
			return err
		}
		source := text(entry["source"])
		if !filepath.IsAbs(source) {
			source = filepath.Join(base, source)
		}
		source, err = filepath.Abs(source)
		if err != nil {
			return err
		}
		info, err := os.Lstat(source)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Missing source or symlink source: %s", source)
		}
		entry["source"], entry["target"] = source, target
	}
	return nil
}
func setDefault(r Record, key string, value any) {
	if _, ok := r[key]; !ok {
		r[key] = value
	}
}

func LoadCampaign(filename string) (Record, error) {
	filename, err := filepath.Abs(filename)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("Cannot load campaign: %w", err)
	}
	var data Record
	if err = decodeJSON(b, &data); err != nil {
		return nil, fmt.Errorf("Cannot load campaign: %w", err)
	}
	if err = checkKeys(data, []string{"version", "tasks", "agents", "models", "harnesses", "trials", "concurrency", "limits"}, []string{"version", "tasks", "agents", "models"}); err != nil {
		return nil, err
	}
	if !integer(data["version"]) || number(data["version"]) != 1 {
		return nil, fmt.Errorf("Only manifest version 1 is supported")
	}
	setDefault(data, "trials", 1)
	setDefault(data, "concurrency", 1)
	setDefault(data, "limits", Record{})
	setDefault(data, "harnesses", []any{Record{"id": "none", "mode": "explicit", "files": []any{}}})
	for _, key := range []string{"trials", "concurrency"} {
		if err = checkNumber(data[key], key, true, true); err != nil {
			return nil, err
		}
	}
	if err = checkKeys(data["limits"], []string{"agent_seconds", "cost_usd"}, nil); err != nil {
		return nil, err
	}
	for key, value := range object(data["limits"]) {
		if err = checkNumber(value, key, false, true); err != nil {
			return nil, err
		}
	}
	models := array(data["models"])
	if models == nil || len(models) == 0 {
		return nil, fmt.Errorf("models must be a nonempty array")
	}
	seen := map[string]bool{}
	for _, model := range models {
		if err = checkText(model, "model"); err != nil {
			return nil, err
		}
		m := text(model)
		if !strings.Contains(m, "/") {
			return nil, fmt.Errorf("Use OpenRouter model IDs in provider/model form")
		}
		if seen[m] {
			return nil, fmt.Errorf("Duplicate models")
		}
		seen[m] = true
	}
	for _, name := range []string{"tasks", "agents", "harnesses"} {
		if err = checkItems(data[name], name); err != nil {
			return nil, err
		}
	}
	for _, agent := range records(data["agents"]) {
		if err = checkKeys(agent, []string{"id", "kind", "command"}, []string{"id", "kind"}); err != nil {
			return nil, err
		}
		if agent["kind"] != "pi" && agent["kind"] != "opencode" {
			return nil, fmt.Errorf("Supported live agents: pi, opencode")
		}
		if c, ok := agent["command"]; ok {
			if err = checkCommand(c); err != nil {
				return nil, err
			}
		}
	}
	for _, harness := range records(data["harnesses"]) {
		if err = checkKeys(harness, []string{"id", "mode", "files"}, []string{"id", "mode"}); err != nil {
			return nil, err
		}
		if harness["mode"] != "explicit" && harness["mode"] != "inherit" {
			return nil, fmt.Errorf("Harness mode must be explicit or inherit")
		}
		setDefault(harness, "files", []any{})
		if err = checkFiles(harness["files"], filepath.Dir(filename)); err != nil {
			return nil, err
		}
	}
	for _, task := range records(data["tasks"]) {
		if err = checkKeys(task, []string{"id", "prompt", "image", "setup", "repository", "attachments", "evaluation"}, []string{"id", "prompt", "image", "evaluation"}); err != nil {
			return nil, err
		}
		for _, key := range []string{"prompt", "image"} {
			if err = checkText(task[key], key); err != nil {
				return nil, err
			}
		}
		setDefault(task, "setup", []any{})
		if array(task["setup"]) == nil {
			return nil, fmt.Errorf("setup must be an array of argv arrays")
		}
		for _, command := range array(task["setup"]) {
			if err = checkCommand(command); err != nil {
				return nil, err
			}
		}
		if repo, ok := task["repository"]; ok {
			if err = checkKeys(repo, []string{"source", "revision"}, []string{"source", "revision"}); err != nil {
				return nil, err
			}
			r := object(repo)
			for _, k := range []string{"source", "revision"} {
				if err = checkText(r[k], map[string]string{"source": "repository source", "revision": "revision"}[k]); err != nil {
					return nil, err
				}
			}
			source, rev := text(r["source"]), text(r["revision"])
			if strings.HasPrefix(rev, "-") {
				return nil, fmt.Errorf("Invalid repository revision")
			}
			u, e := url.Parse(source)
			if e == nil && (u.Scheme == "http" || u.Scheme == "https") {
				if !regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`).MatchString(rev) {
					return nil, fmt.Errorf("Remote repository revisions must be full commit hashes")
				}
				if u.User != nil {
					return nil, fmt.Errorf("Do not embed credentials in repository URLs")
				}
			} else {
				if !filepath.IsAbs(source) {
					source = filepath.Join(filepath.Dir(filename), source)
				}
				source, _ = filepath.Abs(source)
				info, e := os.Stat(source)
				if e != nil || !info.IsDir() {
					return nil, fmt.Errorf("Repository must be a local directory or HTTP(S) Git URL")
				}
				r["source"] = source
			}
		}
		setDefault(task, "attachments", []any{})
		if err = checkFiles(task["attachments"], filepath.Dir(filename)); err != nil {
			return nil, err
		}
		eval := object(task["evaluation"])
		if err = checkKeys(task["evaluation"], []string{"image", "files", "tests", "timeout_seconds"}, []string{"tests"}); err != nil {
			return nil, err
		}
		setDefault(eval, "image", task["image"])
		setDefault(eval, "timeout_seconds", 60)
		setDefault(eval, "files", []any{})
		if err = checkText(eval["image"], "evaluation image"); err != nil {
			return nil, err
		}
		if err = checkNumber(eval["timeout_seconds"], "evaluation timeout", false, true); err != nil {
			return nil, err
		}
		if err = checkFiles(eval["files"], filepath.Dir(filename)); err != nil {
			return nil, err
		}
		if err = checkItems(eval["tests"], "tests"); err != nil {
			return nil, err
		}
		total := 0.0
		for _, test := range records(eval["tests"]) {
			if err = checkKeys(test, []string{"id", "score", "command"}, []string{"id", "score", "command"}); err != nil {
				return nil, err
			}
			if err = checkNumber(test["score"], "test score", false, false); err != nil {
				return nil, err
			}
			if err = checkCommand(test["command"]); err != nil {
				return nil, err
			}
			total += number(test["score"])
		}
		if total == 0 {
			return nil, fmt.Errorf("Evaluation must have positive total score")
		}
	}
	return data, nil
}
func ExpandRuns(c Record) []Record {
	out := []Record{}
	for _, task := range records(c["tasks"]) {
		for _, agent := range records(c["agents"]) {
			for _, model := range array(c["models"]) {
				for _, harness := range records(c["harnesses"]) {
					for trial := 1; trial <= int(number(c["trials"])); trial++ {
						out = append(out, Record{"task": cloneRecord(task), "agent": cloneRecord(agent), "model": model, "harness": cloneRecord(harness), "trial": trial, "limits": cloneRecord(object(c["limits"]))})
					}
				}
			}
		}
	}
	return out
}
