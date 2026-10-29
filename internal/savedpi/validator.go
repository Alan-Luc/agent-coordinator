package savedpi

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"
)

const (
	maxFile    = 64 << 20
	maxLine    = 4 << 20
	maxEntries = 100000
)

var nativeID = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

func ValidSessionID(id string) bool { return nativeID.MatchString(id) }

type Snapshot struct {
	CanonicalFile string `json:"canonical_file"`
	NativeID      string `json:"native_id"`
	Cwd           string `json:"cwd"`
	Device        uint64 `json:"device"`
	Inode         uint64 `json:"inode"`
	Bytes         int64  `json:"bytes"`
	SHA256        string `json:"sha256"`
}

func Validate(path string) (Snapshot, error) {
	var zero Snapshot
	canonical, err := canonicalNoSymlinks(path)
	if err != nil {
		return zero, err
	}
	f, info, err := openSafe(canonical)
	if err != nil {
		return zero, err
	}
	defer f.Close()
	if info.Size() <= 0 || info.Size() > maxFile {
		return zero, errors.New("session file size is outside supported bounds")
	}
	cwd, id, hash, count, err := validateJSONL(f)
	if err != nil {
		return zero, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return zero, errors.New("file identity unavailable")
	}
	if !filepath.IsAbs(cwd) {
		return zero, errors.New("saved cwd must be absolute")
	}
	if _, err := os.Stat(cwd); err != nil {
		return zero, fmt.Errorf("saved cwd is unavailable: %w", err)
	}
	canonicalCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return zero, fmt.Errorf("saved cwd is unavailable: %w", err)
	}
	if filepath.Clean(canonicalCwd) != filepath.Clean(cwd) {
		return zero, errors.New("saved cwd is not canonical")
	}
	cwd = canonicalCwd
	cwdInfo, err := os.Stat(cwd)
	if err != nil || !cwdInfo.IsDir() {
		return zero, errors.New("saved cwd is not an existing directory")
	}
	finalInfo, err := f.Stat()
	if err != nil {
		return zero, err
	}
	if rechecked, err := canonicalNoSymlinks(canonical); err != nil || rechecked != canonical {
		return zero, errors.New("session path changed while validating")
	}
	pathInfo, err := os.Lstat(canonical)
	if err != nil {
		return zero, errors.New("session path changed while validating")
	}
	pathStat, pathOK := pathInfo.Sys().(*syscall.Stat_t)
	finalStat, ok := finalInfo.Sys().(*syscall.Stat_t)
	if !pathOK || pathInfo.Mode()&os.ModeSymlink != 0 || pathStat.Dev != st.Dev || pathStat.Ino != st.Ino || pathStat.Nlink != 1 || pathStat.Uid != st.Uid || !ok || !finalInfo.Mode().IsRegular() || finalStat.Dev != st.Dev || finalStat.Ino != st.Ino || finalStat.Nlink != 1 || finalStat.Uid != st.Uid || finalInfo.Mode().Perm()&0022 != 0 || count != finalInfo.Size() || count != info.Size() {
		return zero, errors.New("session changed while validating")
	}
	return Snapshot{CanonicalFile: canonical, NativeID: id, Cwd: cwd, Device: uint64(st.Dev), Inode: st.Ino, Bytes: count, SHA256: hash}, nil
}

func Recheck(s Snapshot) error {
	current, err := Validate(s.CanonicalFile)
	if err != nil {
		return err
	}
	if current != s {
		return errors.New("saved session changed since validation")
	}
	return nil
}

func canonicalNoSymlinks(path string) (string, error) {
	if path == "" {
		return "", errors.New("session path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	vol := filepath.VolumeName(abs)
	rest := strings.TrimPrefix(abs, vol+string(filepath.Separator))
	cur := vol + string(filepath.Separator)
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("session path traverses a symlink")
		}
	}
	return abs, nil
}

func openSafe(path string) (*os.File, os.FileInfo, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Nlink != 1 || st.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0022 != 0 {
		f.Close()
		return nil, nil, errors.New("session must be a current-owner regular single-link file without group/world write access")
	}
	return f, info, nil
}

func validateJSONL(f *os.File) (string, string, string, int64, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", "", "", 0, err
	}
	h := sha256.New()
	counter := &countingReader{r: io.TeeReader(io.LimitReader(f, maxFile+1), h)}
	scanner := bufio.NewScanner(counter)
	scanner.Buffer(make([]byte, 64*1024), maxLine+1)
	scanner.Split(splitBoundedLine)
	ids := make(map[string]bool)
	var header struct {
		Type    string `json:"type"`
		Version int    `json:"version"`
		ID      string `json:"id"`
		Cwd     string `json:"cwd"`
	}
	var native string
	lineNo := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		lineNo++
		if !utf8.Valid(line) {
			return "", "", "", counter.n, errors.New("session contains invalid utf-8")
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(line, &obj) != nil || obj == nil {
			return "", "", "", counter.n, fmt.Errorf("invalid JSON on line %d", lineNo)
		}
		var typ string
		if json.Unmarshal(obj["type"], &typ) != nil {
			return "", "", "", counter.n, errors.New("entry type is missing")
		}
		if lineNo == 1 {
			if json.Unmarshal(line, &header) != nil || header.Type != "session" || header.Version != 3 || !nativeID.MatchString(header.ID) || header.Cwd == "" || !stringValue(obj, "timestamp") {
				return "", "", "", counter.n, errors.New("unsupported session header")
			}
			native = header.ID
		} else {
			if typ == "session" {
				return "", "", "", counter.n, errors.New("duplicate session header")
			}
			if len(ids) >= maxEntries {
				return "", "", "", counter.n, errors.New("session has too many entries")
			}
			var base struct {
				ID     string          `json:"id"`
				Parent json.RawMessage `json:"parentId"`
			}
			if json.Unmarshal(line, &base) != nil || base.ID == "" || len(base.Parent) == 0 || ids[base.ID] {
				return "", "", "", counter.n, errors.New("invalid or duplicate entry identity")
			}
			if string(base.Parent) != "null" {
				var parent string
				if json.Unmarshal(base.Parent, &parent) != nil || !ids[parent] {
					return "", "", "", counter.n, errors.New("entry parent must precede child")
				}
			}
			if !supportedEntry(typ, obj) || !validReferences(typ, obj, ids, base.ID) {
				return "", "", "", counter.n, fmt.Errorf("unsupported or invalid %s entry", typ)
			}
			ids[base.ID] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", "", counter.n, fmt.Errorf("invalid bounded JSONL: %w", err)
	}
	if counter.n > maxFile {
		return "", "", "", counter.n, errors.New("session exceeds 64 MiB limit")
	}
	if lineNo == 0 {
		return "", "", "", 0, errors.New("session is empty")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", "", "", counter.n, err
	}
	return header.Cwd, native, hex.EncodeToString(h.Sum(nil)), counter.n, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func splitBoundedLine(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		if i+1 > maxLine {
			return 0, nil, errors.New("session line exceeds 4 MiB limit")
		}
		return i + 1, data[:i], nil
	}
	if len(data) >= maxLine {
		return 0, nil, errors.New("session line exceeds 4 MiB limit")
	}
	if atEOF && len(data) > 0 {
		return 0, nil, errors.New("session must end with a newline")
	}
	return 0, nil, nil
}

func supportedEntry(t string, obj map[string]json.RawMessage) bool {
	if !stringValue(obj, "timestamp") {
		return false
	}
	switch t {
	case "message":
		var m map[string]json.RawMessage
		return decodeObject(obj["message"], &m) && validMessage(m)
	case "model_change":
		return stringValue(obj, "provider") && stringValue(obj, "modelId")
	case "thinking_level_change":
		return stringValue(obj, "thinkingLevel")
	case "usage":
		return stringValue(obj, "kind") && validUsage(obj["usage"])
	case "compaction":
		return stringValue(obj, "summary") && numberValue(obj, "tokensBefore") && stringValue(obj, "firstKeptEntryId")
	case "context_edit":
		return stringValue(obj, "targetId") && (stringValue(obj, "replacement") || objectValue(obj, "replacement"))
	case "branch_summary":
		return stringValue(obj, "summary") && (stringValue(obj, "fromId") || nullValue(obj, "fromId"))
	case "custom":
		return stringValue(obj, "customType")
	case "custom_message":
		return stringValue(obj, "customType") && validUserContent(obj["content"]) && boolValue(obj, "display")
	case "label":
		return stringValue(obj, "targetId") && stringValue(obj, "label")
	case "session_info":
		return stringValue(obj, "name")
	default:
		return false
	}
}

func validReferences(t string, obj map[string]json.RawMessage, prior map[string]bool, current string) bool {
	for _, key := range []string{"targetId", "firstKeptEntryId"} {
		if raw, ok := obj[key]; ok {
			var id string
			if json.Unmarshal(raw, &id) != nil || (id != current && !prior[id]) {
				return false
			}
		}
	}
	if raw, ok := obj["fromId"]; ok && string(raw) != "null" {
		var id string
		if json.Unmarshal(raw, &id) != nil || !prior[id] {
			return false
		}
	}
	return true
}

func validUsage(raw json.RawMessage) bool {
	var usage map[string]json.RawMessage
	if !decodeObject(raw, &usage) {
		return false
	}
	for _, key := range []string{"input", "output", "cacheRead", "cacheWrite", "totalTokens"} {
		if !numberValue(usage, key) {
			return false
		}
	}
	var cost map[string]json.RawMessage
	if !decodeObject(usage["cost"], &cost) {
		return false
	}
	for _, key := range []string{"input", "output", "cacheRead", "cacheWrite", "total"} {
		if !numberValue(cost, key) {
			return false
		}
	}
	return true
}

func validMessage(m map[string]json.RawMessage) bool {
	if !numberValue(m, "timestamp") {
		return false
	}
	switch stringField(m, "role") {
	case "system":
		return validSystemContent(m["content"])
	case "user":
		return validUserContent(m["content"])
	case "assistant":
		return stringValue(m, "api") && stringValue(m, "provider") && stringValue(m, "model") && arrayOf(m["content"], validAssistantBlock) && validUsage(m["usage"]) && stringValue(m, "stopReason")
	case "toolResult":
		return stringValue(m, "toolCallId") && stringValue(m, "toolName") && validUserContent(m["content"]) && boolValue(m, "isError")
	case "bashExecution":
		return stringValue(m, "command") && stringValue(m, "output") && boolValue(m, "cancelled") && boolValue(m, "truncated")
	case "custom":
		return stringValue(m, "customType") && validUserContent(m["content"]) && boolValue(m, "display")
	case "branchSummary":
		return stringValue(m, "summary") && (stringValue(m, "fromId") || nullValue(m, "fromId"))
	case "compactionSummary":
		return stringValue(m, "summary") && numberValue(m, "tokensBefore")
	default:
		return false
	}
}
func validSystemContent(raw json.RawMessage) bool {
	if isString(raw) {
		return true
	}
	return arrayOf(raw, func(b map[string]json.RawMessage) bool {
		return stringField(b, "type") == "text" && stringValue(b, "text")
	})
}
func validUserContent(raw json.RawMessage) bool {
	if isString(raw) {
		return true
	}
	return arrayOf(raw, validUserBlock)
}
func validUserBlock(b map[string]json.RawMessage) bool {
	switch stringField(b, "type") {
	case "text":
		return stringValue(b, "text")
	case "image":
		return stringValue(b, "data") && stringValue(b, "mimeType")
	default:
		return false
	}
}
func validAssistantBlock(b map[string]json.RawMessage) bool {
	switch stringField(b, "type") {
	case "text":
		return stringValue(b, "text")
	case "thinking":
		return stringValue(b, "thinking") || boolValue(b, "redacted")
	case "toolCall":
		return stringValue(b, "id") && stringValue(b, "name") && objectValue(b, "arguments")
	default:
		return false
	}
}
func arrayOf(raw json.RawMessage, valid func(map[string]json.RawMessage) bool) bool {
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return false
	}
	for _, v := range values {
		var m map[string]json.RawMessage
		if !decodeObject(v, &m) || !valid(m) {
			return false
		}
	}
	return true
}
func decodeObject(raw json.RawMessage, out *map[string]json.RawMessage) bool {
	return json.Unmarshal(raw, out) == nil && *out != nil
}
func objectValue(m map[string]json.RawMessage, k string) bool {
	var v map[string]json.RawMessage
	return decodeObject(m[k], &v)
}
func stringField(m map[string]json.RawMessage, k string) string {
	var v string
	_ = json.Unmarshal(m[k], &v)
	return v
}
func stringValue(m map[string]json.RawMessage, k string) bool {
	v, ok := m[k]
	return ok && json.Unmarshal(v, new(string)) == nil && string(v) != "null" && isString(v)
}
func isString(raw json.RawMessage) bool { var v string; return json.Unmarshal(raw, &v) == nil }
func boolValue(m map[string]json.RawMessage, k string) bool {
	var v bool
	return json.Unmarshal(m[k], &v) == nil && string(m[k]) != "null"
}
func numberValue(m map[string]json.RawMessage, k string) bool {
	var v json.Number
	return json.Unmarshal(m[k], &v) == nil && v.String() != "" && v.String() != "null"
}
func nullValue(m map[string]json.RawMessage, k string) bool { return string(m[k]) == "null" }
