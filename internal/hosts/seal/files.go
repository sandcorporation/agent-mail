package seal

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// renameFile is swapped by tests to simulate a failed final step.
var renameFile = os.Rename

// writeAtomic writes data to a temp file in path's directory with mode
// 0600, syncs it, then renames it over path. On failure path is untouched
// and the temp file is removed.
func writeAtomic(path string, data []byte) (err error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	f, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return fmt.Errorf("seal: write %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		return fmt.Errorf("seal: write %s: %w", path, err)
	}
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("seal: write %s: %w", path, err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("seal: write %s: %w", path, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("seal: write %s: %w", path, err)
	}
	if err = renameFile(tmp, path); err != nil {
		return fmt.Errorf("seal: write %s: %w", path, err)
	}
	return nil
}

// --- key files ---

// SaveKey writes key to path as one base64 line, mode 0600, atomically.
func SaveKey(path string, key [KeySize]byte) error {
	return writeAtomic(path, []byte(base64.StdEncoding.EncodeToString(key[:])+"\n"))
}

// LoadKey reads a key written by SaveKey. On Unix it refuses a file that
// group or other can read.
func LoadKey(path string) ([KeySize]byte, error) {
	var key [KeySize]byte
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		if err != nil {
			return key, fmt.Errorf("seal: load key: %w", err)
		}
		if perm := st.Mode().Perm(); perm&0o077 != 0 {
			return key, fmt.Errorf("seal: key file %s has mode %04o; it must not be readable by group or other (chmod 600)", path, perm)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return key, fmt.Errorf("seal: load key: %w", err)
	}
	b, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(raw)))
	if err != nil || len(b) != KeySize {
		// Say nothing about the content: it may be key material.
		return key, fmt.Errorf("seal: key file %s does not hold a %d-byte base64 key", path, KeySize)
	}
	copy(key[:], b)
	return key, nil
}

// --- role env file ---

var envNameRE = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// ValidEnvName reports whether name can be set with SetEnv.
func ValidEnvName(name string) bool { return envNameRE.MatchString(name) }

// SetEnv sets name=value in the env file at path, creating it if needed.
// Other lines (comments included) are kept as they are; an existing line
// for name is replaced in place and later duplicates are dropped. The file
// is rewritten atomically with mode 0600.
func SetEnv(path, name, value string) error {
	if !ValidEnvName(name) {
		return fmt.Errorf("seal: env name %q must match [A-Z_][A-Z0-9_]*", name)
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("seal: value for %s contains a newline or NUL", name)
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("seal: read %s: %w", path, err)
	}
	var lines []string
	if len(raw) > 0 {
		lines = strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	}
	entry := name + "=" + value
	out := make([]string, 0, len(lines)+1)
	done := false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSuffix(line, "\r"), name+"=") {
			if !done {
				out = append(out, entry)
				done = true
			}
			continue
		}
		out = append(out, line)
	}
	if !done {
		out = append(out, entry)
	}
	return writeAtomic(path, []byte(strings.Join(out, "\n")+"\n"))
}

// LoadEnv reads an env file: KEY=value lines, blank lines and # comments.
// The value is everything after the first '='. A later line for the same
// name wins.
func LoadEnv(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("seal: read %s: %w", path, err)
	}
	env := map[string]string{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || !ValidEnvName(name) {
			// Line content may be a secret; report the line number only.
			return nil, fmt.Errorf("seal: %s line %d is not NAME=value", path, i+1)
		}
		env[name] = value
	}
	return env, nil
}
