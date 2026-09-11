package review

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

// fakeDocker writes a shell script standing in for the docker CLI and
// returns its path. Beside it: `image inspect` succeeds when the tag asked
// about is a line of images.txt; `build` records its arguments in
// build-args.txt, keeps the Dockerfile it was given as Dockerfile.built,
// fails when a file named fail-build is there, and lists the tag otherwise;
// `run` records its arguments in run-args.txt and the client's environment
// in run-env.txt, fails when a file named fail-run is there, and otherwise
// writes widget.go into the directory mounted at the checkout, the way the
// clone fills it. Every subcommand appends its name to calls.txt.
func fakeDocker(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
here="$(dirname "$0")"
echo "$1" >> "$here/calls.txt"
case "$1" in
image)
  tag="$5"
  [ -f "$here/images.txt" ] && grep -qx "$tag" "$here/images.txt"
  exit $?
  ;;
build)
  printf '%s\n' "$@" > "$here/build-args.txt"
  tag=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --tag) tag="$2"; shift 2 ;;
    --file) cp "$2" "$here/Dockerfile.built"; shift 2 ;;
    *) shift ;;
    esac
  done
  if [ -f "$here/fail-build" ]; then
    echo "ERROR: failed to solve: alpine:3.22: not found" >&2
    exit 1
  fi
  echo "$tag" >> "$here/images.txt"
  exit 0
  ;;
run)
  printf '%s\n' "$@" > "$here/run-args.txt"
  env > "$here/run-env.txt"
  if [ -f "$here/fail-run" ]; then
    echo "fatal: could not read Username for 'https://github.com': terminal prompts disabled" >&2
    exit 128
  fi
  src=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --mount) src="${2#type=bind,source=}"; src="${src%%,*}"; shift 2 ;;
    *) shift ;;
    esac
  done
  echo "package widgets" > "$src/widget.go"
  exit 0
  ;;
esac
echo "unexpected docker $1" >&2
exit 2
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// beside is the content of a file the fake docker wrote beside itself, and
// "" when it wrote none.
func beside(t *testing.T, docker, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(docker), name))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// touch puts a marker file beside the fake docker.
func touch(t *testing.T, docker, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(docker), name), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTheAnglesRunInACheckoutOfThePullRequestHead(t *testing.T) {
	t.Setenv(CheckoutTokenVar, "the-hosts-own")
	docker := fakeDocker(t)
	local := t.TempDir()
	artifact := filepath.Join(t.TempDir(), "review-7")
	agent := newFakeAngleAgent(len(BuiltinAngles))
	var log bytes.Buffer
	angles := &Angles{Agent: agent, Provider: config.AgentClaude, Dir: local, Checkout: &Checkout{DockerBin: docker, Token: "ghp_secret"}, Log: &log}
	runs, err := angles.Run(context.Background(), artifact, &Project{}, testBrief(), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	// Every session ran in the checkout the container made, not in the
	// checkout the machine had, and recorded it so it can be reopened
	// there.
	want := filepath.Join(artifact, CheckoutDir)
	for _, r := range runs {
		if agent.reqs[r.Angle].Dir != want || r.Dir != want {
			t.Errorf("%s ran in %q and recorded %q, want the checkout %s", r.Angle, agent.reqs[r.Angle].Dir, r.Dir, want)
		}
	}
	if _, err := os.Stat(filepath.Join(want, "widget.go")); err != nil {
		t.Errorf("the clone's files are not in the checkout: %v", err)
	}
	if !strings.Contains(log.String(), "checked out acme/widgets#7 under "+want) {
		t.Errorf("log = %q, want the checkout reported", log.String())
	}
	// The image was looked for, built and run, in that order.
	if got := beside(t, docker, "calls.txt"); got != "image\nbuild\nrun\n" {
		t.Errorf("docker was called %q, want image inspect, build, run", got)
	}
	// Alpine with git, tagged after the Dockerfile.
	dockerfile := beside(t, docker, "Dockerfile.built")
	if !strings.HasPrefix(dockerfile, "FROM alpine:") || !strings.Contains(dockerfile, "apk add --no-cache git") {
		t.Errorf("Dockerfile built:\n%s\nwant Alpine with git", dockerfile)
	}
	if build := beside(t, docker, "build-args.txt"); !strings.Contains(build, "\n--tag\n"+checkoutTag()+"\n") || !strings.HasPrefix(checkoutTag(), checkoutImage+":") {
		t.Errorf("build args:\n%swant --tag %s", build, checkoutTag())
	}
	// The container is removed when it exits, mounts the checkout, runs as
	// this user, and fetches the pull request's head reference from the
	// repository's own HTTPS remote.
	run := beside(t, docker, "run-args.txt")
	source, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{
		"run\n--rm\n",
		"\n--mount\ntype=bind,source=" + source + ",destination=/src\n",
		"\n--workdir\n/src\n",
		"\n--user\n",
		"\n--env\nGIT_TERMINAL_PROMPT=0\n",
		"\n" + checkoutTag() + "\nsh\n-c\n",
		"\nhttps://github.com/acme/widgets.git\nrefs/pull/7/head\n",
		"fetch -q --depth 1 origin \"$2\"",
		"credential.helper=",
	} {
		if !strings.Contains(run, arg) {
			t.Errorf("run args missing %q:\n%s", arg, run)
		}
	}
	// The token is named on the command line and carried in the client's
	// environment, and the host's own value under that name is not what
	// the container gets.
	if !strings.Contains(run, "\n--env\n"+CheckoutTokenVar+"\n") {
		t.Errorf("run args do not name %s:\n%s", CheckoutTokenVar, run)
	}
	if strings.Contains(run, "ghp_secret") {
		t.Errorf("run args carry the token's value:\n%s", run)
	}
	env := beside(t, docker, "run-env.txt")
	if !strings.Contains(env, "\n"+CheckoutTokenVar+"=ghp_secret\n") && !strings.HasPrefix(env, CheckoutTokenVar+"=ghp_secret\n") {
		t.Errorf("the client's environment does not carry the token:\n%s", env)
	}
	if strings.Contains(env, "the-hosts-own") {
		t.Errorf("the client's environment carries the host's %s:\n%s", CheckoutTokenVar, env)
	}
}

func TestWithoutATokenTheCloneIsAnonymous(t *testing.T) {
	t.Setenv(CheckoutTokenVar, "the-hosts-own")
	docker := fakeDocker(t)
	agent := newFakeAngleAgent(1)
	project := projectWith(t, "[angles]\nacceptance_criteria = false\ntest_coverage = false\nside_effects = false\n")
	if _, err := (&Angles{Agent: agent, Checkout: &Checkout{DockerBin: docker}}).Run(context.Background(), t.TempDir(), project, testBrief(), testDiff); err != nil {
		t.Fatal(err)
	}
	run := beside(t, docker, "run-args.txt")
	if strings.Contains(run, "\n--env\n"+CheckoutTokenVar+"\n") {
		t.Errorf("run args name %s with no token configured:\n%s", CheckoutTokenVar, run)
	}
	if env := beside(t, docker, "run-env.txt"); strings.Contains(env, CheckoutTokenVar+"=") {
		t.Errorf("the client's environment carries a %s with no token configured:\n%s", CheckoutTokenVar, env)
	}
}

func TestTheCheckoutImageIsBuiltOnce(t *testing.T) {
	docker := fakeDocker(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(docker), "images.txt"), []byte(checkoutTag()+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), CheckoutDir)
	if err := (&Checkout{DockerBin: docker}).Run(context.Background(), Ref{Repo: testRepo, Number: 7}, dir); err != nil {
		t.Fatal(err)
	}
	if got := beside(t, docker, "calls.txt"); got != "image\nrun\n" {
		t.Errorf("docker was called %q, want no build for an image the engine has", got)
	}
}

func TestWithoutDockerTheAnglesRunInTheMachinesCheckout(t *testing.T) {
	local := t.TempDir()
	artifact := filepath.Join(t.TempDir(), "review-7")
	agent := newFakeAngleAgent(len(BuiltinAngles))
	var log bytes.Buffer
	missing := filepath.Join(t.TempDir(), "docker")
	angles := &Angles{Agent: agent, Dir: local, Checkout: &Checkout{DockerBin: missing}, Log: &log}
	runs, err := angles.Run(context.Background(), artifact, &Project{}, testBrief(), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if agent.reqs[r.Angle].Dir != local || r.Dir != local {
			t.Errorf("%s ran in %q and recorded %q, want the machine's checkout %s", r.Angle, agent.reqs[r.Angle].Dir, r.Dir, local)
		}
	}
	if _, err := os.Stat(filepath.Join(artifact, CheckoutDir)); !os.IsNotExist(err) {
		t.Errorf("a checkout directory was left behind: %v", err)
	}
	for _, want := range []string{"could not check out acme/widgets#7 in a container: " + missing + " is not installed", "the angles run in " + local} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log = %q, want %q", log.String(), want)
		}
	}
}

func TestACheckoutThatFailsLeavesNothingAndTheAnglesRunElsewhere(t *testing.T) {
	for _, marker := range []string{"fail-build", "fail-run"} {
		t.Run(marker, func(t *testing.T) {
			docker := fakeDocker(t)
			touch(t, docker, marker)
			local := t.TempDir()
			artifact := filepath.Join(t.TempDir(), "review-7")
			agent := newFakeAngleAgent(len(BuiltinAngles))
			var log bytes.Buffer
			angles := &Angles{Agent: agent, Dir: local, Checkout: &Checkout{DockerBin: docker}, Log: &log}
			runs, err := angles.Run(context.Background(), artifact, &Project{}, testBrief(), testDiff)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range runs {
				if agent.reqs[r.Angle].Dir != local {
					t.Errorf("%s ran in %q, want the machine's checkout %s", r.Angle, agent.reqs[r.Angle].Dir, local)
				}
			}
			if _, err := os.Stat(filepath.Join(artifact, CheckoutDir)); !os.IsNotExist(err) {
				t.Errorf("a checkout directory was left behind: %v", err)
			}
			// The failure is reported with what docker said.
			want := "not found"
			if marker == "fail-run" {
				want = "terminal prompts disabled"
			}
			if !strings.Contains(log.String(), "could not check out acme/widgets#7 in a container: ") || !strings.Contains(log.String(), want) {
				t.Errorf("log = %q, want the failure and what docker said (%q)", log.String(), want)
			}
		})
	}
}

func TestWithoutDockerOrACheckoutTheAnglesRunInTheScratchDirectory(t *testing.T) {
	artifact := filepath.Join(t.TempDir(), "review-7")
	agent := newFakeAngleAgent(len(BuiltinAngles))
	var log bytes.Buffer
	angles := &Angles{Agent: agent, Checkout: &Checkout{DockerBin: filepath.Join(t.TempDir(), "docker")}, Log: &log}
	runs, err := angles.Run(context.Background(), artifact, &Project{}, testBrief(), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(artifact, ScratchDir)
	for _, r := range runs {
		if agent.reqs[r.Angle].Dir != want || r.Dir != want {
			t.Errorf("%s ran in %q and recorded %q, want the scratch directory %s", r.Angle, agent.reqs[r.Angle].Dir, r.Dir, want)
		}
	}
	if st, err := os.Stat(want); err != nil || !st.IsDir() {
		t.Errorf("the scratch directory is not there: %v", err)
	}
	if !strings.Contains(log.String(), "the angles run in an empty directory") {
		t.Errorf("log = %q, want the scratch directory reported", log.String())
	}
}

func TestACheckoutThatRunsOutOfTimeIsNotOne(t *testing.T) {
	docker := fakeDocker(t)
	script, err := os.ReadFile(docker)
	if err != nil {
		t.Fatal(err)
	}
	// A clone that never ends: the fake sleeps in place of the container,
	// and dies of the termination signal the client would forward to it.
	slow := strings.Replace(string(script), "run)\n", "run)\n  sleep 30 & pid=$!\n  trap 'kill $pid; exit 143' TERM\n  wait $pid\n", 1)
	if err := os.WriteFile(docker, []byte(slow), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), CheckoutDir)
	c := &Checkout{DockerBin: docker, Timeout: 200 * time.Millisecond}
	err = c.Run(context.Background(), Ref{Repo: testRepo, Number: 7}, dir)
	if err == nil || !strings.Contains(err.Error(), "ran out of time") {
		t.Fatalf("err = %v, want the checkout to have run out of time", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("a checkout directory was left behind: %v", err)
	}
}

func TestNewAnglesAttemptsTheCheckoutWithTheConfiguredToken(t *testing.T) {
	t.Setenv("REVIEW_GH_TOKEN", "ghp_secret")
	cfg, err := ParseConfig("[github]\ntoken = \"$REVIEW_GH_TOKEN\"\n", filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	a := NewAngles(cfg, "/checkout")
	if a.Checkout == nil || a.Checkout.Token != "ghp_secret" || a.Checkout.DockerBin != "" {
		t.Errorf("checkout = %+v, want one attempted with the configured token and the engine's own client", a.Checkout)
	}
	cfg, err = ParseConfig("", filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	a = NewAngles(cfg, "")
	if a.Checkout == nil || a.Checkout.Token != "" {
		t.Errorf("checkout = %+v, want one attempted anonymously without a token", a.Checkout)
	}
}

func TestWithoutACheckoutRunnerTheAnglesRunWhereTheyDid(t *testing.T) {
	// Checkout nil attempts nothing: no docker is looked for.
	local := t.TempDir()
	agent := newFakeAngleAgent(len(BuiltinAngles))
	var log bytes.Buffer
	runs, err := (&Angles{Agent: agent, Dir: local, Log: &log}).Run(context.Background(), t.TempDir(), &Project{}, testBrief(), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.Dir != local {
			t.Errorf("%s ran in %q, want %s", r.Angle, r.Dir, local)
		}
	}
	if log.Len() != 0 {
		t.Errorf("log = %q, want nothing said about a checkout nobody attempted", log.String())
	}
}
