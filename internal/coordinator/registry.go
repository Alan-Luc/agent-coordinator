package coordinator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
var idPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var tokenPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

type store struct{ root, token string }
type ownerRecord struct {
	Schema int    `json:"schema"`
	Token  string `json:"token"`
}

func validateName(name string) error {
	if !namePattern.MatchString(name) {
		return errors.New("name must match [a-z0-9][a-z0-9-]{0,47}")
	}
	return nil
}
func validateTask(task []byte) error {
	if len(task) > MaxTaskBytes || !utf8.Valid(task) || strings.TrimSpace(string(task)) == "" {
		return errors.New("task must be nonblank UTF-8, at most 1 MiB")
	}
	return nil
}
func validProcessIdentity(identity ProcessIdentity) bool {
	return identity.BootID != "" &&
		identity.PID > 0 &&
		identity.UID == uint32(os.Getuid()) &&
		identity.StartSec > 0 &&
		identity.StartUsec >= 0 && identity.StartUsec < 1_000_000
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
func privateDir(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return checkPrivateDir(path)
}

func checkPrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || stat.Uid != uint32(os.Getuid()) {
		return errors.New("managed directory is not private and current-owner")
	}
	return nil
}
func privateFile(path string, flags int) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0777 != 0600 || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 {
		f.Close()
		return nil, errors.New("managed file is not private and current-owner")
	}
	return f, nil
}
func openStore(root string) (*store, error) {
	if root == "" {
		root = os.Getenv("AC_STATE_DIR")
	}
	if root == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			base = filepath.Join(home, ".local", "state")
		}
		root = filepath.Join(base, "agent-coordinator")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	abs = filepath.Join(parent, filepath.Base(abs))
	if err := privateDir(abs); err != nil {
		return nil, err
	}
	s := &store{root: abs}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	unlock, err := s.lock(ctx, "registry.lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	var owner ownerRecord
	err = readJSON(filepath.Join(abs, "owner.json"), &owner)
	if errors.Is(err, os.ErrNotExist) {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		owner = ownerRecord{SchemaVersion, hex.EncodeToString(b)}
		err = writeJSON(abs, "owner.json", owner)
	}
	if err != nil {
		return nil, err
	}
	if owner.Schema != SchemaVersion || !tokenPattern.MatchString(owner.Token) {
		return nil, errors.New("unknown or invalid installation schema")
	}
	s.token = owner.Token
	if err := privateDir(filepath.Join(abs, "jobs")); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *store) lock(ctx context.Context, rel string) (func(), error) {
	if err := checkPrivateDir(s.root); err != nil {
		return nil, err
	}
	if rel != "registry.lock" {
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 3 || parts[0] != "jobs" || !idPattern.MatchString(parts[1]) || parts[2] != "launch.lock" {
			return nil, errors.New("invalid lock path")
		}
		for _, dir := range []string{filepath.Join(s.root, "jobs"), filepath.Join(s.root, "jobs", parts[1])} {
			if err := checkPrivateDir(dir); err != nil {
				return nil, err
			}
		}
	}
	f, err := privateFile(filepath.Join(s.root, rel), unix.O_RDWR|unix.O_CREAT)
	if err != nil {
		return nil, err
	}
	timer := time.NewTicker(10 * time.Millisecond)
	defer timer.Stop()
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, &ActionError{3, Diagnostic{Code: "lock_timeout", Message: "registry or launch lock unavailable", Uncertain: false}}
		case <-timer.C:
		}
	}
}
func readJSON(path string, out interface{}) error {
	f, err := privateFile(path, unix.O_RDONLY)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return err
	}
	if len(b) > 65536 {
		return errors.New("record too large")
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return errors.New("invalid managed JSON record")
	}
	if err := d.Decode(new(interface{})); err != io.EOF {
		return errors.New("trailing managed JSON")
	}
	return nil
}
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func writeBytes(dir, name string, b []byte) error {
	if filepath.Base(name) != name {
		return errors.New("invalid managed filename")
	}
	path := filepath.Join(dir, name)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("invalid managed destination")
		}
		f, err := privateFile(path, unix.O_RDONLY)
		if err != nil {
			return err
		}
		f.Close()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(dir, ".write-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}
func writeJSON(dir, name string, value interface{}) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeBytes(dir, name, append(b, '\n'))
}
func (s *store) load(id string) (Manifest, Launch, error) {
	var m Manifest
	var l Launch
	if !idPattern.MatchString(id) {
		return m, l, errors.New("invalid job UUID")
	}
	for _, dir := range []string{s.root, filepath.Join(s.root, "jobs")} {
		if err := checkPrivateDir(dir); err != nil {
			return m, l, err
		}
	}
	p := filepath.Join(s.root, "jobs", id)
	info, err := os.Lstat(p)
	if err != nil {
		return m, l, err
	}
	if !info.IsDir() {
		return m, l, errors.New("invalid job directory")
	}
	if err := checkPrivateDir(p); err != nil {
		return m, l, err
	}
	if err := readJSON(filepath.Join(p, "manifest.json"), &m); err != nil {
		return m, l, err
	}
	if err := readJSON(filepath.Join(p, "launch.json"), &l); err != nil {
		return m, l, err
	}
	if m.Schema != SchemaVersion || l.Schema != SchemaVersion || m.ID != id || m.Owner != s.token || m.Protocol != ProtocolVersion || m.BootID == "" || validateName(m.Name) != nil {
		return m, l, errors.New("unknown or invalid job schema/ownership")
	}
	expected, err := s.socket(id, false)
	if err != nil || m.Socket != expected {
		return m, l, errors.New("invalid private socket path")
	}
	switch l.Phase {
	case "preexec", "exec-intent", "failed", "revoked":
	default:
		return m, l, errors.New("unknown launch phase")
	}
	if l.Phase == "exec-intent" && (l.Server == nil || l.Pane == nil) {
		return m, l, errors.New("missing launch process evidence")
	}
	for _, identity := range []*ProcessIdentity{l.Server, l.Pane} {
		if identity != nil && (!validProcessIdentity(*identity) || identity.BootID != m.BootID) {
			return m, l, errors.New("invalid recorded process evidence")
		}
	}
	return m, l, nil
}
func (s *store) socket(id string, create bool) (string, error) {
	if !idPattern.MatchString(id) {
		return "", errors.New("invalid job UUID")
	}
	base, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return "", err
	}
	root := filepath.Join(base, fmt.Sprintf("ac-%d-%s", os.Getuid(), s.token))
	dir := filepath.Join(root, id)
	if create {
		if err := privateDir(root); err != nil {
			return "", err
		}
		if err := privateDir(dir); err != nil {
			return "", err
		}
	}
	socket := filepath.Join(dir, "s")
	if len(socket) > 103 {
		return "", errors.New("tmux socket path too long")
	}
	return socket, nil
}
func (s *store) ids() ([]string, error) {
	for _, dir := range []string{s.root, filepath.Join(s.root, "jobs")} {
		if err := checkPrivateDir(dir); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "jobs"))
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, e := range entries {
		if idPattern.MatchString(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}
