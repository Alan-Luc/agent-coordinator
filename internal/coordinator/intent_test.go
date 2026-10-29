package coordinator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agent-coordinator/internal/pilaunch"
)

func TestInteractiveLaunchInput(t *testing.T) {
	for _, test := range []struct {
		name      string
		payload   []byte
		mutate    func(string)
		wantError bool
	}{
		{name: "literal and empty arguments", payload: []byte(`{"schema":1,"argv":["","@image.png","$(touch malicious)","--model","model name"]}`)},
		{name: "payload larger than record limit", payload: []byte(`{"schema":1,"argv":["` + strings.Repeat("x", 70000) + `"]}`)},
		{name: "missing payload", payload: []byte(`{"schema":1,"argv":[]}`), mutate: func(dir string) { _ = os.Remove(filepath.Join(dir, "launch-argv.json")) }, wantError: true},
		{name: "mixed task and interactive", payload: []byte(`{"schema":1,"argv":[]}`), mutate: func(dir string) {
			if err := writeBytes(dir, "initial-task.txt", []byte(TaskHeader+"task")); err != nil {
				panic(err)
			}
		}, wantError: true},
		{name: "hash mismatch", payload: []byte(`{"schema":1,"argv":[]}`), mutate: func(dir string) {
			if err := writeBytes(dir, "launch-argv.json", []byte(`{"schema":1,"argv":["changed"]}`)); err != nil {
				panic(err)
			}
		}, wantError: true},
		{name: "empty argv", payload: []byte(`{"schema":1,"argv":[]}`)},
		{name: "Unicode and literal escapes", payload: []byte(`{"schema":1,"argv":["é","\ud83d\ude00","\\ud800","\ufffd"]}`)},
		{name: "null argument", payload: []byte(`{"schema":1,"argv":[null]}`), wantError: true},
		{name: "null argv", payload: []byte(`{"schema":1,"argv":null}`), wantError: true},
		{name: "low surrogate", payload: []byte(`{"schema":1,"argv":["\udc00"]}`), wantError: true},
		{name: "malformed UTF-8", payload: append([]byte(`{"schema":1,"argv":["`), append([]byte{255}, []byte(`"]}`)...)...), wantError: true},
		{name: "unknown field", payload: []byte(`{"schema":1,"argv":[],"extra":true}`), wantError: true},
		{name: "trailing JSON", payload: []byte(`{"schema":1,"argv":[]} {}`), wantError: true},
		{name: "NUL", payload: []byte(`{"schema":1,"argv":["\u0000"]}`), wantError: true},
		{name: "unpaired surrogate", payload: []byte(`{"schema":1,"argv":["\ud800"]}`), wantError: true},
		{name: "argument count", payload: []byte(`{"schema":1,"argv":[` + strings.Repeat(`"",`, 256) + `""]}`), wantError: true},
		{name: "payload limit", payload: []byte(`{"schema":1,"argv":["` + strings.Repeat("x", 1<<20) + `"]}`), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			sum := sha256.Sum256(test.payload)
			intent := InteractiveIntent{Schema: 1, Kind: "interactive", TerminalProfile: "transparent-v1", ArgvSHA256: hex.EncodeToString(sum[:]), NativePi: &pilaunch.Managed{Executable: "/bin/pi", Observation: pilaunch.Observation{Version: "1.4.0"}}}
			if err := writeJSON(dir, "intent.json", intent); err != nil {
				t.Fatal(err)
			}
			if err := writeBytes(dir, "launch-argv.json", test.payload); err != nil {
				t.Fatal(err)
			}
			if test.mutate != nil {
				test.mutate(dir)
			}
			got, err := readInteractiveArguments(dir)
			if test.wantError {
				if err == nil {
					t.Fatal("invalid launch input accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var expected struct {
				Argv []string `json:"argv"`
			}
			if err := json.Unmarshal(test.payload, &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, expected.Argv) {
				t.Fatalf("argv changed: %#v", got)
			}
			if _, err := os.Stat(filepath.Join(dir, "launch-argv.json")); err != nil {
				t.Fatal("validation consumed argv before bootstrap claimed exec")
			}
		})
	}
}

func TestInteractiveIntentRejectsUnknownProfileAndSchema(t *testing.T) {
	for _, fixture := range []string{
		`{"schema":2,"kind":"interactive","terminal_profile":"transparent-v1","argv_sha256":"` + strings.Repeat("a", 64) + `"}`,
		`{"schema":1,"kind":"task","terminal_profile":"transparent-v1","argv_sha256":"` + strings.Repeat("a", 64) + `"}`,
		`{"schema":1,"kind":"interactive","terminal_profile":"unknown","argv_sha256":"` + strings.Repeat("a", 64) + `"}`,
		`{"schema":1,"kind":"interactive","terminal_profile":"transparent-v1","argv_sha256":"bad"}`,
		`{"schema":1,"kind":"interactive","terminal_profile":"transparent-v1","argv_sha256":"` + strings.Repeat("a", 64) + `","extra":1}`,
	} {
		t.Run(fixture, func(t *testing.T) {
			dir := t.TempDir()
			if err := writeBytes(dir, "intent.json", []byte(fixture)); err != nil {
				t.Fatal(err)
			}
			if _, err := readInteractiveIntent(dir); err == nil {
				t.Fatal("unsupported intent accepted")
			}
		})
	}
}
