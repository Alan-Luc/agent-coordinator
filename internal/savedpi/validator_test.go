package savedpi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "saved.jsonl")
	body := `{"type":"session","version":3,"id":"native_session.id-1","timestamp":"2026-10-29T00:00:00Z","cwd":"` + filepath.ToSlash(dir) + `"}` + "\n" + `{"type":"message","id":"entry1","parentId":null,"timestamp":"2026-10-29T00:00:00Z","message":{"role":"user","content":"hello","timestamp":1793232000000}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateAndRecheck(t *testing.T) {
	path := fixture(t)
	s, err := Validate(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.NativeID != "native_session.id-1" || s.Bytes <= 0 || len(s.SHA256) != 64 {
		t.Fatalf("unexpected snapshot: %+v", s)
	}
	if err := Recheck(s); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(" ")
	_ = f.Close()
	if err := Recheck(s); err == nil {
		t.Fatal("Recheck accepted changed bytes")
	}
}

func TestRecheckRejectsPathReplacement(t *testing.T) {
	path := fixture(t)
	s, err := Validate(path)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0644); err != nil {
		t.Fatal(err)
	}
	if err := Recheck(s); err == nil {
		t.Fatal("Recheck accepted a replacement inode at the same path")
	}
}

func TestValidateRejectsMalformedAndUnsafeFiles(t *testing.T) {
	cases := []struct{ name, body string }{
		{"empty", ""},
		{"legacy", `{"type":"session","version":2,"id":"abc","cwd":"/"}` + "\n"},
		{"truncated", `{"type":"session","version":3`},
		{"duplicate-header", `{"type":"session","version":3,"id":"abc","cwd":"/"}` + "\n" + `{"type":"session"}` + "\n"},
		{"forward-parent", `{"type":"session","version":3,"id":"abc","cwd":"/"}` + "\n" + `{"type":"message","id":"one","parentId":"later","message":{}}` + "\n"},
		{"unknown-entry", `{"type":"session","version":3,"id":"abc","cwd":"/"}` + "\n" + `{"type":"future","id":"one","parentId":null}` + "\n"},
		{"empty-message", `{"type":"session","version":3,"id":"abc","cwd":"/"}` + "\n" + `{"type":"message","id":"one","parentId":null,"message":{}}` + "\n"},
		{"bad-model-types", `{"type":"session","version":3,"id":"abc","cwd":"/"}` + "\n" + `{"type":"model_change","id":"one","parentId":null,"provider":42,"modelId":[]}` + "\n"},
		{"bad-custom-display", `{"type":"session","version":3,"id":"abc","cwd":"/"}` + "\n" + `{"type":"custom_message","id":"one","parentId":null,"customType":"x","content":"ok","display":"yes"}` + "\n"},
		{"no-newline", `{"type":"session","version":3,"id":"abc","cwd":"/"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, "x")
			if err := os.WriteFile(p, []byte(tc.body), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := Validate(p); err == nil {
				t.Fatal("accepted invalid file")
			}
		})
	}
	p := fixture(t)
	link := p + "-link"
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(link); err == nil {
		t.Fatal("accepted symlink")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	unsafe := filepath.Join(dir, "unsafe")
	_ = os.WriteFile(unsafe, []byte(strings.Repeat("x", maxLine+1)), 0644)
	if _, err := Validate(unsafe); err == nil {
		t.Fatal("accepted overlong line")
	}
	longLine := filepath.Join(dir, "long-line")
	large := `{"type":"session","version":3,"id":"abc","cwd":"/"}` + "\n" + `{"type":"custom","id":"one","parentId":null,"customType":"x","data":"` + strings.Repeat("x", maxLine) + `"}` + "\n"
	if err := os.WriteFile(longLine, []byte(large), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(longLine); err == nil {
		t.Fatal("accepted oversized JSONL line")
	}
}
