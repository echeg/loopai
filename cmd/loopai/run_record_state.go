package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/umputun/ralphex/pkg/processor"
)

type runRecordStore struct {
	path string
}

func newRunRecordStore(progressLogPath string) *runRecordStore {
	base := strings.TrimSuffix(filepath.Base(progressLogPath), filepath.Ext(progressLogPath))
	return &runRecordStore{path: filepath.Join(filepath.Dir(progressLogPath), base+".run.json")}
}

func (s *runRecordStore) Load() (processor.RunRecord, bool, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return processor.RunRecord{}, false, nil
	}
	if err != nil {
		return processor.RunRecord{}, false, fmt.Errorf("read run record: %w", err)
	}
	var record processor.RunRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return processor.RunRecord{}, false, fmt.Errorf("parse run record: %w", err)
	}
	return record, true, nil
}

func (s *runRecordStore) Save(record processor.RunRecord) error {
	return saveJSONState(s.path, ".run-record-*.tmp", "run record", record)
}

func (s *runRecordStore) Remove() error {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove run record: %w", err)
	}
	return nil
}
