// Package bench implements the internal benchmark engine. Persisted records keep
// their existing JSON shape; they are not a public Go API.
package bench

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"
)

// Record preserves the existing JSON object shape at engine and persistence boundaries.
// Values must be JSON-encodable; callers should not share mutable records without synchronization.
type Record = map[string]any

func object(v any) Record { m, _ := v.(map[string]any); return m }
func array(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []Record:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = v
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = v
		}
		return out
	default:
		return nil
	}
}
func records(v any) []Record {
	out := []Record{}
	for _, x := range array(v) {
		out = append(out, object(x))
	}
	return out
}
func text(v any) string { s, _ := v.(string); return s }
func number(v any) float64 {
	switch n := v.(type) {
	case json.Number:
		f, _ := n.Float64()
		return f
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	default:
		return 0
	}
}
func stringsOf(v any) []string {
	out := []string{}
	for _, x := range array(v) {
		out = append(out, text(x))
	}
	return out
}

// decodeJSON retains number tokens for strict integer validation and rejects
// trailing JSON values instead of accepting a valid prefix.
func decodeJSON(b []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

// cloneRecord deep-copies a JSON-encodable record through the shared decoder.
// Callers must supply JSON-safe values; encoding errors are not surfaced here.
func cloneRecord(r Record) Record {
	b, _ := json.Marshal(r)
	var out Record
	_ = decodeJSON(b, &out)
	return out
}
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	// Retain the UUID4 hexadecimal layout of existing run and attempt IDs.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b)
}
func unixTime() float64 { return float64(time.Now().UnixNano()) / 1e9 }
func defaultText(r Record, key, value string) string {
	if s := text(r[key]); s != "" {
		return s
	}
	return value
}
func seconds(r Record, key string, fallback float64) time.Duration {
	if v, ok := r[key]; ok {
		return time.Duration(number(v) * float64(time.Second))
	}
	return time.Duration(fallback * float64(time.Second))
}

// numeric reports whether v uses one of the supported numeric representations,
// excluding numeric strings and booleans.
func numeric(v any) bool {
	switch v.(type) {
	case json.Number, float64, float32, int, int64, int32:
		return true
	}
	return false
}

// integer reports whether v has an integer representation, not merely an integral value.
// A JSON token such as 1.0 is therefore rejected.
func integer(v any) bool {
	switch n := v.(type) {
	case json.Number:
		_, err := strconv.ParseInt(string(n), 10, 64)
		return err == nil
	case int, int64, int32:
		return true
	}
	return false
}

// StartOptions selects the files and environment exposed to a runtime container.
// The trusted offline demo accepts this shape without providing Docker isolation.
type StartOptions struct {
	// Home is the staged host configuration directory; empty omits its files.
	Home string
	// Environment contains only explicitly selected container variables.
	// The Docker runtime always sets HOME to /home/agent.
	Environment map[string]string
	// Evaluation is the trusted host test directory mounted at /evaluation.
	// Empty omits hidden evaluator files.
	Evaluation string
	// ReadonlyWorkspace requests read-only access for evaluator containers.
	ReadonlyWorkspace bool
	// CopyBack enables final workspace export for archive-copy transport.
	// Bind transport already shares the workspace and needs no copy-back.
	CopyBack bool
}

// ExecResult separates a command's exit status from its text output streams.
// A nonzero ReturnCode is a command outcome, not by itself a runtime transport error.
type ExecResult struct {
	ReturnCode int
	Stdout     string
	Stderr     string
}

// Process exposes a non-PTY command with separate protocol and diagnostic streams.
// Callers must drain output concurrently with waiting to avoid pipe backpressure.
type Process interface {
	// Stdin returns the input stream; closing it sends EOF without killing the process.
	Stdin() io.WriteCloser
	// Stdout returns the protocol output stream, separate from diagnostics.
	Stdout() io.ReadCloser
	// Stderr returns diagnostics, or a closed reader when capture was disabled.
	Stderr() io.ReadCloser
	// Wait joins execution and output copying, returning exit status and transport errors.
	Wait() (int, error)
	// Poll reports whether exit and output completion have been observed.
	// A false result does not prove liveness; inspection may have failed.
	Poll() (int, bool)
	// Terminate requests graceful process termination without removing its container.
	Terminate() error
	// Kill requests immediate process termination without removing its container.
	Kill() error
}

// Runtime isolates container lifecycle and command execution from orchestration.
// The caller must stop agent writes before exporting the final workspace, then
// remove the agent before starting evaluation. Process streams are never PTYs.
type Runtime interface {
	// Start creates an execution environment and returns its identity.
	// The caller owns Stop, optional ExportWorkspace, and Remove.
	Start(context.Context, string, string, StartOptions) (string, error)
	// Exec runs literal argv with a timeout and separate text output streams.
	Exec(context.Context, string, []string, time.Duration) (ExecResult, error)
	// ExecAs runs argv under an explicit numeric UID or UID:GID in container runtimes.
	ExecAs(context.Context, string, string, []string, time.Duration) (ExecResult, error)
	// Spawn attaches a machine-protocol process; the boolean enables stderr capture.
	Spawn(context.Context, string, []string, bool) (Process, error)
	// Address returns the container address used by a host-side protocol client.
	Address(string) (string, error)
	// TransportFor identifies how the runtime exposes workspace files.
	TransportFor(string) string
	// ExportWorkspace copies final files back to the original workspace after Stop.
	// Transports that already share host files need no export.
	ExportWorkspace(context.Context, string, string) error
	// Stop quiesces execution before snapshot export and independent evaluation.
	Stop(context.Context, string) error
	// Remove releases the execution environment, not the retained host workspace.
	Remove(context.Context, string) error
}

// EventHandler receives normalized lifecycle and transcript events synchronously.
// Handlers must return promptly so protocol readers can continue.
type EventHandler func(Record)

// AskHandler resolves one human request using its complete metadata.
// It must honor cancellation; a nil reply represents cancellation of the request.
type AskHandler func(context.Context, string, Record) (any, error)

// AgentAdapter runs one prompt against an existing runtime container and emits
// observed events. Implementations must not equate prompt acceptance with completion.
type AgentAdapter interface {
	// Run handles a prompt with event and human-reply callbacks until completion or cancellation.
	Run(context.Context, string, string, EventHandler, AskHandler) error
}
