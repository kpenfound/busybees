package config

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// checkPiPackage reports what is wrong with one pi_packages entry: nothing
// pi could not be handed as `-e <source>`, and not the adapter every pi
// session loads already, which the list cannot replace or pin: loaded twice,
// its tools and flag would conflict with themselves.
func checkPiPackage(src string) error {
	switch {
	case strings.TrimSpace(src) == "":
		return errors.New("an empty source")
	case strings.HasPrefix(src, "-"):
		return fmt.Errorf("%q starts with \"-\" and would be read as a pi flag", src)
	}
	name, _, _ := strings.Cut(path.Base(strings.TrimPrefix(src, "npm:")), "@")
	if strings.TrimSuffix(name, ".git") == "pi-mcp-adapter" {
		return fmt.Errorf("%q: every pi session loads %s already, and pi_packages only adds packages to it", src, PiMCPAdapter)
	}
	return nil
}
