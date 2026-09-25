// Package agenttest supplies offline executables for runner and caller
// contract tests: agents, the container engine, the Docker Sandboxes CLI and
// a host MCP server.
package agenttest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Docker writes a fake container engine. It answers `network`, `rm`, `image`
// and `build`, and runs what `run` is given on this machine, recording its
// arguments and environment in the directory sessionVariable names. A `run`
// with --entrypoint is the look an agent.NewContainer session takes into its
// image before any turn: its arguments are recorded in docker-probe.txt
// beside the script, and it prints image-vcs.txt from there, the VCS
// executables the image is to have ("f <path>" or "d <path>" a line; none
// without the file), or fails when a file named fail-probe is there.
func Docker(t *testing.T, image, sessionVariable string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
set -e
here="$(dirname "$0")"
for arg in "$@"; do
  if [ "$arg" = --entrypoint ]; then
    printf '%s\n' "$@" > "$here/docker-probe.txt"
    if [ -f "$here/fail-probe" ]; then
      echo "Unable to find image locally" >&2
      exit 125
    fi
    [ ! -f "$here/image-vcs.txt" ] || cat "$here/image-vcs.txt"
    exit 0
  fi
done
case "$1" in
network)
  echo 172.17.0.1
  exit 0
  ;;
rm)
  echo "$@" >> "$here/docker-rm.txt"
  exit 0
  ;;
image)
  tag="$5"
  echo "$tag" >> "$here/docker-inspect.txt"
  [ -f "$here/images.txt" ] && grep -qx "$tag" "$here/images.txt"
  exit $?
  ;;
build)
  printf '%s\n' "$@" > "$here/docker-build.txt"
  tag=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --tag) tag="$2"; shift 2 ;;
    --file) cp "$2" "$here/Dockerfile.built"; shift 2 ;;
    *) shift ;;
    esac
  done
  echo "#1 [internal] load build definition"
  if [ -f "$here/fail-build" ]; then
    echo "ERROR: process \"/bin/sh -c false\" did not complete successfully: exit code: 1" >&2
    exit 1
  fi
  echo "$tag" >> "$here/images.txt"
  exit 0
  ;;
esac
printf '%s\n' "$@" > "$SESSION_DIRECTORY/docker-args.txt"
env > "$SESSION_DIRECTORY/docker-env.txt"
while [ $# -gt 0 ]; do
  case "$1" in
  --cidfile) echo "abc123" > "$2"; shift 2 ;;
  ` + image + `) shift; break ;;
  *) shift ;;
  esac
done
exec "$@"
`
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(script, "SESSION_DIRECTORY", sessionVariable)), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}
func MCPServer(t *testing.T, sessionVariable string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp-server")
	script := `#!/bin/sh
env > "$SESSION_DIRECTORY/server-env.txt"
echo $$ > "$SESSION_DIRECTORY/server-pid.txt"
printf '%s\n' "$@" > "$SESSION_DIRECTORY/server-args.txt"
echo "listening on 127.0.0.1:45678"
exec sleep 60
`
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(script, "SESSION_DIRECTORY", sessionVariable)), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Sbx writes a fake Docker Sandboxes CLI. `create` records its arguments in
// sbx-create.txt beside the script and fails when a file named fail-create
// is there; `exec --interactive`, the session's command, records its
// arguments and the client's environment in the directory sessionVariable
// names (sbx-exec-args.txt, sbx-exec-env.txt) and runs the command after the
// sandbox name on this machine; `exec --workdir`, a probe the runner runs
// before the session's command, appends its arguments to sbx-probe.txt
// beside the script and runs its command the same way; any other `exec`, a
// setup command, is appended to sbx-setup.txt beside the script, one
// argument per line, runs nothing and fails when a file named fail-setup
// is there; `rm` records
// its arguments in sbx-rm.txt beside the script; `version` prints one.
func Sbx(t *testing.T, sessionVariable string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sbx")
	script := `#!/bin/sh
set -e
here="$(dirname "$0")"
case "$1" in
version)
  echo "sbx version 0.42.0"
  exit 0
  ;;
create)
  printf '%s\n' "$@" > "$here/sbx-create.txt"
  if [ -f "$here/fail-create" ]; then
    echo "Error: sandbox name must not contain underscores" >&2
    exit 1
  fi
  exit 0
  ;;
rm)
  printf '%s\n' "$@" >> "$here/sbx-rm.txt"
  exit 0
  ;;
exec)
  if [ "$2" = "--workdir" ]; then
    printf '%s\n' "$@" >> "$here/sbx-probe.txt"
    shift
    while [ $# -gt 0 ]; do
      case "$1" in
      --env|--workdir) shift 2 ;;
      --interactive) shift ;;
      *) shift; break ;;
      esac
    done
    exec "$@"
  fi
  if [ "$2" != "--interactive" ]; then
    printf '%s\n' "$@" >> "$here/sbx-setup.txt"
    if [ -f "$here/fail-setup" ]; then
      echo "curl: (6) Could not resolve host: dl.dagger.io" >&2
      exit 1
    fi
    exit 0
  fi
  printf '%s\n' "$@" > "$SESSION_DIRECTORY/sbx-exec-args.txt"
  env > "$SESSION_DIRECTORY/sbx-exec-env.txt"
  shift
  while [ $# -gt 0 ]; do
    case "$1" in
    --interactive) shift ;;
    --env|--workdir) shift 2 ;;
    *) shift; break ;;
    esac
  done
  exec "$@"
  ;;
esac
echo "sbx: unknown command $1" >&2
exit 1
`
	if err := os.WriteFile(p, []byte(strings.ReplaceAll(script, "SESSION_DIRECTORY", sessionVariable)), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Script writes a fake executable under the test's temporary directory.
func Script(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -e\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
