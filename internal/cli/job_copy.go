package cli

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// runnerCopyPrefix starts every runner copy's file name.
const runnerCopyPrefix = "switchboard-"

// RunnerExecutable is the program a job runner runs as. On Windows it is a
// copy of exe in <data dir>/runner, named by version and content, so the
// installed binary can be replaced while jobs run; copies no runner holds
// any more are removed. Elsewhere, or when the copy fails, it is exe.
func RunnerExecutable(exe string) string {
	if !runnerCopies {
		return exe
	}
	p, err := runnerCopy(filepath.Join(dataDir(), "runner"), exe, version)
	if err != nil {
		log.Printf("runner copy: %v; running from %s", err, exe)
		return exe
	}
	return p
}

var unsafeVersion = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// runnerCopy returns exe's copy in dir, making it when missing, and removes
// the other copies there that can be removed (Windows refuses while a runner
// runs from one).
func runnerCopy(dir, exe, ver string) (string, error) {
	f, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	if ver = unsafeVersion.ReplaceAllString(ver, "_"); ver == "" {
		ver = "dev"
	}
	name := fmt.Sprintf("%s%s-%x%s", runnerCopyPrefix, ver, h.Sum(nil)[:6], filepath.Ext(exe))
	dst := filepath.Join(dir, name)
	if _, err = os.Stat(dst); err != nil {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		tmp, err := os.CreateTemp(dir, name+".*.tmp")
		if err != nil {
			return "", err
		}
		if _, err = f.Seek(0, io.SeekStart); err == nil {
			_, err = io.Copy(tmp, f)
		}
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), dst)
		}
		if err != nil {
			_ = os.Remove(tmp.Name())
			// Another job run made the same copy meanwhile.
			if _, serr := os.Stat(dst); serr != nil {
				return "", err
			}
		}
	}
	pruneRunnerCopies(dir, name, time.Now())
	return dst, nil
}

// pruneRunnerCopies removes the copies in dir other than keep. A copy a
// runner still runs from cannot be removed on Windows and stays until its
// last job ends; an unfinished copy is left an hour to its writer.
func pruneRunnerCopies(dir, keep string, now time.Time) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		n := e.Name()
		if n == keep || !strings.HasPrefix(n, runnerCopyPrefix) || e.IsDir() {
			continue
		}
		if strings.HasSuffix(n, ".tmp") {
			if info, err := e.Info(); err != nil || now.Sub(info.ModTime()) < time.Hour {
				continue
			}
		}
		_ = os.Remove(filepath.Join(dir, n))
	}
}
