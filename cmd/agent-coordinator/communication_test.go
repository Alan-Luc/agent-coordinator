package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCommunicationCLIRejectsIrrelevantFields(t *testing.T) {
	body := []byte(`{"protocol":1,"action":"capability","caller":{"session_file":"/private/session","session_id":"id","generation":"00000000-0000-4000-8000-000000000000","pid":123}}`)
	var out bytes.Buffer
	fail := func(error) int { t.Fatal("backend called for invalid action shape"); return 1 }
	invalid := func(string) int { return 2 }
	if code := runCommunicationBody(context.Background(), nil, []string{"_communication", "--json"}, body, &out, fail, invalid); code != 2 {
		t.Fatalf("exit %d, want strict usage error", code)
	}
}

func TestCommunicationCapabilityDoesNotCreateState(t *testing.T) {
	state := filepath.Join(t.TempDir(), "unused-state")
	t.Setenv("AC_STATE_DIR", state)
	in := inputFile(t, `{"protocol":1,"action":"capability"}`)
	var out bytes.Buffer
	if code := run([]string{"_communication", "--json"}, in, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("capability exit=%d output=%s", code, out.String())
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("capability mutated coordinator state: %v", err)
	}
}

func TestCommunicationCLIRejectsInvalidUTF8(t *testing.T) {
	body := string([]byte(`{"protocol":1,"action":"capability","text":"` + "\xff" + `"}`))
	if code := runCommunicationBody(context.Background(), nil, []string{"_communication", "--json"}, []byte(body), &bytes.Buffer{}, func(error) int { t.Fatal("backend called for invalid UTF-8"); return 1 }, func(string) int { return 2 }); code != 2 {
		t.Fatalf("invalid UTF-8 exited %d", code)
	}
}

func TestCommunicationCLIRejectsDuplicateKeys(t *testing.T) {
	for _, body := range []string{
		`{"protocol":1,"action":"capability","action":"publish"}`,
		`{"protocol":1,"action":"capability","caller":{"session_id":"a","session_id":"b"}}`,
	} {
		invalid := func(string) int { return 2 }
		if code := runCommunicationBody(context.Background(), nil, []string{"_communication", "--json"}, []byte(body), &bytes.Buffer{}, func(error) int { t.Fatal("backend called with duplicate keys"); return 1 }, invalid); code != 2 {
			t.Fatalf("duplicate-key request exited %d", code)
		}
	}
}
