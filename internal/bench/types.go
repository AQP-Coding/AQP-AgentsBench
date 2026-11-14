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
func numeric(v any) bool {
	switch v.(type) {
	case json.Number, float64, float32, int, int64, int32:
		return true
	}
	return false
}
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

// StartOptions are shared by the Docker engine and the trusted offline demo.
type StartOptions struct {
	Home              string
	Environment       map[string]string
	Evaluation        string
	ReadonlyWorkspace bool
	CopyBack          bool
}
type ExecResult struct {
	ReturnCode int
	Stdout     string
	Stderr     string
}

// Process streams remain separate. Machine protocols must not use a PTY.
type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Wait() (int, error)
	Poll() (int, bool)
	Terminate() error
	Kill() error
}
type Runtime interface {
	Start(context.Context, string, string, StartOptions) (string, error)
	Exec(context.Context, string, []string, time.Duration) (ExecResult, error)
	ExecAs(context.Context, string, string, []string, time.Duration) (ExecResult, error)
	Spawn(context.Context, string, []string, bool) (Process, error)
	Address(string) (string, error)
	TransportFor(string) string
	ExportWorkspace(context.Context, string, string) error
	Stop(context.Context, string) error
	Remove(context.Context, string) error
}
type EventHandler func(Record)
type AskHandler func(context.Context, string, Record) (any, error)
type AgentAdapter interface {
	Run(context.Context, string, string, EventHandler, AskHandler) error
}
