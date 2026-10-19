package review

import (
	"strings"

	"github.com/kpenfound/agentcore/jev"
	"github.com/kpenfound/busybees/internal/config"
)

// JevConfig is the global review file's [jev] table. Omitted keys take
// agentcore's defaults; explicit empty values are invalid, even without a key.
type JevConfig struct {
	URL       string          `toml:"url"`
	Model     string          `toml:"model"`
	APIKeyEnv string          `toml:"api_key_env"`
	Timeout   config.Duration `toml:"timeout"`
}

func defaultJevConfig() JevConfig {
	s := jev.DefaultSettings()
	return JevConfig{URL: s.URL, Model: s.Model, APIKeyEnv: s.APIKeyEnv, Timeout: config.Duration{Duration: s.Timeout}}
}

// Settings returns settings without credentials or an enabled switch.
// NewJevJudge enables them only while APIKeyEnv holds a value.
func (c JevConfig) Settings() jev.Settings {
	return jev.Settings{URL: c.URL, Model: c.Model, APIKeyEnv: c.APIKeyEnv, Timeout: c.Timeout.Duration}
}

func (c JevConfig) validate() []string {
	s := c.Settings()
	// Validate each field against the other defaults to name every bad key.
	// Settings.Validate's prose is intentionally not used to infer key names.
	var errs []string
	for _, key := range []string{"url", "model", "api_key_env", "timeout"} {
		field := jev.DefaultSettings()
		switch key {
		case "url":
			field.URL = s.URL
		case "model":
			field.Model = s.Model
		case "api_key_env":
			field.APIKeyEnv = s.APIKeyEnv
		case "timeout":
			field.Timeout = s.Timeout
		}
		if err := field.Validate(); err != nil {
			errs = append(errs, "jev."+key+": "+strings.TrimPrefix(err.Error(), "jev "))
		}
	}
	return errs
}
