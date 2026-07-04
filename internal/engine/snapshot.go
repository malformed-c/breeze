package engine

import (
	"encoding/json"
	"os"
)

// Snapshot is the serializable form of current daemon state. Stage instances are
// intentionally NOT unbounded here in the final design (retention window, step 16) but
// for now we persist all currently-known ones; waiters/listeners are transient and
// deliberately excluded, mirroring mess's persist.go.
type Snapshot struct {
	Seq             int                       `json:"seq"`
	Identities      []Identity                `json:"identities,omitempty"`
	Pipelines       []Pipeline                `json:"pipelines,omitempty"`
	Locks           []FileLock                `json:"locks,omitempty"`
	CommitSeq       map[string]int            `json:"commitSeq,omitempty"`
	LastDeployedSeq map[string]int            `json:"lastDeployedSeq,omitempty"`
	StageInstances  []StageInstance           `json:"stageInstances,omitempty"`
	DeployHistory   map[string][]DeployRecord `json:"deployHistory,omitempty"`
}

func LoadSnapshotFile(path string) (Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Snapshot{}, nil
		}
		return Snapshot{}, err
	}
	if len(data) == 0 {
		return Snapshot{}, nil
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// SaveSnapshot writes the snapshot atomically: marshal -> write to path+".tmp" -> rename.
// Mirrors mess/persist.go's saveSnapshot exactly.
func SaveSnapshot(path string, snap Snapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
