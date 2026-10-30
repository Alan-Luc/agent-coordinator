package main

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"coordinate/internal/coordinator"
)

func runManagerHost(ctx context.Context, c *coordinator.Coordinator, args []string, in io.Reader, out io.Writer, fail func(error) int, invalid func(string) int) int {
	if len(args) != 2 || args[1] != "--json" {
		return invalid("usage: coordinate _manager-host --json")
	}
	var request struct {
		Target *coordinator.SessionTarget `json:"target"`
	}
	decoder := json.NewDecoder(io.LimitReader(in, 65537))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.Target == nil {
		return invalid("invalid manager-host request")
	}
	role, err := c.ReserveManagerHost(ctx, *request.Target, os.Getppid())
	if err != nil {
		return fail(err)
	}
	if err := json.NewEncoder(out).Encode(struct {
		Role coordinator.JobRole `json:"role"`
	}{Role: role}); err != nil {
		return 1
	}
	return 0
}
