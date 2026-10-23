package coordinator

import (
	"context"
	"testing"
)

func TestAttachRefusesOuterTmuxBeforeAccessingState(t *testing.T) {
	t.Setenv("TMUX", "/tmp/outer,1,0")
	c := &Coordinator{}
	if _, err := c.Attach(context.Background(), "job", false); err == nil {
		t.Fatal("Attach allowed terminal changes from inside tmux")
	}
}
