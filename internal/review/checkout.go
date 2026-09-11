package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The checkout the angle sessions read. Before the angles run, the pull
// request's head is cloned into the review's artifact directory
// (CheckoutDir, angles.go) by a container: an Alpine image with git,
// built here the first time and kept, runs `git fetch` of
// refs/pull/<number>/head from the pull request's own repository over
// HTTPS into a bind-mounted host directory and exits. That reference
// resolves a fork's pull request as well as one from a branch of the
// repository, which the head branch's name would not, and it is the head
// as it is when the review runs: a pull request that gains a commit
// during the review is the race every freshly gathered source has.
//
// Nothing runs inside the container after the clone: the angle sessions
// run on the host (agent.go), under their read-only restriction, in the
// directory the container filled. The image needs git alone, not the
// agent.
//
// A configured github.token authenticates the clone. It reaches the
// container as an environment variable named on `docker run`'s command
// line and read from the client's own environment, never as a value on an
// argument, the way the factory's container sessions carry theirs. With no
// token the clone is anonymous, which a private repository refuses.
//
// The container engine is optional. A machine without docker, an image
// that does not build, a clone that fails (no network, no token for a
// private repository) all leave the review where it was: Angles.Run says
// so and runs the angles in the checkout it was given or in the empty
// scratch directory, as it would without this file. Nothing configures
// that; there is no key to turn the checkout on or off.

// checkoutDockerfile builds the image the checkout runs in: Alpine with git,
// and nothing else.
const checkoutDockerfile = "FROM alpine:3.22\nRUN apk add --no-cache git\n"

// checkoutImage is the repository the built image belongs to; its tag is a
// hash of the Dockerfile, so a change to the Dockerfile builds a new one.
const checkoutImage = "bees-review-checkout"

// checkoutMount is where the host directory is mounted inside the
// container, and the directory the clone runs in.
const checkoutMount = "/src"

// CheckoutTokenVar is the environment variable the token reaches the
// container as.
const CheckoutTokenVar = "GIT_TOKEN"

// DefaultCheckoutTimeout is how long the build and the clone together may
// take before the review goes on without the checkout.
const DefaultCheckoutTimeout = 5 * time.Minute

// checkoutScript is what the container runs, through `sh -c`, with the
// repository's URL as $1 and the reference to fetch as $2. The clone is
// shallow: the sessions read files and never run git, so the history is
// nothing they could reach. The token is read from the environment by a
// credential helper, so it is on no command line inside the container
// either; with no token the helper answers an empty password and a
// repository that wants one refuses the fetch.
const checkoutScript = `set -e
git init -q .
git remote add origin "$1"
git -c credential.helper='!f() { echo username=x-access-token; echo "password=$GIT_TOKEN"; }; f' fetch -q --depth 1 origin "$2"
git checkout -q --detach FETCH_HEAD
`

// Checkout clones a pull request's head into a directory, in a container.
type Checkout struct {
	// DockerBin is the container engine's client, "docker" when it is
	// empty.
	DockerBin string
	// Token authenticates the clone, and "" clones anonymously.
	Token string
	// Timeout bounds the build and the clone together,
	// DefaultCheckoutTimeout when it is zero.
	Timeout time.Duration
}

// Run clones ref's head into dir, creating it. An error is a checkout
// that could not be made: no docker, an image that did not build, a clone
// that failed; dir is removed again, so nothing is left of a checkout that
// is not one, and the caller runs the sessions elsewhere.
func (c *Checkout) Run(ctx context.Context, ref Ref, dir string) error {
	docker := c.DockerBin
	if docker == "" {
		docker = "docker"
	}
	if _, err := exec.LookPath(docker); err != nil {
		return fmt.Errorf("%s is not installed", docker)
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultCheckoutTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	image, err := checkoutImageFor(ctx, docker)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := c.clone(ctx, docker, image, ref, dir); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	return nil
}

// checkoutImageFor is the image the checkout runs in: the one its tag
// names when the engine has it, built otherwise. The build context is a
// directory holding the Dockerfile alone.
func checkoutImageFor(ctx context.Context, docker string) (string, error) {
	tag := checkoutTag()
	if exec.CommandContext(ctx, docker, "image", "inspect", "--format", "{{.Id}}", tag).Run() == nil {
		return tag, nil
	}
	buildDir, err := os.MkdirTemp("", "bees-review-checkout-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(buildDir) }()
	dockerfile := filepath.Join(buildDir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte(checkoutDockerfile), 0o644); err != nil {
		return "", err
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, docker, "build", "--tag", tag, "--file", dockerfile, buildDir)
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s build --tag %s: %w%s", docker, tag, err, tail(out.String()))
	}
	return tag, nil
}

// checkoutTag names the image checkoutDockerfile builds.
func checkoutTag() string {
	sum := sha256.Sum256([]byte(checkoutDockerfile))
	return checkoutImage + ":" + hex.EncodeToString(sum[:8])
}

// clone runs the container: removed when it exits, dir bind-mounted at
// checkoutMount and the clone run there as the host's user, so what it
// writes into the mount is the host's to remove. The token's variable is
// named on the command line and its value laid over the client's
// environment; a value the host's environment happens to hold under that
// name is dropped first, so the container gets the configured token and
// only that. Git is told not to prompt, so a refused clone fails instead
// of waiting for a terminal there is none of.
func (c *Checkout) clone(ctx context.Context, docker, image string, ref Ref, dir string) error {
	source := dir
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		source = real
	}
	args := []string{
		"run", "--rm", "--init",
		"--mount", "type=bind,source=" + source + ",destination=" + checkoutMount,
		"--workdir", checkoutMount,
		"--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		"--env", "HOME=/tmp",
		"--env", "GIT_TERMINAL_PROMPT=0",
		"--env", "GIT_CONFIG_COUNT=1",
		"--env", "GIT_CONFIG_KEY_0=safe.directory",
		"--env", "GIT_CONFIG_VALUE_0=*",
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, CheckoutTokenVar+"=") {
			env = append(env, kv)
		}
	}
	if c.Token != "" {
		args = append(args, "--env", CheckoutTokenVar)
		env = append(env, CheckoutTokenVar+"="+c.Token)
	}
	args = append(args, image, "sh", "-c", checkoutScript, "sh", "https://github.com/"+ref.Repo+".git", fmt.Sprintf("refs/pull/%d/head", ref.Number))
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, docker, args...)
	cmd.Env = env
	cmd.Stdout = &out
	cmd.Stderr = &out
	// A clone that ran out of time is stopped, not only its client: the
	// client forwards a termination signal to the container, and --init
	// makes sure the shell inside dies of it.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("clone %s: the checkout ran out of time (%w)", ref, ctx.Err())
		}
		return fmt.Errorf("clone %s: %w%s", ref, err, tail(out.String()))
	}
	return nil
}
