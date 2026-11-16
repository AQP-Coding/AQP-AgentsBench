package bench

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	harnessSecretNames = map[string]bool{
		"auth.json": true, ".env": true, "credentials": true,
		"credentials.json": true, "id_rsa": true, "id_ed25519": true,
	}
	harnessSecretKeys = map[string]bool{
		"apikey": true, "api_key": true, "token": true, "password": true,
		"secret": true, "access_token": true, "refresh_token": true,
	}
	privateKeyPattern = []byte("PRIVATE KEY-----")
	apiKeyPattern     = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`)
	frontMatterModel  = regexp.MustCompile(`(?m)^model:\s*([^\n]+)`)
)

func checkHarnessFile(filename string) error {
	info, err := os.Lstat(filename)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Harness symlinks are unsupported; select real files")
	}
	base := filepath.Base(filename)
	if harnessSecretNames[strings.ToLower(base)] || strings.HasPrefix(base, ".env.") {
		return fmt.Errorf("Harness credential/secret files must not be inherited")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Harness entries must be regular files or directories")
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	if strings.Contains(string(data), string(privateKeyPattern)) || apiKeyPattern.Match(data) {
		return fmt.Errorf("Harness secret-bearing files must not be inherited")
	}
	if filepath.Ext(filename) == ".json" {
		var value any
		if err := decodeJSON(data, &value); err != nil {
			return fmt.Errorf("Harness JSON must be valid UTF-8 JSON")
		}
		if err := inspectHarnessSecrets(value); err != nil {
			return err
		}
	}
	return nil
}

func inspectHarnessSecrets(value any) error {
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			if harnessSecretKeys[strings.ToLower(key)] && !jsonFalsy(child) {
				textValue, ok := child.(string)
				if !ok || !(strings.HasPrefix(textValue, "{env:") || strings.HasPrefix(textValue, "$")) {
					return fmt.Errorf("Harness contains literal credentials; use explicit environment references")
				}
			}
			if err := inspectHarnessSecrets(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range item {
			if err := inspectHarnessSecrets(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func jsonFalsy(value any) bool {
	switch item := value.(type) {
	case nil:
		return true
	case bool:
		return !item
	case string:
		return item == ""
	case json.Number:
		number, err := item.Float64()
		return err == nil && number == 0
	case float64:
		return item == 0
	case []any:
		return len(item) == 0
	case map[string]any:
		return len(item) == 0
	default:
		return false
	}
}

func planHarnessFiles(files []Record, destination string) (string, []copyPlan, error) {
	root, err := resolveExisting(destination)
	if err != nil {
		return "", nil, err
	}
	plans := make([]copyPlan, 0, len(files))
	for _, entry := range files {
		targetName, err := safeRelativeTarget(text(entry["target"]))
		if err != nil {
			return "", nil, err
		}
		source := text(entry["source"])
		info, err := os.Lstat(source)
		if err != nil {
			return "", nil, fmt.Errorf("Harness sources must exist")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", nil, fmt.Errorf("Harness symlinks are unsupported")
		}
		plan := copyPlan{source: source, target: filepath.Join(root, filepath.FromSlash(targetName)), dir: info.IsDir()}
		if plan.dir {
			err = filepath.WalkDir(source, func(filename string, item os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if filename == source {
					return nil
				}
				if item.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("Harness symlinks are unsupported")
				}
				if item.IsDir() {
					return nil
				}
				return checkHarnessFile(filename)
			})
		} else {
			err = checkHarnessFile(source)
		}
		if err != nil {
			return "", nil, err
		}
		if err := rejectSymlinkAncestors(root, plan.target); err != nil {
			return "", nil, err
		}
		if err := rejectSourceTargetOverlap(plan); err != nil {
			return "", nil, err
		}
		if _, err := os.Lstat(plan.target); err == nil {
			return "", nil, fmt.Errorf("Harness targets must not overlap existing or selected files")
		} else if !os.IsNotExist(err) {
			return "", nil, err
		}
		for _, old := range plans {
			if plan.target == old.target || inside(plan.target, old.target) || inside(old.target, plan.target) {
				return "", nil, fmt.Errorf("Harness targets must not overlap existing or selected files")
			}
		}
		plans = append(plans, plan)
	}
	return root, plans, nil
}

// StageHarness copies only explicitly selected files after validating every
// source and destination, including nested symlinks and literal secrets.
func StageHarness(files []Record, destination string) error {
	root, plans, err := planHarnessFiles(files, destination)
	if err != nil {
		return err
	}
	_, err = copyPlanned(plans, root)
	return err
}

func validateGatewayURL(gatewayURL string) error {
	parsed, err := url.Parse(gatewayURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.TrimRight(parsed.Path, "/") != "/v1" {
		return fmt.Errorf("Gateway URL must be an absolute HTTP(S) /v1 endpoint")
	}
	return nil
}

func checkConfigAncestors(filename string) error {
	current := filepath.Clean(filename)
	for {
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Configuration destination cannot contain symlinks")
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

func readConfig(filename string) (Record, error) {
	data, err := os.ReadFile(filename)
	if os.IsNotExist(err) {
		return Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	var config Record
	if err := decodeJSON(data, &config); err != nil {
		return nil, err
	}
	if config == nil {
		return nil, fmt.Errorf("Harness configuration must be a JSON object")
	}
	return config, nil
}

func validateHarnessModels(value any, model string) error {
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			if key == "model" || key == "small_model" {
				if modelValue, ok := child.(string); ok && modelValue != model && modelValue != "aqp/"+model {
					return fmt.Errorf("Selected harness model conflicts with benchmark provider")
				}
			}
			if key == "defaultProvider" && child != "aqp" {
				return fmt.Errorf("Selected harness provider conflicts with benchmark gateway")
			}
			if err := validateHarnessModels(child, model); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range item {
			if err := validateHarnessModels(child, model); err != nil {
				return err
			}
		}
	}
	return nil
}

// ConfigureHarness adds the mandatory gateway provider while keeping the
// credential itself only in the returned environment map.
func ConfigureHarness(home, kind, model, gatewayURL, token string) (map[string]string, error) {
	if kind != "pi" && kind != "opencode" {
		return nil, fmt.Errorf("Unsupported agent kind: %s", kind)
	}
	if err := validateGatewayURL(gatewayURL); err != nil {
		return nil, err
	}
	env := map[string]string{"HOME": "/home/agent", "AQP_GATEWAY_TOKEN": token}
	var filename string
	var generated Record
	if kind == "pi" {
		filename = filepath.Join(home, ".pi", "agent", "models.json")
		provider := Record{
			"baseUrl": gatewayURL, "api": "openai-completions", "apiKey": "$AQP_GATEWAY_TOKEN",
			"models": []any{Record{"id": model, "name": model}},
		}
		generated = Record{"providers": Record{"aqp": provider}}
		env["PI_CODING_AGENT_DIR"] = "/home/agent/.pi/agent"
	} else {
		filename = filepath.Join(home, ".config", "opencode", "opencode.json")
		provider := Record{
			"npm": "@ai-sdk/openai-compatible", "name": "Benchmark gateway",
			"options": Record{"baseURL": gatewayURL, "apiKey": "{env:AQP_GATEWAY_TOKEN}"},
			"models":  Record{model: Record{"name": model}},
		}
		generated = Record{
			"provider": Record{"aqp": provider}, "enabled_providers": []any{"aqp"},
			"model": "aqp/" + model, "small_model": "aqp/" + model,
		}
		env["XDG_CONFIG_HOME"] = "/home/agent/.config"
		env["XDG_DATA_HOME"] = "/home/agent/.local/share"
		env["OPENCODE_CONFIG"] = "/home/agent/.config/opencode/opencode.json"
		env["OPENCODE_DISABLE_AUTOUPDATE"] = "true"
	}
	for _, conflict := range []string{
		filepath.Join(home, ".config", "opencode", "opencode.jsonc"),
		filepath.Join(home, ".local", "share", "opencode", "auth.json"),
		filepath.Join(home, ".pi", "agent", "auth.json"),
	} {
		if _, err := os.Lstat(conflict); err == nil {
			return nil, fmt.Errorf("Remove incompatible credential/JSONC configuration from selected harness")
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if err := checkConfigAncestors(filename); err != nil {
		return nil, err
	}
	existing, err := readConfig(filename)
	if err != nil {
		return nil, err
	}
	if err := validateHarnessModels(existing, model); err != nil {
		return nil, err
	}
	key := "provider"
	if kind == "pi" {
		key = "providers"
	}
	if !jsonFalsy(existing[key]) {
		oldBytes, _ := json.Marshal(existing[key])
		newBytes, _ := json.Marshal(generated[key])
		if string(oldBytes) != string(newBytes) {
			return nil, fmt.Errorf("Selected harness provider configuration conflicts with mandatory benchmark gateway")
		}
	}
	if kind == "opencode" {
		for _, name := range []string{"model", "small_model", "enabled_providers"} {
			if old, ok := existing[name]; ok {
				oldBytes, _ := json.Marshal(old)
				newBytes, _ := json.Marshal(generated[name])
				if string(oldBytes) != string(newBytes) {
					return nil, fmt.Errorf("Selected harness %s conflicts with mandatory benchmark provider", name)
				}
			}
		}
	}
	var settings Record
	var settingsPath string
	if kind == "pi" {
		settingsPath = filepath.Join(home, ".pi", "agent", "settings.json")
		if err := checkConfigAncestors(settingsPath); err != nil {
			return nil, err
		}
		settings, err = readConfig(settingsPath)
		if err != nil {
			return nil, err
		}
		defaults := Record{"defaultProvider": "aqp", "defaultModel": model, "enabledModels": []any{"aqp/" + model}}
		for name, value := range defaults {
			if existingValue, ok := settings[name]; ok {
				oldBytes, _ := json.Marshal(existingValue)
				newBytes, _ := json.Marshal(value)
				if string(oldBytes) != string(newBytes) {
					return nil, fmt.Errorf("Selected harness %s conflicts with benchmark provider", name)
				}
			}
		}
		for name, value := range defaults {
			settings[name] = value
		}
	} else {
		content, err := json.Marshal(generated)
		if err != nil {
			return nil, err
		}
		env["OPENCODE_CONFIG_CONTENT"] = string(content)
	}
	for name, value := range generated {
		existing[name] = value
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return nil, err
	}
	if err := writeJSON(filename, existing); err != nil {
		return nil, err
	}
	if kind == "pi" {
		if err := writeJSON(settingsPath, settings); err != nil {
			return nil, err
		}
	}
	return env, nil
}

func writeJSON(filename string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filename, data, 0o644)
}

// ValidateWorkspace rejects recognized project-level provider and model
// overrides that would bypass the injected benchmark gateway.
func ValidateWorkspace(workspace, kind, model string) error {
	root := filepath.Clean(workspace)
	var candidates []string
	configDirectory := ".pi"
	if kind == "opencode" {
		candidates = []string{filepath.Join(root, "opencode.json"), filepath.Join(root, "opencode.jsonc")}
		configDirectory = ".opencode"
	} else {
		candidates = []string{filepath.Join(root, ".pi", "settings.json"), filepath.Join(root, ".pi", "models.json")}
	}
	directory := filepath.Join(root, configDirectory)
	if info, err := os.Lstat(directory); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Project provider configuration cannot be a symlink")
	} else if err == nil && info.IsDir() {
		err = filepath.WalkDir(directory, func(filename string, item os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if item.IsDir() {
				return nil
			}
			if item.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("Project provider configuration cannot be a symlink")
			}
			switch filepath.Ext(filename) {
			case ".json", ".jsonc":
				candidates = append(candidates, filename)
			case ".md":
				data, err := os.ReadFile(filename)
				if err != nil {
					return err
				}
				content := string(data)
				if strings.HasPrefix(content, "---") {
					front := strings.SplitN(content, "---", 3)
					if len(front) > 2 {
						for _, match := range frontMatterModel.FindAllStringSubmatch(front[1], -1) {
							if strings.Trim(strings.TrimSpace(match[1]), "\"'") != "aqp/"+model {
								return fmt.Errorf("Project agent model conflicts with benchmark provider")
							}
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	seen := map[string]bool{}
	for _, filename := range candidates {
		if seen[filename] {
			continue
		}
		seen[filename] = true
		info, err := os.Lstat(filename)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Project provider configuration cannot be a symlink")
		}
		if filepath.Ext(filename) == ".jsonc" {
			return fmt.Errorf("Project JSONC routing cannot be validated; use explicit JSON")
		}
		config, err := readConfig(filename)
		if err != nil {
			return err
		}
		if err := validateHarnessModels(config, model); err != nil {
			return err
		}
		if len(object(config["provider"])) > 0 || len(object(config["providers"])) > 0 {
			return fmt.Errorf("Project provider configuration conflicts with mandatory benchmark gateway")
		}
	}
	return nil
}
