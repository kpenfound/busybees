package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/core/agent/agentbin"
)

// vcsProbe is the script an image is run with to find its VCS executables:
// what executablePaths finds on the host, found inside the image. It prints
// "f <path>" for a file and "d <path>" for git's directory of subcommand
// programs, each with its links resolved, and fails when it cannot resolve
// one: an executable it found and could not name is not one to leave alone.
var vcsProbe = `set -f
dirs="$PATH:` + strings.Join(binDirs, ":") + `"
IFS=:
for d in $dirs; do
  case "$d" in /*) ;; *) continue ;; esac
  for n in ` + strings.Join(VCSExecutables, " ") + `; do
    p="$d/$n"
    [ -f "$p" ] || continue
    r=$(readlink -f "$p") || exit 1
    echo "f $r"
    [ "$n" = git ] || continue
    for from in "$p" "$r"; do
      up=$(dirname "$(dirname "$from")")
      for rel in ` + strings.Join(gitExecDirs, " ") + `; do
        [ -d "$up/$rel" ] || continue
        x=$(readlink -f "$up/$rel") || exit 1
        echo "d $x"
      done
    done
  done
done
`

// imagePath is a path the probe found inside an image.
type imagePath struct {
	path string
	dir  bool
}

// imageVCS runs the image once, with no network and nothing of the host, and
// returns the VCS executables it holds.
func (r *Runner) imageVCS(ctx context.Context, image string) ([]imagePath, error) {
	cmd := agentbin.CommandContext(ctx, r.dockerBin(), "run", "--rm", "--network", "none", "--entrypoint", "/bin/sh", image, "-c", vcsProbe)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			err = fmt.Errorf("%w: %s", err, bytes.TrimSpace(exit.Stderr))
		}
		return nil, fmt.Errorf("look for the VCS executables of image %s: %w", image, err)
	}
	return parseImageVCS(string(out))
}

func parseImageVCS(out string) ([]imagePath, error) {
	var found []imagePath
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		// A line with no path has an empty one, which is not absolute.
		kind, path, _ := strings.Cut(line, " ")
		if (kind != "f" && kind != "d") || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
			return nil, fmt.Errorf("the image's VCS executables: %q is not a path the probe prints", line)
		}
		if p := (imagePath{path: path, dir: kind == "d"}); !slices.Contains(found, p) {
			found = append(found, p)
		}
	}
	return found, nil
}

// writeMasks writes the stand-ins into dir and returns the binds that lay
// them over the paths found: an empty directory over a directory, the
// stand-in executable over a file outside every such directory.
func writeMasks(dir string, found []imagePath) ([]Bind, error) {
	if len(found) == 0 {
		return nil, nil
	}
	standIn, empty := filepath.Join(dir, "denied"), filepath.Join(dir, "empty")
	script := "#!/bin/sh\necho \"$0: not granted to this session\" >&2\nexit 126\n"
	if err := os.WriteFile(standIn, []byte(script), 0o555); err != nil {
		return nil, err
	}
	if err := os.Mkdir(empty, 0o555); err != nil {
		return nil, err
	}
	var masks []Bind
	for _, f := range found {
		source := standIn
		if f.dir {
			source = empty
		} else if slices.ContainsFunc(found, func(d imagePath) bool { return d.dir && inside(d.path, f.path) }) {
			continue
		}
		masks = append(masks, Bind{Source: source, Destination: f.path, Access: ReadOnly})
	}
	slices.SortFunc(masks, func(a, b Bind) int { return strings.Compare(a.Destination, b.Destination) })
	return masks, nil
}
