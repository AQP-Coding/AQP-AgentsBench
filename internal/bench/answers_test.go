package bench

import "testing"

func TestHumanAnswerCoercion(t *testing.T) {
	for _, tt := range []struct {
		input string
		meta  Record
		want  any
	}{
		{"yes", Record{"method": "confirm"}, true}, {"false", Record{"method": "confirm"}, false},
		{"once", Record{"permission": "bash"}, "once"}, {"null", Record{"method": "input"}, nil},
		{"first", Record{"method": "select", "options": []any{"first", "second"}}, "first"},
	} {
		got, err := CoerceAnswer(tt.input, tt.meta)
		if err != nil || got != tt.want {
			t.Errorf("%q got %v,%v", tt.input, got, err)
		}
	}
	got, err := CoerceAnswer(`[["first"]]`, Record{"questions": []any{Record{"question": "Choose"}}})
	if err != nil || text(array(array(got)[0])[0]) != "first" {
		t.Fatal(got, err)
	}
	for _, tt := range []struct {
		input string
		meta  Record
	}{
		{"maybe", Record{"method": "confirm"}}, {"yes", Record{"permission": "bash"}},
		{"other", Record{"method": "select", "options": []any{"first"}}},
		{`[["other"]]`, Record{"questions": []any{Record{"custom": false, "options": []any{Record{"label": "first"}}}}}},
		{`[["a","b"]]`, Record{"questions": []any{Record{"multiple": false}}}},
		{`[]`, Record{"questions": []any{Record{}}}},
	} {
		if _, err := CoerceAnswer(tt.input, tt.meta); err == nil {
			t.Errorf("accepted %q", tt.input)
		}
	}
}

func TestRedactNestedKnownSecrets(t *testing.T) {
	input := Record{"text": "provider-key token", "nested": []any{Record{"value": "token"}}}
	got := object(Redact(input, []string{"provider-key", "token"}))
	if got["text"] != "[redacted] [redacted]" || object(array(got["nested"])[0])["value"] != "[redacted]" {
		t.Fatal(got)
	}
	if input["text"] != "provider-key token" {
		t.Fatal("redaction mutated source")
	}
}
