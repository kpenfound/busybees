package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kpenfound/busybees/internal/config"
)

// A container session whose role sets container_use_environment
// (config.ResolvedRole.ContainerUseEnvironment) runs in an image bees builds
// from a dagger/container-use environment definition instead of one a
// person built and named in sandbox_image. Only the definition's file
// format is shared with container-use: bees does not run its Go package,
// the Dagger SDK, a Dagger engine or its MCP server, none of which it needs
// to turn four fields into a Dockerfile. The file is
// <container_use_environment>/.container-use/environment.json, read from
// the session's worktree, and of its fields these are used:
//
//   - base_image: the FROM line.
//   - env: KEY=VALUE entries, one ENV line each, set before any command
//     runs, as container-use sets them.
//   - setup_commands, then install_commands: one RUN each, through
//     `sh -c`, at build time. container-use runs setup_commands, copies its
//     environment's source tree in, then runs install_commands, so an
//     install command there can read a checked-in file (package.json). Bees
//     has no source copy to put between the two lists: the worktree is
//     bind-mounted when the container runs (container.mounts), not baked
//     into the image, so the lists run back to back and an install command
//     that needs the source must tolerate its absence or move out of the
//     definition. That difference is deliberate, not an oversight.
//
// The rest of the schema is read and ignored, without an error:
//
//   - workdir: the container always runs in the worktree, at its host path
//     (container.command's --workdir), which the definition cannot move.
//   - secrets: container-use resolves them through Dagger's secret
//     providers (env://, file://, op://) in its pipeline; bees has no
//     pipeline to resolve them in, and the sandbox already forwards the
//     credentials a session needs (containerVars, config.AgentCredentials).
//   - services: other containers; the sandbox is one container.
//
// The image is tagged after a hash of the Dockerfile, so a definition
// whose resolved inputs are unchanged reuses the image built for it and a
// change to any of them builds a new one. It must hold the agent, git and
// gh like a sandbox_image, which is the definition's own job, and `docker
// build` pulls base_image when it is not on the machine. A definition that
// is missing, malformed or fails to build fails the session, not the
// configuration: it is a file of the project, and its branch may change it.

// containerUseDir and containerUseFile are where container-use keeps a
// definition under the directory container_use_environment names.
const (
	containerUseDir  = ".container-use"
	containerUseFile = "environment.json"
)

// containerUseRepo is the repository the built images belong to; the tag
// is the hash of what they were built from.
const containerUseRepo = "bees-container-use"

// containerUseBuildLog is the file in the session directory that holds the
// build's output.
const containerUseBuildLog = "docker-build.log"

// containerUseEnvironment is the part of container-use's EnvironmentConfig
// bees builds from. The fields it leaves out (workdir, secrets, services)
// are dropped by the decoder, which is what ignoring them means here.
type containerUseEnvironment struct {
	BaseImage       string   `json:"base_image"`
	SetupCommands   []string `json:"setup_commands"`
	InstallCommands []string `json:"install_commands"`
	Env             []string `json:"env"`
}

// containerUseImage is the image a session with container_use_environment
// runs in: it reads the role's definition from the worktree, renders it to a
// Dockerfile, and builds it unless the image its hash names is already on
// the machine. The Dockerfile is kept in the session directory either way,
// for people reading it afterwards, and a build's output goes to
// containerUseBuildLog beside it.
func (r *Runner) containerUseImage(ctx context.Context, req Request, sessionDir string) (string, error) {
	path := filepath.Join(req.WorkDir, req.Role.ContainerUseEnvironment, containerUseDir, containerUseFile)
	env, err := loadContainerUseEnvironment(path)
	if err != nil {
		return "", fmt.Errorf("container_use_environment %q: %w", req.Role.ContainerUseEnvironment, err)
	}
	dockerfile, err := containerUseDockerfile(env)
	if err != nil {
		return "", fmt.Errorf("container_use_environment %q: %s: %w", req.Role.ContainerUseEnvironment, path, err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return "", err
	}
	tag := containerUseTag(dockerfile)
	docker := r.dockerBin()
	if exec.CommandContext(ctx, docker, "image", "inspect", "--format", "{{.Id}}", tag).Run() == nil {
		return tag, nil
	}
	// The build context is a directory of its own holding the Dockerfile
	// alone: the session directory has the prompts in it, and everything
	// in a context is sent to the engine.
	buildDir, err := os.MkdirTemp("", "bees-container-use-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(buildDir) }()
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return "", err
	}
	logPath := filepath.Join(sessionDir, containerUseBuildLog)
	log, err := os.Create(logPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = log.Close() }()
	cmd := exec.CommandContext(ctx, docker, "build", "--tag", tag, "--file", filepath.Join(buildDir, "Dockerfile"), buildDir)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("container_use_environment %q: %s build --tag %s failed building %s: %w; see %s", req.Role.ContainerUseEnvironment, config.ContainerEngine, tag, path, err, logPath)
	}
	return tag, nil
}

// loadContainerUseEnvironment reads one definition. A definition that is
// missing, that is not JSON, or that names no base image is an error naming
// the file and what is wrong with it.
func loadContainerUseEnvironment(path string) (containerUseEnvironment, error) {
	var env containerUseEnvironment
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return env, fmt.Errorf("%s does not exist: the directory container_use_environment names must hold %s/%s", path, containerUseDir, containerUseFile)
	}
	if err != nil {
		return env, err
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return env, fmt.Errorf("%s is not a container-use environment definition: %w", path, err)
	}
	if env.BaseImage == "" {
		return env, fmt.Errorf("%s names no base_image: the image the definition builds on, which must hold the agent, git and gh", path)
	}
	if strings.ContainsAny(env.BaseImage, "\n\r") {
		return env, fmt.Errorf("%s names a base_image that spans more than one line", path)
	}
	return env, nil
}

// containerUseDockerfile renders a definition: FROM, the env, then one RUN
// per command in setup order then install order (see the package comment
// for why nothing separates them). A command runs through `sh -c` in exec
// form, so a command with quotes or newlines in it is passed whole.
func containerUseDockerfile(env containerUseEnvironment) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "FROM %s\n", env.BaseImage)
	for _, kv := range env.Env {
		key, value, _ := strings.Cut(kv, "=")
		if key == "" {
			return "", fmt.Errorf("env entry %q is not KEY=VALUE", kv)
		}
		if strings.ContainsAny(kv, "\n\r") {
			return "", fmt.Errorf("env entry %q spans more than one line", kv)
		}
		fmt.Fprintf(&b, "ENV %s=\"%s\"\n", key, strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value))
	}
	for _, list := range [][]string{env.SetupCommands, env.InstallCommands} {
		for _, command := range list {
			var line bytes.Buffer
			enc := json.NewEncoder(&line)
			enc.SetEscapeHTML(false)
			if err := enc.Encode([]string{"sh", "-c", command}); err != nil {
				return "", err
			}
			b.WriteString("RUN " + line.String()) // Encode ends the line
		}
	}
	return b.String(), nil
}

// containerUseTag names the image a Dockerfile builds: the repository and a
// prefix of the hash of the Dockerfile's text. The Dockerfile holds exactly
// the fields bees builds from, so a change to the definition's formatting
// or to a field bees ignores leaves the tag alone, and a change to any of
// base_image, env, setup_commands or install_commands changes it.
func containerUseTag(dockerfile string) string {
	sum := sha256.Sum256([]byte(dockerfile))
	return containerUseRepo + ":" + hex.EncodeToString(sum[:8])
}
