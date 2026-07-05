package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// registry.go implements a small, best-effort, machine-wide index of every breeze
// daemon directory that has ever started on this machine — NOT coordination state
// (that stays strictly per-repo, per resolvePaths' rules), just a discovery aid for
// `breeze operator update-all` so it doesn't need a maintained list of repos or a
// filesystem scan. Every daemon upserts its own entry on successful bind and removes
// it on a graceful (non-restart) stop; update-all treats the registry as a set of
// leads to dial-probe, not a source of truth — a stale or missing entry is just a
// daemon it won't find, never a correctness problem for that daemon itself.

type registryEntry struct {
	Dir      string    `json:"dir"`
	Pid      int       `json:"pid"`
	Sock     string    `json:"sock"`
	LastSeen time.Time `json:"lastSeen"`
}

// registryPath returns ~/.cache/breeze/registry.json (respecting $XDG_CACHE_HOME),
// distinct in both location and purpose from the old ~/.breeze fallback directory
// removed earlier — this holds no coordination state, only a discovery index.
func registryPath() (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "breeze", "registry.json"), nil
}

// withRegistryLock runs fn while holding an flock on registry.json.lock — brief
// critical section around the whole read-modify-write so two daemons starting at
// nearly the same moment can't silently lose one update to the other.
func withRegistryLock(fn func(path string) error) error {
	path, err := registryPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	fd, err := syscall.Open(path+".lock", syscall.O_CREAT|syscall.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	return fn(path)
}

func loadRegistryFile(path string) ([]registryEntry, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []registryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, nil // a corrupt registry is a discovery gap, not worth failing over
	}
	return entries, nil
}

func saveRegistryFile(path string, entries []registryEntry) error {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// registerSelf upserts an entry for p.dir — best-effort: a failure here (unwritable
// cache dir, etc.) is logged but must never block the daemon from actually serving.
func registerSelf(p paths) error {
	return withRegistryLock(func(path string) error {
		entries, err := loadRegistryFile(path)
		if err != nil {
			return err
		}
		entry := registryEntry{Dir: p.dir, Pid: os.Getpid(), Sock: p.sock, LastSeen: time.Now()}
		replaced := false
		for i, e := range entries {
			if e.Dir == p.dir {
				entries[i] = entry
				replaced = true
				break
			}
		}
		if !replaced {
			entries = append(entries, entry)
		}
		return saveRegistryFile(path, entries)
	})
}

// deregisterSelf removes p.dir's entry on a graceful stop (not a restart, which
// keeps the same dir/pid and simply re-execs) — keeps the registry from
// accumulating entries for repos that have been cleanly shut down, though
// update-all's own dial-probe would skip a stale entry regardless.
func deregisterSelf(p paths) error {
	return withRegistryLock(func(path string) error {
		entries, err := loadRegistryFile(path)
		if err != nil {
			return err
		}
		kept := entries[:0]
		for _, e := range entries {
			if e.Dir != p.dir {
				kept = append(kept, e)
			}
		}
		return saveRegistryFile(path, kept)
	})
}
