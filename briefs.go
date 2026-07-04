package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// writeBriefFile is the daemon's brief-writing wiring: given the directory/filename/
// content the engine already computed, ensure the directory exists and write the
// file, appending -2, -3, ... if a same-day retry would otherwise collide with an
// existing brief (rather than silently overwriting prior history). Best-effort — a
// failure here is logged only, never propagated back to block the stage resolution
// that triggered it (briefs are a convenience artifact, not load-bearing).
func writeBriefFile(dir, filename, content string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("warning: brief: mkdir %s: %v", dir, err)
		return
	}

	path := filepath.Join(dir, filename)
	if _, err := os.Stat(path); err == nil {
		ext := filepath.Ext(filename)
		base := strings.TrimSuffix(filename, ext)
		for i := 2; ; i++ {
			candidate := filepath.Join(dir, fmt.Sprintf("%s-%d%s", base, i, ext))
			if _, err := os.Stat(candidate); os.IsNotExist(err) {
				path = candidate
				break
			}
		}
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		log.Printf("warning: brief: write %s: %v", path, err)
	}
}
