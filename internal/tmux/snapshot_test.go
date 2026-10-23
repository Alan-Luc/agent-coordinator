package tmux

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectAcceptsLivePaneFacts(t *testing.T) {
	const job = "26dcbc36-2ad2-4db1-84f3-8d9d3c171eb6"
	const captured = "f1a7022b9a032fca|" + job + "|1|ac-" + job + "|%0|14815|14816|0||0|120|30\n"
	binary := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s' '"+captured+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	client := Client{Executable: binary}
	snapshot, err := client.Inspect(context.Background(), "/private/test/socket", "ac-"+job)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Owner != "f1a7022b9a032fca" || snapshot.Job != job || snapshot.PaneDead || snapshot.ExitStatus != nil || snapshot.ServerPID != 14815 || snapshot.PanePID != 14816 || snapshot.Width != 120 || snapshot.Height != 30 {
		t.Fatalf("captured live facts changed: %+v", snapshot)
	}
}
