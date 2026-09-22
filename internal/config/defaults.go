package config

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultsFile is the user-level project defaults file. It deliberately has
// a smaller schema than bees.toml: only agent profiles and the selectors that
// choose them can be shared between projects.
const DefaultsFile = "defaults.toml"

// defaultsMigrations is independent of the bees.toml migrations. The file is
// introduced at the current format version, so there are no older versions to
// migrate today. A future profile-related migration belongs in both maps.
var defaultsMigrations = map[int]migration{}

// DefaultConfigDir is where user-level busybees configuration lives:
// $XDG_CONFIG_HOME/bees when XDG_CONFIG_HOME is absolute, or ~/.config/bees.
func DefaultConfigDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "bees")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "bees")
	}
	return filepath.Join(home, ".config", "bees")
}

// DefaultDefaultsPath is the user-level defaults file read below every
// project bees.toml and below the global review configuration.
func DefaultDefaultsPath() string { return filepath.Join(DefaultConfigDir(), DefaultsFile) }

// UserDefaults is what a defaults.toml holds: agent profiles and the
// selectors that choose them, in the shape of the bees.toml tables they
// stand in for ([profiles.*], [global] and [roles.*]). Loading has held the
// file to bees.toml's rules: an unknown or misplaced key, and a value of
// the wrong shape, are errors naming the file. What the references name is
// checked after the merge, where either file may define the profile.
type UserDefaults struct {
	Version  int
	Global   RoleSettings
	Roles    map[string]RoleSettings
	Profiles map[string]AgentProfile
}

// LoadUserDefaults reads the user defaults file, DefaultDefaultsPath. A file
// that is not there is not an error: it loads as nil.
func LoadUserDefaults() (*UserDefaults, error) {
	parsed, err := loadUserDefaults()
	if err != nil || parsed == nil {
		return nil, err
	}
	d := parsed.config
	return &d, nil
}

type parsedDefaults struct {
	config   UserDefaults
	metadata toml.MetaData
	path     string
}

func loadUserDefaults() (*parsedDefaults, error) {
	path := DefaultDefaultsPath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseUserDefaults(string(data), path)
}

func parseUserDefaults(text, path string) (*parsedDefaults, error) {
	version, err := fileVersion(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if version < CurrentVersion {
		text, err = migrateFile(text, version, CurrentVersion, defaultsMigrations, DefaultsFile)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	var d UserDefaults
	md, err := toml.Decode(text, &d)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}
		return nil, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	for _, key := range md.Keys() {
		if !allowedDefaultsKey(key) {
			return nil, fmt.Errorf("%s: key %s is not allowed in %s", path, key.String(), DefaultsFile)
		}
	}
	if errs := d.validateSyntax(); len(errs) > 0 {
		return nil, fmt.Errorf("invalid %s:\n  - %s: %s", DefaultsFile, path, strings.Join(errs, "\n  - "+path+": "))
	}
	return &parsedDefaults{config: d, metadata: md, path: path}, nil
}

func allowedDefaultsKey(key toml.Key) bool {
	if len(key) == 0 {
		return false
	}
	switch key[0] {
	case "version":
		return len(key) == 1
	case "profiles":
		return len(key) <= 2 || (len(key) == 3 && slices.Contains([]string{"agent", "model", "fallback", "effort", "sandbox"}, key[2]))
	case "global":
		return len(key) == 1 || (len(key) >= 2 && len(key) <= 3 && slices.Contains([]string{"profile", "profile_by_size"}, key[1]))
	case "roles":
		if len(key) <= 2 {
			return true
		}
		if slices.Contains([]string{"profile", "profile_by_size"}, key[2]) {
			return len(key) <= 4
		}
		return key[1] == RoleReviewer && slices.Contains([]string{"brief_profile", "judge_profile", "angle_profiles", "angles"}, key[2]) && len(key) <= 4
	default:
		return false
	}
}

// validateSyntax checks values that remain wrong even when the project layer
// replaces another value. References and fallback chains intentionally wait
// for validation of the merged config, where either file may define the
// profile they name.
func (d UserDefaults) validateSyntax() []string {
	var errs []string
	for _, name := range slices.Sorted(maps.Keys(d.Profiles)) {
		errs = append(errs, validateProfileSyntax(name, d.Profiles[name])...)
	}
	for name := range d.Roles {
		if _, err := CanonicalRole(name); err != nil || name != mustCanonical(name) {
			errs = append(errs, fmt.Sprintf("roles.%s: unknown role (want one of %s)", name, strings.Join(Roles, ", ")))
		}
	}
	check := func(scope string, rs RoleSettings, reviewer bool) {
		for _, size := range slices.Sorted(maps.Keys(rs.ProfileBySize)) {
			if !slices.Contains(Sizes, size) {
				errs = append(errs, fmt.Sprintf("%s.profile_by_size: unknown size %q (want one of %s)", scope, size, strings.Join(Sizes, ", ")))
			}
		}
		if !reviewer {
			return
		}
		for _, size := range slices.Sorted(maps.Keys(rs.Angles)) {
			angles := rs.Angles[size]
			if !slices.Contains(Sizes, size) {
				errs = append(errs, fmt.Sprintf("%s.angles: unknown size %q (want one of %s)", scope, size, strings.Join(Sizes, ", ")))
				continue
			}
			if len(angles) == 0 {
				errs = append(errs, fmt.Sprintf("%s.angles.%s must name at least one angle (want one or more of %s)", scope, size, strings.Join(KnownReviewAngles, ", ")))
			}
			for _, angle := range angles {
				if !slices.Contains(KnownReviewAngles, angle) {
					errs = append(errs, fmt.Sprintf("%s.angles.%s: unknown angle %q (want one or more of %s)", scope, size, angle, strings.Join(KnownReviewAngles, ", ")))
				}
			}
		}
		for _, angle := range slices.Sorted(maps.Keys(rs.AngleProfiles)) {
			if !slices.Contains(KnownReviewAngles, angle) {
				errs = append(errs, fmt.Sprintf("%s.angle_profiles.%s: unknown angle %q (want one of %s)", scope, angle, angle, strings.Join(KnownReviewAngles, ", ")))
			}
		}
	}
	check("global", d.Global, false)
	for name, rs := range d.Roles {
		check("roles."+name, rs, name == RoleReviewer)
	}
	return errs
}

func (c *Config) mergeUserDefaults(d *parsedDefaults, projectMD toml.MetaData) {
	if d == nil {
		return
	}

	c.sources = metadataSources(d.metadata, d.path)
	c.defaultsPath = d.path
	projectPath := c.Path
	if c.Profiles == nil {
		c.Profiles = map[string]AgentProfile{}
	}
	for name, profile := range d.config.Profiles {
		if _, projectDefined := c.Profiles[name]; !projectDefined {
			c.Profiles[name] = profile
		}
	}
	for name := range c.Profiles {
		if !projectMD.IsDefined("profiles", name) {
			continue
		}
		deleteSourcePrefix(c.sources, "profiles."+name)
		c.sources["profiles."+name] = projectPath
	}

	c.Global = mergeDefaultRole(d.config.Global, c.Global, projectMD, []string{"global"}, false)
	if c.Roles == nil {
		c.Roles = map[string]RoleSettings{}
	}
	for name, defaults := range d.config.Roles {
		c.Roles[name] = mergeDefaultRole(defaults, c.Roles[name], projectMD, []string{"roles", name}, name == RoleReviewer)
	}
	for _, key := range projectMD.Keys() {
		c.sources[key.String()] = projectPath
	}
}

func mergeDefaultRole(defaults, project RoleSettings, md toml.MetaData, path []string, reviewer bool) RoleSettings {
	defined := func(parts ...string) bool { return md.IsDefined(append(slices.Clone(path), parts...)...) }
	if !defined("profile") {
		project.Profile = defaults.Profile
	}
	project.ProfileBySize = mergeStringMap(defaults.ProfileBySize, project.ProfileBySize)
	if reviewer {
		if !defined("brief_profile") {
			project.BriefProfile = defaults.BriefProfile
		}
		if !defined("judge_profile") {
			project.JudgeProfile = defaults.JudgeProfile
		}
		project.AngleProfiles = mergeStringMap(defaults.AngleProfiles, project.AngleProfiles)
		project.Angles = mergeStringSliceMap(defaults.Angles, project.Angles)
	}
	return project
}

func mergeStringMap(defaults, project map[string]string) map[string]string {
	if len(defaults) == 0 && len(project) == 0 {
		return project
	}
	merged := maps.Clone(defaults)
	if merged == nil {
		merged = map[string]string{}
	}
	maps.Copy(merged, project)
	return merged
}

func mergeStringSliceMap(defaults, project map[string][]string) map[string][]string {
	if len(defaults) == 0 && len(project) == 0 {
		return project
	}
	merged := make(map[string][]string, len(defaults)+len(project))
	for key, value := range defaults {
		merged[key] = slices.Clone(value)
	}
	for key, value := range project {
		merged[key] = slices.Clone(value)
	}
	return merged
}

func metadataSources(md toml.MetaData, path string) map[string]string {
	sources := map[string]string{}
	for _, key := range md.Keys() {
		sources[key.String()] = path
	}
	return sources
}

func deleteSourcePrefix(sources map[string]string, prefix string) {
	for key := range sources {
		if key == prefix || strings.HasPrefix(key, prefix+".") {
			delete(sources, key)
		}
	}
}
