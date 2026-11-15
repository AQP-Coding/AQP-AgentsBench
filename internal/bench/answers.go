package bench

import (
	"fmt"
	"strings"
)

// CoerceAnswer validates a raw operator reply using the request metadata.
// The exact string "null" cancels any request. Confirmation replies become booleans;
// structured questions require JSON arrays of string arrays. Other accepted replies
// remain strings without trimming.
func CoerceAnswer(reply string, metadata Record) (any, error) {
	if reply == "null" {
		return nil, nil
	}
	if metadata["method"] == "confirm" {
		switch strings.ToLower(reply) {
		case "true", "yes":
			return true, nil
		case "false", "no":
			return false, nil
		}
		return nil, fmt.Errorf("Confirmation requires yes/no or true/false")
	}
	if _, ok := metadata["permission"]; ok {
		if reply != "once" && reply != "always" && reply != "reject" {
			return nil, fmt.Errorf("Permission reply must be once, always, or reject")
		}
		return reply, nil
	}
	if questions, ok := metadata["questions"]; ok {
		var value any
		if err := decodeJSON([]byte(reply), &value); err != nil {
			return nil, fmt.Errorf("Reply must be JSON: one string array per question")
		}
		answers := array(value)
		if answers == nil || len(answers) != len(array(questions)) {
			return nil, fmt.Errorf("Reply requires one answer array per question")
		}
		for i, item := range answers {
			selection := array(item)
			if selection == nil {
				return nil, fmt.Errorf("Each question answer must be an array of strings")
			}
			q := object(array(questions)[i])
			if q["multiple"] != true && len(selection) > 1 {
				return nil, fmt.Errorf("Question permits only one selection")
			}
			labels := map[string]bool{}
			for _, o := range records(q["options"]) {
				labels[text(o["label"])] = true
			}
			for _, answer := range selection {
				s, ok := answer.(string)
				if !ok {
					return nil, fmt.Errorf("Each question answer must be an array of strings")
				}
				if q["custom"] == false && !labels[s] {
					return nil, fmt.Errorf("Answer must match a presented option")
				}
			}
		}
		return value, nil
	}
	if metadata["method"] == "select" {
		found := false
		for _, option := range stringsOf(metadata["options"]) {
			if option == reply {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("Reply must match a presented option")
		}
	}
	return reply, nil
}

// Redact creates a separate JSON-shaped value so callers never mutate records
// shared with protocol readers or live attempt accounting.
func Redact(value any, secrets []string) any {
	switch v := value.(type) {
	case string:
		for _, secret := range secrets {
			if secret != "" {
				v = strings.ReplaceAll(v, secret, "[redacted]")
			}
		}
		return v
	case map[string]any:
		out := Record{}
		for k, item := range v {
			out[k] = Redact(item, secrets)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = Redact(item, secrets)
		}
		return out
	case []Record:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = Redact(item, secrets)
		}
		return out
	case []string:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = Redact(item, secrets)
		}
		return out
	default:
		return value
	}
}
