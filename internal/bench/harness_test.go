package bench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageHarnessPreservesSelectedFilesAndBytes(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "selected")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("instructions\n\x00bytes")
	if err := os.WriteFile(filepath.Join(source, "AGENTS.md"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "home")
	if err := StageHarness([]Record{{"source": source, "target": ".pi/./agent//skills"}}, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, ".pi", "agent", "skills", "AGENTS.md"))
	if err != nil || string(got) != string(content) {
		t.Fatalf("staged content = %q, err %v", got, err)
	}
}

func TestStageHarnessRejectsUnsafeTargetsSecretsAndSymlinksBeforeCopy(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "instructions")
	if err := os.WriteFile(source, []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "home")
	for _, target := range []string{"../escape", "/absolute", ".", "x/../../escape"} {
		if err := StageHarness([]Record{{"source": source, "target": target}}, destination); err == nil {
			t.Errorf("StageHarness(%q) unexpectedly succeeded", target)
		}
		if _, err := os.Stat(destination); !os.IsNotExist(err) {
			t.Fatalf("unsafe target created destination: %v", err)
		}
	}
	secret := filepath.Join(root, "settings.json")
	if err := os.WriteFile(secret, []byte(`{"apiKey":"literal-secret"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := StageHarness([]Record{{"source": secret, "target": "settings.json"}}, destination); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("literal JSON credential error = %v", err)
	}
	secretName := filepath.Join(root, "auth.json")
	if err := os.WriteFile(secretName, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := StageHarness([]Record{{"source": secretName, "target": "auth.json"}}, destination); err == nil {
		t.Fatal("StageHarness accepted a credential filename")
	}
	if err := StageHarness([]Record{{"source": source, "target": "selected.txt"}, {"source": secretName, "target": "auth.json"}}, destination); err == nil {
		t.Fatal("StageHarness accepted a batch containing a credential file")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("later invalid source left an earlier file staged: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := StageHarness([]Record{{"source": link, "target": "link"}}, destination); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink source error = %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("invalid sources created destination: %v", err)
	}
	selected := filepath.Join(root, "selected")
	if err := os.Mkdir(selected, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(selected, "auth.json"), []byte(`{"apiKey":"secret"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := StageHarness([]Record{{"source": selected, "target": ".pi/agent"}}, destination); err == nil {
		t.Fatal("StageHarness accepted a directory containing auth.json")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("secret-bearing directory created destination: %v", err)
	}
	if err := os.Remove(filepath.Join(selected, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source, filepath.Join(selected, "nested-link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := StageHarness([]Record{{"source": selected, "target": ".pi/agent"}}, destination); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("directory symlink error = %v", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("directory symlink created destination: %v", err)
	}
}

func TestConfigureHarnessRoutesGatewayWithoutPersistingToken(t *testing.T) {
	for _, test := range []struct {
		kind string
		path string
	}{
		{kind: "pi", path: ".pi/agent/models.json"},
		{kind: "opencode", path: ".config/opencode/opencode.json"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			home := t.TempDir()
			env, err := ConfigureHarness(home, test.kind, "org/model", "http://host.docker.internal:1234/v1", "ephemeral-token")
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(home, test.path))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "ephemeral-token") || !strings.Contains(string(data), "host.docker.internal:1234/v1") || !strings.Contains(string(data), "org/model") {
				t.Fatalf("unexpected generated config: %s", data)
			}
			if env["AQP_GATEWAY_TOKEN"] != "ephemeral-token" || env["HOME"] != "/home/agent" {
				t.Fatalf("gateway environment = %#v", env)
			}
			if test.kind == "pi" {
				settings, err := os.ReadFile(filepath.Join(home, ".pi", "agent", "settings.json"))
				if err != nil || !strings.Contains(string(settings), `"defaultProvider": "aqp"`) || !strings.Contains(string(settings), `"defaultModel": "org/model"`) {
					t.Fatalf("Pi settings = %s, err %v", settings, err)
				}
			}
		})
	}
}

func TestConfigureHarnessPreservesSettingsAndRejectsProviderConflicts(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, ".config", "opencode", "opencode.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"instructions":["AGENTS.md"],"plugin":["gentle-ai"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfigureHarness(home, "opencode", "org/model", "http://gateway/v1", "local"); err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	data, err := os.ReadFile(configPath)
	if err != nil || json.Unmarshal(data, &config) != nil {
		t.Fatalf("read generated config: %v", err)
	}
	if len(array(config["plugin"])) != 1 {
		t.Fatalf("existing plugin setting was lost: %#v", config)
	}
	conflict := []byte(`{"provider":{"other":{}}}`)
	if err := os.WriteFile(configPath, conflict, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfigureHarness(home, "opencode", "org/model", "http://gateway/v1", "local"); err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("provider conflict error = %v", err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil || string(after) != string(conflict) {
		t.Fatalf("provider conflict modified config: %q, err %v", after, err)
	}
}

func TestConfigureHarnessRejectsSymlinkEscapeAndUnsupportedKindWithoutWriting(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".pi")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ConfigureHarness(home, "pi", "org/model", "http://gateway/v1", "local"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink destination error = %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink escape wrote outside files: %v, err %v", entries, err)
	}
	newHome := filepath.Join(root, "new-home")
	if _, err := ConfigureHarness(newHome, "other", "org/model", "http://gateway/v1", "local"); err == nil || !strings.Contains(err.Error(), "Unsupported") {
		t.Fatalf("unsupported kind error = %v", err)
	}
	if _, err := os.Stat(newHome); !os.IsNotExist(err) {
		t.Fatalf("unsupported kind created home: %v", err)
	}
}

func TestValidateWorkspaceRejectsKnownRoutingOverrides(t *testing.T) {
	root := t.TempDir()
	if err := ValidateWorkspace(root, "opencode", "org/model"); err != nil {
		t.Fatalf("empty workspace rejected: %v", err)
	}
	config := filepath.Join(root, "opencode.json")
	if err := os.WriteFile(config, []byte(`{"provider":{"other":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspace(root, "opencode", "org/model"); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("provider override error = %v", err)
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".opencode", "agent.md"), []byte("---\nmodel: other/model\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspace(root, "opencode", "org/model"); err == nil || !strings.Contains(err.Error(), "Project agent model") {
		t.Fatalf("agent model override error = %v", err)
	}
}

func TestStageHarnessRejectsSourceTargetOverlap(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "instructions.md"), []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := StageHarness([]Record{{"source": directory, "target": "copied"}}, directory); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("source/target overlap error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "copied")); !os.IsNotExist(err) {
		t.Fatalf("overlapping directory was copied into itself: %v", err)
	}
}
