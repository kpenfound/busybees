package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/prompts"
)

// A provider with no repository holds a real directory until Release. This
// checks cleanup through the interface, including after a context is cancelled.
type directoryProvider struct {
	WorkspaceProvider
	dir      string
	cancel   context.CancelFunc
	released bool
}

func (p *directoryProvider) Fetch(context.Context) error { return nil }
func (p *directoryProvider) Acquire(context.Context, vcs.Request) (vcs.Workspace, error) {
	if p.cancel != nil {
		p.cancel()
	}
	return vcs.Directory(p.dir), nil
}
func (p *directoryProvider) Release(ctx context.Context, ws vcs.Workspace) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ws.Directory() != p.dir {
		return errors.New("released a different workspace")
	}
	p.released = true
	return os.RemoveAll(p.dir)
}
func TestSingletonReleasesWorkspaceContract(t *testing.T) {
	for _, mode := range []string{"success", "error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, baseTOML)
			p := &directoryProvider{dir: filepath.Join(t.TempDir(), "held")}
			if err := os.Mkdir(p.dir, 0700); err != nil {
				t.Fatal(err)
			}
			h.sched.ws = p
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				p.cancel = cancel
			}
			if mode == "error" {
				h.sched.runner.ClaudeBin = filepath.Join(t.TempDir(), "missing-agent")
			}
			err := h.sched.runSingleton(ctx, config.RoleQA, prompts.Data{})
			if mode == "success" && err != nil {
				t.Fatal(err)
			}
			if mode != "success" && err == nil {
				t.Fatal("expected session failure")
			}
			if !p.released {
				t.Fatal("workspace was not released with a usable context")
			}
			if _, err := os.Stat(p.dir); !os.IsNotExist(err) {
				t.Fatalf("workspace leaked: %v", err)
			}
		})
	}
}
