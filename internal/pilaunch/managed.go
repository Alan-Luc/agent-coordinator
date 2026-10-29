package pilaunch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const launcherSHA256 = "da0c6a49fe94aadc9765cc7afc6ba6b17e553eed0247d0385c160bc188f5cfbb"

type FileObservation struct {
	Path string
	Hash string
	Size int64
	Mode uint32
	UID  uint32
	Dev  uint64
	Ino  uint64
}

type LinkObservation struct {
	Path   string
	Target string
	UID    uint32
	Dev    uint64
	Ino    uint64
}

type Observation struct {
	Launcher FileObservation
	Current  FileObservation
	Package  FileObservation
	BinLink  LinkObservation
	Entry    FileObservation
	Version  string
}

type Managed struct {
	Executable  string
	Observation Observation
}

func Resolve(argv0, path, coordinator string) (string, error) {
	resolved, err := lookPath(argv0, path)
	if err != nil {
		return "", errors.New("native Pi executable is unavailable")
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", errors.New("native Pi executable path is invalid")
	}
	real, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", errors.New("native Pi executable cannot be resolved")
	}
	if coordinator != "" {
		self, selfErr := filepath.EvalSymlinks(coordinator)
		if selfErr == nil {
			if a, e := os.Stat(real); e == nil {
				if b, e2 := os.Stat(self); e2 == nil && os.SameFile(a, b) {
					return "", errors.New("native Pi resolves to coordinator")
				}
			}
		}
		if real == coordinator {
			return "", errors.New("native Pi resolves to coordinator")
		}
	}
	return resolved, nil
}

func lookPath(name, path string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		return name, nil
	}
	for _, dir := range filepath.SplitList(path) {
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("native Pi executable is unavailable")
}

func Observe(executable string) (Observation, error) {
	var result Observation
	launcher, err := observeFile(executable, 1<<20)
	if err != nil || launcher.Hash != launcherSHA256 {
		return result, errors.New("unrecognized managed Pi launcher")
	}
	launcherPath, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return result, errors.New("managed Pi launcher cannot be resolved")
	}
	agentDir := filepath.Dir(filepath.Dir(launcherPath))
	installDir := filepath.Join(agentDir, "install")
	current, currentBody, err := readObservedFile(filepath.Join(installDir, "current-version"), 128)
	if err != nil {
		return result, errors.New("managed Pi version metadata is unsafe")
	}
	version, err := parseToken(currentBody)
	if err != nil || version != "1.4.0" {
		return result, errors.New("unsupported managed Pi version")
	}
	pkgPath := filepath.Join(installDir, "releases", version, "node_modules", "@earendil-works", "pi-coding-agent", "package.json")
	pkg, body, err := readObservedFile(pkgPath, 256<<10)
	if err != nil {
		return result, errors.New("managed Pi package metadata is unsafe")
	}
	var meta struct {
		Name    string            `json:"name"`
		Version string            `json:"version"`
		Bin     map[string]string `json:"bin"`
	}
	if json.Unmarshal(body, &meta) != nil || meta.Name != "@earendil-works/pi-coding-agent" || meta.Version != version || meta.Bin["pi"] != "dist/bundle/cli.js" {
		return result, errors.New("managed Pi package identity mismatch")
	}
	releaseDir := filepath.Join(installDir, "releases", version)
	bin := filepath.Join(releaseDir, "node_modules", ".bin", "pi")
	link, err := observeLink(bin)
	if err != nil {
		return result, errors.New("managed Pi bin link unavailable")
	}
	entryPath, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return result, errors.New("managed Pi bin entry unavailable")
	}
	want := filepath.Join(filepath.Dir(pkgPath), meta.Bin["pi"])
	if !sameResolvedEntry(bin, want) {
		return result, errors.New("managed Pi bin link mismatch")
	}
	entry, err := observeFile(entryPath, 16<<20)
	if err != nil {
		return result, errors.New("managed Pi entry is unsafe")
	}
	result = Observation{Launcher: launcher, Current: current, Package: pkg, BinLink: link, Entry: entry, Version: version}
	return result, nil
}

func observeLink(path string) (LinkObservation, error) {
	var result LinkObservation
	abs, err := filepath.Abs(path)
	if err != nil {
		return result, err
	}
	info, err := os.Lstat(abs)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return result, errors.New("managed bin is not a symlink")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return result, errors.New("unsafe managed bin link owner")
	}
	target, err := os.Readlink(abs)
	if err != nil {
		return result, err
	}
	after, err := os.Lstat(abs)
	if err != nil {
		return result, err
	}
	afterStat, ok := after.Sys().(*syscall.Stat_t)
	if !ok || afterStat.Dev != stat.Dev || afterStat.Ino != stat.Ino {
		return result, errors.New("managed bin link changed while observing")
	}
	return LinkObservation{Path: abs, Target: target, UID: stat.Uid, Dev: uint64(stat.Dev), Ino: uint64(stat.Ino)}, nil
}

func sameResolvedEntry(bin, declared string) bool {
	actual, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return false
	}
	want, err := filepath.EvalSymlinks(declared)
	return err == nil && actual == want
}

func ObserveManaged(executable string) (Managed, error) {
	observation, err := Observe(executable)
	if err != nil {
		return Managed{}, err
	}
	return Managed{Executable: executable, Observation: observation}, nil
}

func Recheck(managed Managed) error {
	current, err := Observe(managed.Executable)
	if err != nil {
		return err
	}
	if current != managed.Observation {
		return errors.New("managed Pi changed since observation")
	}
	return nil
}

func observeFile(path string, limit int64) (FileObservation, error) {
	var result FileObservation
	result, _, err := readObservedFile(path, limit)
	return result, err
}

func readObservedFile(path string, limit int64) (FileObservation, []byte, error) {
	var result FileObservation
	abs, err := filepath.Abs(path)
	if err != nil {
		return result, nil, err
	}
	before, err := os.Lstat(abs)
	if err != nil || !before.Mode().IsRegular() || before.Size() > limit {
		return result, nil, errors.New("unsafe metadata file")
	}
	fd, err := syscall.Open(abs, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return result, nil, err
	}
	f := os.NewFile(uintptr(fd), abs)
	defer f.Close()
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Uid != uint32(os.Getuid()) || stat.Mode&0022 != 0 || stat.Size > limit {
		return result, nil, errors.New("unsafe metadata ownership, type, or permissions")
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return result, nil, errors.New("metadata changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return result, nil, errors.New("metadata exceeds limit")
	}
	sum := sha256.Sum256(b)
	return FileObservation{Path: abs, Hash: hex.EncodeToString(sum[:]), Size: int64(len(b)), Mode: uint32(stat.Mode), UID: stat.Uid, Dev: uint64(stat.Dev), Ino: uint64(stat.Ino)}, b, nil
}

func parseToken(b []byte) (string, error) {
	s := strings.TrimSuffix(string(b), "\n")
	if strings.ContainsAny(s, "\r\n") || s == "" {
		return "", fmt.Errorf("invalid version token")
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._+-", r)) {
			return "", fmt.Errorf("invalid version token")
		}
	}
	return s, nil
}
