package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Kind says which of the two configuration files a path holds.
type Kind int

const (
	// KindProject is a project's bees.toml, loaded with Load.
	KindProject Kind = iota
	// KindMachine is a machine config, the file one bees process managing
	// several projects reads to find their bees.toml files, loaded with
	// LoadMachine.
	KindMachine
)

// machineKey is the top-level key only a machine config has. A bees.toml
// with it fails to load as an unknown key, so its presence alone tells the
// two files apart.
const machineKey = "projects"

// ErrMachineConfig is what Load returns, wrapped with the path, for a machine
// config: a caller that resolved the active config can tell it apart from a
// broken bees.toml with errors.Is.
var ErrMachineConfig = errors.New("is a machine config listing projects, not a project bees.toml")

// Machine is a machine config: the paths of the bees.toml files of the
// projects one bees process manages. Each project keeps its own bees.toml,
// state directory, mailbox and notes; this file only says where they are.
//
//	projects = [
//	  "~/src/foo/bees.toml",
//	  "../bar/bees.toml",
//	]
type Machine struct {
	// Path is the absolute path of the machine config itself.
	Path string `toml:"-"`
	// Projects are the listed paths as written in the file.
	Projects []string `toml:"projects"`
	// Configs are the projects' configs, loaded in the order Projects lists
	// them, each Config's Path the absolute path the entry resolved to.
	Configs []*Config `toml:"-"`
}

// DetectKind reads the file at path and says whether it is a project
// bees.toml or a machine config. It reads only the file's top-level keys: a
// file of either kind that does not validate is still told apart, and the
// loader for its kind reports what is wrong with it.
func DetectKind(path string) (Kind, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return KindProject, err
	}
	kind, err := kindOf(string(data))
	if err != nil {
		return KindProject, fmt.Errorf("%s: %w", path, err)
	}
	return kind, nil
}

func kindOf(text string) (Kind, error) {
	var raw map[string]any
	if _, err := toml.Decode(text, &raw); err != nil {
		return KindProject, fmt.Errorf("parse: %w", err)
	}
	if _, ok := raw[machineKey]; ok {
		return KindMachine, nil
	}
	return KindProject, nil
}

// LoadMachine reads and validates the machine config at path: every entry of
// projects must name an existing file that loads as a project bees.toml.
// Relative entries are relative to the machine config's directory and a
// leading ~/ is the home directory. Errors name the offending entry.
func LoadMachine(path string) (*Machine, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	m := Machine{Path: abs}
	md, err := toml.Decode(string(data), &m)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("%s: unknown keys: %s (a machine config has only %s; project settings belong in each project's bees.toml)", path, strings.Join(keys, ", "), machineKey)
	}
	if len(m.Projects) == 0 {
		return nil, fmt.Errorf("%s: %s is empty: list the path of at least one project's bees.toml", path, machineKey)
	}
	seen := map[string]int{}
	for i, entry := range m.Projects {
		key := fmt.Sprintf("%s[%d] = %q", machineKey, i, entry)
		p, err := m.resolve(entry)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, key, err)
		}
		if j, dup := seen[p]; dup {
			return nil, fmt.Errorf("%s: %s: names the same bees.toml as %s[%d]: list each project once", path, key, machineKey, j)
		}
		seen[p] = i
		st, err := os.Stat(p)
		switch {
		case err != nil:
			return nil, fmt.Errorf("%s: %s: %w", path, key, err)
		case st.IsDir():
			return nil, fmt.Errorf("%s: %s: is a directory: name the bees.toml file in it", path, key)
		}
		cfg, err := Load(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", path, key, err)
		}
		m.Configs = append(m.Configs, cfg)
	}
	return &m, nil
}

// resolve turns one projects entry into an absolute, cleaned path.
func (m *Machine) resolve(entry string) (string, error) {
	p := strings.TrimSpace(entry)
	if p == "" {
		return "", errors.New("is empty: give the path of a project's bees.toml")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~: %w", err)
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(m.Path), p)
	}
	return filepath.Clean(p), nil
}
