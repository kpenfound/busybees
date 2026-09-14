package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const projectTOML = "version = 2\n[project]\nrepo = \"a/b\"\n"

// writeFile writes body to name under dir, creating the directories on the way.
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMachine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	foo := writeFile(t, root, "foo/bees.toml", projectTOML)
	bar := writeFile(t, home, "src/bar/bees.toml", "version = 2\n[project]\nrepo = \"c/d\"\n")
	baz := writeFile(t, root, "elsewhere/baz.toml", "version = 1\n[project]\nrepo = \"e/f\"\n")
	machine := writeFile(t, root, "machine/bees.toml",
		"projects = [\n  \"../foo/bees.toml\",\n  \"~/src/bar/bees.toml\",\n  \""+baz+"\",\n]\n")

	m, err := LoadMachine(machine)
	if err != nil {
		t.Fatal(err)
	}
	if m.Path != machine {
		t.Errorf("Path = %q, want %q", m.Path, machine)
	}
	if len(m.Projects) != 3 || m.Projects[0] != "../foo/bees.toml" {
		t.Errorf("Projects = %q, want the entries as written", m.Projects)
	}
	var paths, repos []string
	for _, cfg := range m.Configs {
		paths = append(paths, cfg.Path)
		repos = append(repos, cfg.Project.Repo)
	}
	if want := []string{foo, bar, baz}; strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Errorf("config paths = %q, want %q", paths, want)
	}
	if want := "a/b c/d e/f"; strings.Join(repos, " ") != want {
		t.Errorf("repos = %q, want %q", repos, want)
	}
	if !m.Configs[2].NeedsRewrite() {
		t.Error("a listed version-1 bees.toml should load migrated in memory")
	}
	if m.MaxDevelopers != 0 {
		t.Errorf("MaxDevelopers = %d, want 0: no cap unless the file sets one", m.MaxDevelopers)
	}
}

// max_developers is the one setting a machine config has besides the
// projects: a cap across all of them.
func TestLoadMachineMaxDevelopers(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "foo/bees.toml", projectTOML)
	m, err := LoadMachine(writeFile(t, root, "machine.toml", "projects = [\"foo/bees.toml\"]\nmax_developers = 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.MaxDevelopers != 3 {
		t.Errorf("MaxDevelopers = %d, want 3", m.MaxDevelopers)
	}
}

// A machine config's retention_period reaches every listed project that does
// not set its own; a project's own value wins.
func TestLoadMachineRetentionPeriod(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "unset/bees.toml", projectTOML)
	writeFile(t, root, "set/bees.toml", projectTOML+"[scheduler]\nretention_period = \"2h\"\n")
	m, err := LoadMachine(writeFile(t, root, "machine.toml", "projects = [\"unset/bees.toml\", \"set/bees.toml\"]\nretention_period = \"72h\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Configs[0].Scheduler.RetentionPeriod.Duration; got != 72*time.Hour {
		t.Errorf("unset project: retention_period = %s, want the machine's 72h", got)
	}
	if got := m.Configs[1].Scheduler.RetentionPeriod.Duration; got != 2*time.Hour {
		t.Errorf("project setting its own: retention_period = %s, want its 2h", got)
	}

	m, err = LoadMachine(writeFile(t, root, "machine.toml", "projects = [\"unset/bees.toml\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.RetentionPeriod != nil {
		t.Errorf("RetentionPeriod = %s, want nil when the file does not set it", m.RetentionPeriod)
	}
	if got := m.Configs[0].Scheduler.RetentionPeriod.Duration; got != DefaultRetentionPeriod {
		t.Errorf("no machine value: retention_period = %s, want the default %s", got, DefaultRetentionPeriod)
	}
}

// Every rejection names the machine config and, for an entry, the entry.
func TestLoadMachineRejects(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "good/bees.toml", projectTOML)
	writeFile(t, root, "bad/bees.toml", "version = 2\n[project]\nrepo = \"a/b\"\nbogus = 1\n")
	writeFile(t, root, "nested/bees.toml", "projects = [\"../good/bees.toml\"]\n")
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"empty list", "projects = []\n", []string{"projects is empty"}},
		{"negative cap", "projects = [\"good/bees.toml\"]\nmax_developers = -1\n", []string{"max_developers must be >= 0"}},
		{"zero retention", "projects = [\"good/bees.toml\"]\nretention_period = \"0s\"\n", []string{"retention_period", "must be a positive duration"}},
		{"negative retention", "projects = [\"good/bees.toml\"]\nretention_period = \"-1h\"\n", []string{"retention_period", "must be a positive duration"}},
		{"unparseable retention", "projects = [\"good/bees.toml\"]\nretention_period = \"a day\"\n", []string{"retention_period", "invalid duration"}},
		{"not a list", "projects = \"good/bees.toml\"\n", []string{"parse"}},
		{"unknown key", "projects = [\"good/bees.toml\"]\n[project]\nrepo = \"a/b\"\n", []string{"unknown keys: project, project.repo", "a machine config has only projects, max_developers and retention_period"}},
		{"blank entry", "projects = [\"good/bees.toml\", \" \"]\n", []string{`projects[1] = " "`, "is empty"}},
		{"missing file", "projects = [\"nope/bees.toml\"]\n", []string{`projects[0] = "nope/bees.toml"`, "no such file"}},
		{"directory", "projects = [\"dir\"]\n", []string{`projects[0] = "dir"`, "is a directory: name the bees.toml file in it"}},
		{"invalid project", "projects = [\"good/bees.toml\", \"bad/bees.toml\"]\n", []string{`projects[1] = "bad/bees.toml"`, "unknown keys: project.bogus"}},
		{"duplicate", "projects = [\"good/bees.toml\", \"./dir/../good/bees.toml\"]\n", []string{`projects[1]`, "same bees.toml as projects[0]"}},
		{"machine inside machine", "projects = [\"nested/bees.toml\"]\n", []string{`projects[0] = "nested/bees.toml"`, "is a machine config"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := writeFile(t, root, "machine.toml", tc.body)
			m, err := LoadMachine(p)
			if err == nil {
				t.Fatalf("loaded %+v, want an error", m)
			}
			for _, w := range append([]string{p}, tc.want...) {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
		})
	}
}

func TestDetectKind(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, body string
		want       Kind
	}{
		{"project", projectTOML, KindProject},
		{"invalid project", "version = 2\n[project]\nbogus = 1\n", KindProject},
		{"machine", "projects = [\"a/bees.toml\"]\n", KindMachine},
		{"empty machine", "projects = []\n", KindMachine},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DetectKind(writeFile(t, dir, "bees.toml", tc.body))
			if err != nil || got != tc.want {
				t.Fatalf("DetectKind = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	if _, err := DetectKind(writeFile(t, dir, "broken.toml", "projects = [")); err == nil {
		t.Error("unparseable file: want an error")
	}
}

// Load refuses a machine config with an error a caller can recognise, rather
// than reporting projects as an unknown key of a broken bees.toml.
func TestLoadRefusesAMachineConfig(t *testing.T) {
	p := writeConfig(t, "projects = [\"foo/bees.toml\"]\n")
	_, err := Load(p)
	if !errors.Is(err, ErrMachineConfig) {
		t.Fatalf("Load = %v, want ErrMachineConfig", err)
	}
	if !strings.Contains(err.Error(), p) {
		t.Errorf("error %q does not name the file", err)
	}
}
