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

// The checkout the diff is read from and the angle sessions read. As the
// context is gathered (Pipeline.Gather, context.go), the pull request's
// head is cloned into the review's artifact directory (CheckoutDir,
// angles.go) by a container: an Alpine image with git, built here the
// first time and kept, runs `git fetch` of refs/pull/<number>/head from
// the pull request's own repository over HTTPS into a bind-mounted host
// directory, then of the base branch's tip beside it (CheckoutBaseRef),
// and exits. The head reference resolves a fork's pull request as well as
// one from a branch of the repository, which the head branch's name would
// not, and it is the head as it is when the review runs: a pull request
// that gains a commit during the review is the race every freshly
// gathered source has. The base is what the diff source diffs the head
// against (checkoutDiff): read here, the diff has no limit on the number
// of files a pull request changes, which GitHub's own diff has.
//
// Nothing runs inside the container after the clone: the diff is read on
// the host with the host's git, and the angle sessions run on the host
// (agent.go), under their read-only restriction, in the directory the
// container filled. The image needs git alone, not the agent.
//
// A configured github.token authenticates the clone. It reaches the
// container as an environment variable named on `docker run`'s command
// line and read from the client's own environment, never as a value on an
// argument, the way the factory's container sessions carry theirs. With no
// token the clone is anonymous, which a private repository refuses.
//
// The container engine is optional. A machine without docker, an image
// that does not build, a clone that fails (no network, no token for a
// private repository) all leave the review where it was: the diff source
// reads the diff through gh instead, and Angles.Run runs the angles in
// the checkout it was given or in the empty scratch directory, as it
// would without this file, and the review says so. Nothing configures
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

// CheckoutBaseRef is the reference inside a checkout that points at the
// tip of the pull request's base branch, fetched beside the head, and
// what checkoutDiff diffs the checked-out head against. A Clone of the
// caller's own that sets it lets the diff be read from its checkout too;
// one that does not leaves the diff to gh.
const CheckoutBaseRef = "refs/review/base"

// checkoutHeadRef is the reference the pull request's head is fetched
// into before it is checked out, detached.
const checkoutHeadRef = "refs/review/head"

// checkoutScript is what the container runs, through `sh -c`, with the
// repository's URL as $1, the head reference to fetch as $2 and the base
// branch's name as $3, or "" for none. The clone is shallow, each tip on
// its own: the sessions read files and never run git, so the history is
// nothing they could reach, and the diff is a plain diff of the two trees
// (checkoutDiff). A base that cannot be fetched costs the diff its read
// from the checkout, not the angles their checkout: the head is checked
// out first, and the base fetch is allowed to fail. The token is read
// from the environment by a credential helper, so it is on no command
// line inside the container either; with no token the helper answers an
// empty password and a repository that wants one refuses the fetch.
const checkoutScript = `set -e
git init -q .
git remote add origin "$1"
helper='!f() { echo username=x-access-token; echo "password=$GIT_TOKEN"; }; f'
git -c credential.helper="$helper" fetch -q --depth 1 origin "$2:` + checkoutHeadRef + `"
git checkout -q --detach ` + checkoutHeadRef + `
if [ -n "$3" ]; then
  git -c credential.helper="$helper" fetch -q --depth 1 origin "refs/heads/$3:` + CheckoutBaseRef + `" || true
fi
`

// Checkout clones a pull request's head into a directory, in a container
// or, for a caller that has the head at hand already, by a Clone of its
// own.
type Checkout struct {
	// Clone, when set, makes the checkout instead of the container: it
	// fills dir, which exists and is empty, with ref's head checked out,
	// and an error is a checkout that could not be made. base is the base
	// branch's name, or "" when the caller wants the head alone; a Clone
	// that can point CheckoutBaseRef at what the head is to be diffed
	// against does so, and the diff is read from its checkout. The factory
	// sets it (its reviewer worker has the pull request's branch checked
	// out already, and clones that rather than fetching the head over the
	// network); `bees review` leaves it nil and clones in the container.
	Clone func(ctx context.Context, ref Ref, base, dir string) error
	// DockerBin is the container engine's client, "docker" when it is
	// empty.
	DockerBin string
	// Token authenticates the clone, and "" clones anonymously.
	Token string
	// Timeout bounds the build and the clone together,
	// DefaultCheckoutTimeout when it is zero.
	Timeout time.Duration
}

// Run clones ref's head into dir, creating it, with the tip of the base
// branch named base fetched beside it as CheckoutBaseRef, or the head
// alone when base is "". An error is a checkout that could not be made:
// no docker, an image that did not build, a clone that failed; dir is
// removed again, so nothing is left of a checkout that is not one, and
// the caller reads the diff and runs the sessions elsewhere.
func (c *Checkout) Run(ctx context.Context, ref Ref, base, dir string) error {
	if c.Clone != nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := c.Clone(ctx, ref, base, dir); err != nil {
			_ = os.RemoveAll(dir)
			return err
		}
		return nil
	}
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
	if err := c.clone(ctx, docker, image, ref, base, dir); err != nil {
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
func (c *Checkout) clone(ctx context.Context, docker, image string, ref Ref, base, dir string) error {
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
	args = append(args, image, "sh", "-c", checkoutScript, "sh", "https://github.com/"+ref.Repo+".git", fmt.Sprintf("refs/pull/%d/head", ref.Number), base)
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

// checkoutDiff is the pull request's diff as the checkout in dir has it:
// what changed between the base branch's tip, CheckoutBaseRef, and the
// head that is checked out, as a plain diff of the two trees. Read here,
// on the host and with the host's git, the diff has no limit on the number
// of files it may touch, which the one gh reads from GitHub has.
//
// It is an approximation of the diff GitHub shows, which is the head
// against the merge base of the two: the checkout is two shallow tips
// with no history between them to find a merge base in, so a base branch
// that has moved on since the pull request forked from it shows its own
// later changes here, reversed, as if the pull request undid them. That
// is the best-effort reading every source's gathering makes, and a
// pull request kept up with its base has no such difference.
func checkoutDiff(ctx context.Context, dir string) (string, error) {
	out, _, err := gitRun(ctx, dir, "diff", "--no-color", "--no-ext-diff", CheckoutBaseRef, "HEAD")
	return out, err
}
