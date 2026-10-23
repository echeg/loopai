package runs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// schemaVersion is the version of the descriptor and outcome records this package writes and reads.
const schemaVersion = 1

// Origin kinds recorded in a descriptor.
const (
	OriginCLI        = "cli"         // started from a shell
	OriginT3Terminal = "t3-terminal" // started with a terminal in an existing T3 thread
	OriginT3Thread   = "t3-thread"   // started by the ACP provider of a loopai T3 thread
)

// Origin records who started a run. ThreadID is set for OriginT3Terminal, and for OriginT3Thread
// unless the provider could not look its thread up; ACPSessionID is set only for OriginT3Thread.
type Origin struct {
	Kind         string `json:"kind"`
	ThreadID     string `json:"thread_id,omitempty"`
	ACPSessionID string `json:"acp_session_id,omitempty"`
}

// Descriptor is run.json, the identity and launch record of one run across its attempts. It
// deliberately has no field for a T3 token: a token never reaches a file.
type Descriptor struct {
	Version     int               `json:"version"`
	ID          string            `json:"id"`
	Attempt     int               `json:"attempt"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	PID         int               `json:"pid"`
	PIDStart    string            `json:"pid_start"`  // ProcessStartToken of PID
	Executable  string            `json:"executable"` // absolute path of the loopai binary
	Argv        []string          `json:"argv"`       // launch arguments without the detach-only flags
	EnvOptions  map[string]string `json:"env_options,omitempty"`
	Cwd         string            `json:"cwd"`  // the invoking checkout
	Mode        string            `json:"mode"` // full or tasks-only
	Plan        string            `json:"plan"` // plan path relative to Cwd
	Branch      string            `json:"branch"`
	ProgressLog string            `json:"progress_log"`
	Worktree    string            `json:"worktree"`
	ConsoleLog  string            `json:"console_log"`
	Origin      Origin            `json:"origin"`
}

// SaveDescriptor writes d to dir/run.json with mode 0600. It stamps the schema version and
// UpdatedAt (and CreatedAt when unset) on d, so the caller keeps what was written.
func SaveDescriptor(dir string, d *Descriptor) error {
	d.Version = schemaVersion
	d.UpdatedAt = time.Now().UTC()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = d.UpdatedAt
	}
	if err := saveRecord(filepath.Join(dir, DescriptorFile), d); err != nil {
		return fmt.Errorf("save run descriptor: %w", err)
	}
	return nil
}

// LoadDescriptor reads dir/run.json. A missing file wraps fs.ErrNotExist; a corrupt one, an
// unknown schema version, or an id that does not name dir is an error.
func LoadDescriptor(dir string) (Descriptor, error) {
	path := filepath.Join(dir, DescriptorFile)
	var d Descriptor
	if err := loadRecord(path, &d); err != nil {
		return Descriptor{}, fmt.Errorf("load run descriptor: %w", err)
	}
	if d.Version != schemaVersion {
		return Descriptor{}, fmt.Errorf("load run descriptor %s: unsupported version %d", path, d.Version)
	}
	if d.ID != filepath.Base(dir) {
		return Descriptor{}, fmt.Errorf("load run descriptor %s: id %q does not match its directory", path, d.ID)
	}
	return d, nil
}

// saveRecord writes value as indented JSON through WriteFileAtomic with mode 0600.
func saveRecord(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	return WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// loadRecord decodes the JSON file at path into value.
func loadRecord(path string, value any) error {
	data, err := os.ReadFile(path) //nolint:gosec // path is a registry file under the runs directory
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}
