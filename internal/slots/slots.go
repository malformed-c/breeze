// Package slots implements breeze's machine-wide concurrency budget: a semaphore
// shared by EVERY breeze daemon running as this user, so three daemons on one box
// cannot each launch a fourteen-core build at the same moment.
//
// It lives outside the engine because it is the one piece of breeze state that is
// deliberately not per-daemon. Each daemon owns its own repo's state directory and
// knows nothing about the others; a budget that only bound one daemon would not be
// a budget at all, since the machine is what runs out of cores.
package slots

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Holder describes who is occupying (or waiting for) a slot. Written into the slot
// file on acquire so anyone can see what the machine is busy with — a budget nobody
// can inspect produces "why is my build not starting" with no way to answer it.
type Holder struct {
	PID      int
	Dir      string // the daemon's state directory, i.e. which repo
	Pipeline string
	Stage    string
	Key      string
	Actor    string
	Since    time.Time
}

func (h Holder) String() string {
	return fmt.Sprintf("%s/%s %s  actor=%s  pid=%d  dir=%s  since=%s",
		h.Pipeline, h.Stage, h.Key, h.Actor, h.PID, h.Dir, h.Since.Format(time.RFC3339))
}

func (h Holder) encode() string {
	return strings.Join([]string{
		strconv.Itoa(h.PID), h.Dir, h.Pipeline, h.Stage, h.Key, h.Actor,
		strconv.FormatInt(h.Since.UnixNano(), 10),
	}, "\x1f")
}

func decodeHolder(s string) (Holder, bool) {
	f := strings.Split(strings.TrimSpace(s), "\x1f")
	if len(f) != 7 {
		return Holder{}, false
	}
	pid, _ := strconv.Atoi(f[0])
	ns, _ := strconv.ParseInt(f[6], 10, 64)
	return Holder{PID: pid, Dir: f[1], Pipeline: f[2], Stage: f[3], Key: f[4], Actor: f[5], Since: time.Unix(0, ns)}, true
}

// Dir is where the slot files live: deterministic from uid and home, with NO
// environment input.
//
// That is deliberate and it is the whole correctness argument for the budget.
// Daemons are started from different shells at different times; if this read
// $XDG_RUNTIME_DIR, one daemon with it unset would use a different directory and
// silently get its own private budget — two half-budgets that each look like a
// working one. A split semaphore is worse than no semaphore, because it reports
// success while doing nothing. `breeze status` prints this path for the same
// reason: so a split, if one ever happens, is visible rather than inferred.
func Dir() string {
	if runtime := filepath.Join("/run/user", strconv.Itoa(os.Getuid())); isDir(runtime) {
		return filepath.Join(runtime, "breeze", "slots")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "breeze", "slots")
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// Slot is a held slot. Release returns it. The zero Slot is a no-op release, which
// is what an unlimited (max <= 0) configuration hands back — so callers can always
// `defer s.Release()` without asking whether a budget exists.
type Slot struct {
	fd   int
	path string
}

func (s *Slot) Release() {
	if s == nil || s.fd == 0 {
		return
	}
	// Truncate before closing so a free slot does not keep describing its last
	// occupant. The flock is what actually holds the slot — it is released by the
	// close, and by process death, which is the property that makes this
	// self-healing: a daemon killed mid-run cannot leak a slot the way a counter
	// file or a database row would.
	syscall.Ftruncate(s.fd, 0)
	syscall.Close(s.fd)
	s.fd = 0
}

// Acquire takes one of max slots, waiting up to timeout for one to free. max <= 0
// means no budget is configured and it returns immediately.
//
// waiting, if non-nil, is called once with the current holders when the caller is
// about to block — callers use it to say WHY nothing is happening, since a stage
// that silently sits for twenty minutes is indistinguishable from a stage that hung.
func Acquire(dir string, max int, h Holder, timeout time.Duration, waiting func([]Holder)) (*Slot, error) {
	if max <= 0 {
		return &Slot{}, nil
	}
	if dir == "" {
		return nil, fmt.Errorf("no machine-wide slot directory could be determined (no /run/user/%d and no home directory), so the queue cannot be enforced", os.Getuid())
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating slot directory %s: %w", dir, err)
	}

	if s := tryAcquire(dir, max, h); s != nil {
		return s, nil
	}
	if waiting != nil {
		waiting(Holders(dir, max))
	}

	deadline := time.Now().Add(timeout)
	for {
		if timeout > 0 && time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for one of the machine's %d stage slots; in use by:\n%s",
				timeout, max, describe(Holders(dir, max)))
		}
		time.Sleep(250 * time.Millisecond)
		if s := tryAcquire(dir, max, h); s != nil {
			return s, nil
		}
	}
}

// tryAcquire attempts every slot once, in order, and returns nil if all are taken.
// Ordered rather than random so the occupancy printed by `breeze status` is stable
// between reads, which matters when two people are comparing what they saw.
func tryAcquire(dir string, max int, h Holder) *Slot {
	for i := range max {
		path := filepath.Join(dir, fmt.Sprintf("slot-%d", i))
		fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC, 0o600)
		if err != nil {
			continue
		}
		if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			syscall.Close(fd)
			continue
		}
		syscall.Ftruncate(fd, 0)
		syscall.Pwrite(fd, []byte(h.encode()), 0)
		return &Slot{fd: fd, path: path}
	}
	return nil
}

// Holders reports who currently occupies the slots, by probing each one's flock
// non-destructively: a slot whose lock we CAN take is free, so we take it, read
// nothing, and immediately give it back. Best-effort and inherently a snapshot —
// which is why callers render it as "in use at the time of asking" rather than as a
// standing fact. A point-in-time read is not a claim about any later moment.
func Holders(dir string, max int) []Holder {
	var out []Holder
	for i := range max {
		path := filepath.Join(dir, fmt.Sprintf("slot-%d", i))
		fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0o600)
		if err != nil {
			continue // never created yet, therefore free
		}
		if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			syscall.Flock(fd, syscall.LOCK_UN) // it was free; leave it that way
			syscall.Close(fd)
			continue
		}
		syscall.Close(fd)
		if data, err := os.ReadFile(path); err == nil {
			if h, ok := decodeHolder(string(data)); ok {
				out = append(out, h)
			}
		}
	}
	return out
}

func describe(hs []Holder) string {
	if len(hs) == 0 {
		return "  (nothing — the holders finished while this was being reported)"
	}
	var b strings.Builder
	for _, h := range hs {
		fmt.Fprintf(&b, "  %s\n", h)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Describe renders holders for a human, used by both the wait message and status.
func Describe(hs []Holder) string { return describe(hs) }
