package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"coordinate/internal/coordinator"
)

func runCommunicationBody(ctx context.Context, c *coordinator.Coordinator, args []string, body []byte, out io.Writer, fail func(error) int, invalid func(string) int) int {
	if len(args) != 2 || args[1] != "--json" {
		return invalid("_communication requires --json")
	}
	if !utf8.Valid(body) {
		return invalid("communication request is not UTF-8")
	}
	if !hasUniqueJSONKeys(body) {
		return invalid("duplicate JSON field")
	}
	var shape map[string]json.RawMessage
	d := json.NewDecoder(strings.NewReader(string(body)))
	if d.Decode(&shape) != nil || shape == nil || d.Decode(new(any)) != io.EOF {
		return invalid("invalid communication request")
	}
	var req coordinator.CommunicationRequest
	requestDecoder := json.NewDecoder(strings.NewReader(string(body)))
	requestDecoder.DisallowUnknownFields()
	if err := requestDecoder.Decode(&req); err != nil || requestDecoder.Decode(new(any)) != io.EOF {
		return invalid("invalid communication request")
	}
	if req.Protocol != coordinator.CommunicationProtocol {
		return invalid("unsupported communication protocol")
	}
	fields := map[string][]string{"capability": {"protocol", "action"}, "enroll": {"protocol", "action", "caller"}, "reconnect": {"protocol", "action", "caller"}, "pause": {"protocol", "action", "caller"}, "disconnect": {"protocol", "action", "caller"}, "adopt": {"protocol", "action", "caller", "job_id", "target"}, "release": {"protocol", "action", "caller", "job_id"}, "start": {"protocol", "action", "caller", "name", "cwd", "task"}, "publish": {"protocol", "action", "caller", "message_id", "kind", "text"}, "inbox": {"protocol", "action", "caller", "cursor"}, "attempt": {"protocol", "action", "caller", "message_ids"}, "ack": {"protocol", "action", "caller", "message_ids"}, "escalate": {"protocol", "action", "caller", "message_id", "note"}, "resolve": {"protocol", "action", "caller", "message_id", "note"}, "reply": {"protocol", "action", "caller", "message_id", "request_id", "text"}}
	allowed, ok := fields[req.Action]
	if !ok {
		return invalid("unknown communication action")
	}
	set := map[string]bool{}
	for _, f := range allowed {
		set[f] = true
	}
	for key := range shape {
		if !set[key] {
			return invalid("unexpected field for communication action")
		}
	}
	required := map[string][]string{"capability": {"protocol", "action"}, "enroll": {"protocol", "action", "caller"}, "reconnect": {"protocol", "action", "caller"}, "pause": {"protocol", "action", "caller"}, "disconnect": {"protocol", "action", "caller"}, "adopt": {"protocol", "action", "caller", "job_id", "target"}, "release": {"protocol", "action", "caller", "job_id"}, "start": {"protocol", "action", "caller", "name", "cwd", "task"}, "publish": {"protocol", "action", "caller", "message_id", "kind", "text"}, "inbox": {"protocol", "action", "caller"}, "attempt": {"protocol", "action", "caller", "message_ids"}, "ack": {"protocol", "action", "caller", "message_ids"}, "escalate": {"protocol", "action", "caller", "message_id", "note"}, "resolve": {"protocol", "action", "caller", "message_id", "note"}, "reply": {"protocol", "action", "caller", "message_id", "request_id", "text"}}
	for _, key := range required[req.Action] {
		if _, exists := shape[key]; !exists {
			return invalid("missing required communication field")
		}
	}
	var result coordinator.CommunicationResult
	if req.Action == "capability" {
		result = coordinator.CommunicationResult{Capability: &coordinator.CommunicationCapability{Protocol: coordinator.CommunicationProtocol}}
	} else {
		if c == nil {
			return invalid("coordinator unavailable")
		}
		var callErr error
		result, callErr = c.Communication(ctx, req, os.Getppid())
		if callErr != nil {
			return fail(callErr)
		}
	}
	if err := json.NewEncoder(out).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, "communication response unavailable")
		return 1
	}
	return 0
}
func isCapabilityRequest(body []byte) bool {
	var request struct {
		Action string `json:"action"`
	}
	return hasUniqueJSONKeys(body) && json.Unmarshal(body, &request) == nil && request.Action == "capability"
}

func hasUniqueJSONKeys(body []byte) bool {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	first, err := decoder.Token()
	if err != nil || !consumeUniqueJSONValue(decoder, first) {
		return false
	}
	_, err = decoder.Token()
	return err == io.EOF
}

func consumeUniqueJSONValue(decoder *json.Decoder, token json.Token) bool {
	delim, ok := token.(json.Delim)
	if !ok {
		return true
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return false
			}
			key, ok := keyToken.(string)
			if !ok || keys[key] {
				return false
			}
			keys[key] = true
			value, err := decoder.Token()
			if err != nil || !consumeUniqueJSONValue(decoder, value) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim('}')
	case '[':
		for decoder.More() {
			value, err := decoder.Token()
			if err != nil || !consumeUniqueJSONValue(decoder, value) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim(']')
	default:
		return false
	}
}

func runMessages(ctx context.Context, c *coordinator.Coordinator, args []string, out io.Writer, fail func(error) int, invalid func(string) int) int {
	if len(args) == 0 || args[0] != "--json" {
		return invalid("messages requires --json")
	}
	cursor := 0
	if len(args) > 1 {
		if len(args) != 3 || args[1] != "--cursor" {
			return invalid("messages accepts --cursor N")
		}
		n, err := strconv.Atoi(args[2])
		if err != nil || n < 0 {
			return invalid("invalid message cursor")
		}
		cursor = n
	}
	result, err := c.CommunicationMessages(ctx, cursor)
	if err != nil {
		return fail(err)
	}
	if err := json.NewEncoder(out).Encode(map[string]coordinator.CommunicationDiagnostics{"diagnostics": result}); err != nil {
		return 1
	}
	return 0
}
