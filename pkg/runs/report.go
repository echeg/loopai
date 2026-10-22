package runs

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteReport writes the uncapped completion report to report.md in run directory dir with
// mode 0600. The outcome carries a capped copy; report.md keeps the full text even when the
// run produced no report sidecar on its branch.
func WriteReport(dir, report string) error {
	if err := WriteFileAtomic(filepath.Join(dir, ReportFile), []byte(report), 0o600); err != nil {
		return fmt.Errorf("write run report: %w", err)
	}
	return nil
}

// ReadReport returns the report.md text of run directory dir. A missing report wraps
// fs.ErrNotExist.
func ReadReport(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, ReportFile)) //nolint:gosec // report path under the runs directory
	if err != nil {
		return "", fmt.Errorf("read run report: %w", err)
	}
	return string(data), nil
}
