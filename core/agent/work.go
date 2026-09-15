package agent

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/kpenfound/busybees/core/work"
)

// WorkFile records the caller's work reference independently of session names.
const WorkFile = "work.json"

func WriteWork(dir string, ref work.Ref) error {
	if dir == "" || ref.Key == "" {
		return nil
	}
	b, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".work-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, WorkFile))
}

func ReadWork(dir string) (work.Ref, error) {
	var ref work.Ref
	b, err := os.ReadFile(filepath.Join(dir, WorkFile))
	if os.IsNotExist(err) {
		return ref, nil
	}
	if err != nil {
		return ref, err
	}
	err = json.Unmarshal(b, &ref)
	return ref, err
}
