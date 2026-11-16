package bench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func imageFixturePath(t *testing.T, name string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "images", "pi-gentle-python", name)
}
func imageFixtureRead(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(imageFixturePath(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPythonImageRetainsVerifierToolsWithoutHostEngine(t *testing.T) {
	dockerfile := imageFixtureRead(t, "Dockerfile")
	for _, required := range []string{"FROM python:3.11-slim-bookworm", "FROM node:24-bookworm-slim", "npm ci", "pip install --no-cache-dir -r /tmp/requirements.txt", `CMD ["python", "--version"]`, "USER agent", "GENTLE_SHELL_NO_AUTO_SETUP=1"} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("image missing %q", required)
		}
	}
	if strings.Contains(dockerfile, "COPY . .") || strings.Contains(dockerfile, "-e '.[dev]'") {
		t.Error("task image still installs host engine")
	}
	if !strings.Contains(imageFixtureRead(t, "requirements.txt"), "pytest>=8") {
		t.Error("missing selected Python verifier dependency")
	}
	if !strings.Contains(imageFixtureRead(t, "apt-packages.txt"), "git") {
		t.Error("missing Git tool")
	}
}

func TestPythonImagePreservesPinnedAgentGraphAndBuildGuards(t *testing.T) {
	var packages, lock map[string]any
	if err := json.Unmarshal([]byte(imageFixtureRead(t, "package.json")), &packages); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(imageFixtureRead(t, "package-lock.json")), &lock); err != nil {
		t.Fatal(err)
	}
	deps := packages["dependencies"].(map[string]any)
	if len(deps) != 2 || deps["@earendil-works/pi-coding-agent"] != "1.5.0" || deps["gentle-pi"] != "4.5.1" {
		t.Fatal(deps)
	}
	locked := lock["packages"].(map[string]any)
	root := locked[""].(map[string]any)["dependencies"].(map[string]any)
	for name, version := range deps {
		entry := locked["node_modules/"+name].(map[string]any)
		if root[name] != version || entry["version"] != version || !strings.HasPrefix(entry["integrity"].(string), "sha512-") {
			t.Fatal(name, entry)
		}
	}
	patterns := strings.Split(imageFixtureRead(t, "Dockerfile.dockerignore"), "\n")
	for _, required := range []string{".git", ".aqp-bench", ".atl", "INIT.md", "**/.env", "**/.aws", "**/node_modules"} {
		found := false
		for _, p := range patterns {
			if p == required {
				found = true
			}
		}
		if !found {
			t.Errorf("missing build guard %s", required)
		}
	}
	readme := imageFixtureRead(t, "README.md")
	parts := strings.SplitN(readme, "```json\n", 2)
	if len(parts) != 2 {
		t.Fatal("missing documented agent configuration")
	}
	body := strings.SplitN(parts[1], "\n```", 2)[0]
	var example struct {
		Agents []struct {
			Command []string `json:"command"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body), &example); err != nil {
		t.Fatal(err)
	}
	if len(example.Agents) != 1 || strings.Join(example.Agents[0].Command, " ") != "gentle-shell --link" {
		t.Fatal(example)
	}
}
