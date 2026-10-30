package coordinator

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"coordinate/internal/pilaunch"
)

const InteractiveProtocolVersion = "2"
const TransparentTerminalProfile = "transparent-v1"
const MaxLaunchArguments = 256
const MaxLaunchArgumentBytes = 1 << 20

type InteractiveIntent struct {
	Schema          int               `json:"schema"`
	Kind            string            `json:"kind"`
	TerminalProfile string            `json:"terminal_profile"`
	ArgvSHA256      string            `json:"argv_sha256"`
	NativePi        *pilaunch.Managed `json:"native_pi"`
}

type JobRole struct {
	Schema int    `json:"schema"`
	Role   string `json:"role"`
}

func supportedJobProtocol(protocol string) bool {
	return protocol == ProtocolVersion || protocol == InteractiveProtocolVersion
}

func readJobRole(directory string) (JobRole, error) {
	var role JobRole
	if err := readJSON(filepath.Join(directory, "role.json"), &role); err != nil {
		return role, err
	}
	if role.Schema != SchemaVersion || role.Role != "standalone" && role.Role != "manager-host" {
		return role, errors.New("unsupported job role")
	}
	return role, nil
}

func validateLaunchKind(directory, protocol string) error {
	if protocol == ProtocolVersion {
		if _, err := os.Lstat(filepath.Join(directory, "intent.json")); !errors.Is(err, os.ErrNotExist) {
			return errors.New("legacy job cannot have an interactive intent")
		}
		return nil
	}
	if protocol != InteractiveProtocolVersion {
		return errors.New("unsupported job protocol")
	}
	if _, err := readInteractiveIntent(directory); err != nil {
		return err
	}
	if _, err := readJobRole(directory); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(directory, "initial-task.txt")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("interactive job cannot have a task input")
	}
	return nil
}

type LaunchArguments struct {
	Schema int      `json:"schema"`
	Argv   []string `json:"argv"`
}

func readInteractiveIntent(directory string) (InteractiveIntent, error) {
	var intent InteractiveIntent
	if err := readJSON(filepath.Join(directory, "intent.json"), &intent); err != nil {
		return intent, err
	}
	hash, err := hex.DecodeString(intent.ArgvSHA256)
	if intent.Schema != SchemaVersion || intent.Kind != "interactive" || intent.TerminalProfile != TransparentTerminalProfile || err != nil || len(hash) != sha256.Size || strings.ToLower(intent.ArgvSHA256) != intent.ArgvSHA256 || intent.NativePi == nil || !filepath.IsAbs(intent.NativePi.Executable) || intent.NativePi.Observation.Version != "1.4.0" {
		return intent, errors.New("unsupported interactive launch intent")
	}
	return intent, nil
}

func readInteractiveArguments(directory string) ([]string, error) {
	intent, err := readInteractiveIntent(directory)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(directory, "initial-task.txt")); !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("launch must have exactly one input kind")
	}
	body, err := readPrivateBytes(filepath.Join(directory, "launch-argv.json"), MaxLaunchArgumentBytes)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(body)
	if hex.EncodeToString(hash[:]) != intent.ArgvSHA256 || !validArgumentJSONUnicode(body) {
		return nil, errors.New("invalid launch argument hash or Unicode")
	}
	var payload struct {
		Schema int               `json:"schema"`
		Argv   []json.RawMessage `json:"argv"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, errors.New("invalid launch argument payload")
	}
	if decoder.Decode(new(any)) != io.EOF || payload.Schema != SchemaVersion || payload.Argv == nil || len(payload.Argv) > MaxLaunchArguments {
		return nil, errors.New("unsupported launch argument payload")
	}
	arguments := make([]string, len(payload.Argv))
	for i, raw := range payload.Argv {
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &arguments[i]) != nil || strings.ContainsRune(arguments[i], 0) {
			return nil, errors.New("invalid launch argument")
		}
	}
	return arguments, nil
}

// encoding/json replaces malformed Unicode; argv must instead remain lossless.
func validArgumentJSONUnicode(body []byte) bool {
	if !utf8.Valid(body) {
		return false
	}
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		i++
		if i >= len(body) {
			return false
		}
		if body[i] != 'u' {
			continue
		}
		if i+4 >= len(body) {
			return false
		}
		code, err := strconv.ParseUint(string(body[i+1:i+5]), 16, 16)
		if err != nil || code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		i += 4
		if code < 0xd800 || code > 0xdbff {
			continue
		}
		if i+6 >= len(body) || body[i+1] != '\\' || body[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(body[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}
