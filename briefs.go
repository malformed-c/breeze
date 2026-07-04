package main

import (
	"log"
	"os"
	"path/filepath"
	"sync"
)

// briefFileMu serializes brief writes: multiple stages (e.g. concurrent "build" and
// "test" runs for the same commit) can resolve at nearly the same time and would
// otherwise append to the same file concurrently — os.OpenFile with O_APPEND alone
// only guarantees atomicity per individual write(2) syscall, not "check size, then
// decide whether to prepend a header, then write" as one atomic unit.
var briefFileMu sync.Mutex

// writeBriefFile is the daemon's brief-writing wiring: every stage that touches a
// given (pipeline, commit, environment) appends its own section to ONE shared file
// (per the engine's naming — no stage in the filename), writing the one-time
// document header first if the file doesn't exist yet. Best-effort — a failure here
// is logged only, never propagated back to block the stage resolution that
// triggered it (briefs are a convenience artifact, not load-bearing).
func writeBriefFile(dir, filename, header, section string) {
	briefFileMu.Lock()
	defer briefFileMu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("warning: brief: mkdir %s: %v", dir, err)
		return
	}

	path := filepath.Join(dir, filename)
	needsHeader := false
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		needsHeader = true
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("warning: brief: open %s: %v", path, err)
		return
	}
	defer f.Close()

	if needsHeader {
		if _, err := f.WriteString(header); err != nil {
			log.Printf("warning: brief: write header %s: %v", path, err)
			return
		}
	}
	if _, err := f.WriteString(section); err != nil {
		log.Printf("warning: brief: append section %s: %v", path, err)
	}
}
