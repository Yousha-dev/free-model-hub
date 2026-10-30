// Package logger provides in-plugin logging: memory ring + stderr + optional daily files.
//
// Three deliberate designs:
//   - sink uses stderr instead of stdout: the dynamic library is loaded by the CPA host, and stdout belongs to the host process,
//     writing to stdout would pollute the host's normal output;
//   - memory ring allows the management page to incrementally pull recent logs since a given time (flipping through host logs for plugin logs is inconvenient);
//   - daily files are optional, disabled by default, keeping only the last N days.
//
// Security convention: callers must not write complete credentials into logs, only prefixes are allowed.
package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Level is the log level. The higher the value, the more severe.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelError
)

// ringCapacity is the memory ring capacity. The management page pulls 200 items at a time by default, 1000 items is enough for backtracking.
const ringCapacity = 1000

// fileRetentionDays is the retention days for daily log files.
const fileRetentionDays = 7

// Entry is a single log entry.
type Entry struct {
	Seq     uint64 `json:"seq"`
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

type ring struct {
	mu      sync.Mutex
	entries []Entry
	head    int
	full    bool
	nextSeq uint64
}

var (
	globalRing = &ring{entries: make([]Entry, 0, ringCapacity)}

	levelMu    sync.RWMutex
	currentLvl = LevelInfo

	fileMu   sync.Mutex
	fileSink *os.File
	fileDay  string
	fileDir  string
)

// SetLevel sets the log level. Accepts debug / info / error, unknown values fall back to info.
func SetLevel(raw string) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		setLevel(LevelDebug)
	case "error":
		setLevel(LevelError)
	default:
		setLevel(LevelInfo)
	}
}

func setLevel(lvl Level) {
	levelMu.Lock()
	currentLvl = lvl
	levelMu.Unlock()
}

// CurrentLevel returns the current level name (for display on the management page).
func CurrentLevel() string {
	levelMu.RLock()
	defer levelMu.RUnlock()
	switch currentLvl {
	case LevelDebug:
		return "debug"
	case LevelError:
		return "error"
	default:
		return "info"
	}
}

func enabled(lvl Level) bool {
	levelMu.RLock()
	defer levelMu.RUnlock()
	return lvl >= currentLvl
}

// InitFile enables or disables the file sink.
//
// When enabled is false, closes any opened file; when true, writes daily logs under dir.
// Failure is non-fatal: failing to write logs should not cause the plugin to refuse to start.
func InitFile(enabled bool, dir string) error {
	fileMu.Lock()
	defer fileMu.Unlock()

	if fileSink != nil {
		if errClose := fileSink.Close(); errClose != nil {
			fmt.Fprintf(os.Stderr, "[free-model-hub] close log file failed: %v\n", errClose)
		}
		fileSink = nil
		fileDay = ""
	}
	if !enabled {
		return nil
	}
	if errMkdir := os.MkdirAll(dir, 0o755); errMkdir != nil {
		return fmt.Errorf("create log dir %s: %w", dir, errMkdir)
	}
	fileDir = dir
	pruneLogFiles(dir)
	return openLogFileLocked(time.Now())
}

// openLogFileLocked opens today's log file. The caller must hold fileMu.
func openLogFileLocked(now time.Time) error {
	day := now.Format("2006-01-02")
	path := filepath.Join(fileDir, "free-model-hub-"+day+".log")
	file, errOpen := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if errOpen != nil {
		return fmt.Errorf("open log file %s: %w", path, errOpen)
	}
	fileSink = file
	fileDay = day
	return nil
}

// pruneLogFiles deletes log files older than the retention period.
func pruneLogFiles(dir string) {
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -fileRetentionDays)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "free-model-hub-") {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil || info.ModTime().After(cutoff) {
			continue
		}
		names = append(names, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(names)
	for _, name := range names {
		_ = os.Remove(name)
	}
}

// Close closes the file sink. Called by shutdown.
func Close() {
	fileMu.Lock()
	defer fileMu.Unlock()
	if fileSink != nil {
		_ = fileSink.Close()
		fileSink = nil
		fileDay = ""
	}
}

// Debug logs a debug message.
func Debug(format string, args ...any) { log(LevelDebug, "DEBUG", format, args...) }

// Info logs an info message.
func Info(format string, args ...any) { log(LevelInfo, "INFO", format, args...) }

// Error logs an error message.
func Error(format string, args ...any) { log(LevelError, "ERROR", format, args...) }

func log(lvl Level, label string, format string, args ...any) {
	if !enabled(lvl) {
		return
	}
	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}
	now := time.Now()
	line := fmt.Sprintf("%s [%s] %s\n", now.Format("2006-01-02 15:04:05.000"), label, message)

	fmt.Fprint(os.Stderr, line)

	globalRing.append(Entry{
		Time:    now.Format("2006-01-02 15:04:05.000"),
		Level:   strings.ToLower(label),
		Message: message,
	})

	fileMu.Lock()
	if fileSink != nil {
		// When crossing days, switch the file before writing.
		if day := now.Format("2006-01-02"); day != fileDay {
			if errRotate := openLogFileLocked(now); errRotate != nil {
				fmt.Fprintf(os.Stderr, "[free-model-hub] rotate log file failed: %v\n", errRotate)
			}
		}
		if fileSink != nil {
			_, _ = fileSink.WriteString(line)
		}
	}
	fileMu.Unlock()
}

func (r *ring) append(entry Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextSeq++
	entry.Seq = r.nextSeq
	if len(r.entries) < ringCapacity {
		r.entries = append(r.entries, entry)
		return
	}
	r.entries[r.head] = entry
	r.head = (r.head + 1) % ringCapacity
	r.full = true
}

func (r *ring) orderedEntries() []Entry {
	if !r.full {
		out := make([]Entry, len(r.entries))
		copy(out, r.entries)
		return out
	}
	out := make([]Entry, ringCapacity)
	n := copy(out, r.entries[r.head:])
	copy(out[n:], r.entries[:r.head])
	return out
}

// Snapshot returns logs with a seq greater than since (up to limit entries) and the current maximum seq.
func Snapshot(since uint64, limit int) ([]Entry, uint64) {
	if limit <= 0 || limit > ringCapacity {
		limit = 200
	}
	globalRing.mu.Lock()
	defer globalRing.mu.Unlock()

	newest := globalRing.nextSeq
	all := globalRing.orderedEntries()
	out := make([]Entry, 0, limit)
	if since == 0 {
		start := len(all) - limit
		if start < 0 {
			start = 0
		}
		for _, entry := range all[start:] {
			out = append(out, entry)
		}
		return out, newest
	}
	for _, entry := range all {
		if entry.Seq <= since {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, entry)
	}
	return out, newest
}
