package review

import (
	"context"

	core "github.com/kpenfound/busybees/core/review"
)

// DistillerName is the name of the distiller session, in an error message.
const DistillerName = core.DistillerName

// Distiller runs the first session of a review: it reads the context bundle
// and writes the brief every session after it starts from.
//
// It is the session that turns a gather into a review. The bundle is raw and
// as large as the pull request made it; the brief is short, angle-agnostic
// and the same for every angle, so a change in what the angles are told
// starts here rather than in five prompts.
type Distiller struct {
	// Agent runs the session.
	Agent Agent
	// Dir is the checkout of the repository under review, which the
	// session's read-only tools can reach. It is "" on a machine that has
	// none (see CheckoutOf), and the session then runs in an empty
	// directory of its own: a review must not read whichever repository the
	// command happened to be run in.
	Dir string
}

// NewDistiller is the distiller a review runs, as the global configuration
// says: the profile cfg's brief_profile names, or else cfg's provider with
// its brief_model or else its model, and dir the checkout Open was given.
func NewDistiller(cfg *Config, dir string) *Distiller {
	if cfg != nil && cfg.BriefProfile != "" {
		return &Distiller{Agent: cfg.profileAgent(cfg.BriefProfile), Dir: dir}
	}
	agent := NewAgent(cfg)
	if cfg != nil && cfg.BriefModel != "" {
		agent.Model = cfg.BriefModel
	}
	return &Distiller{Agent: agent, Dir: dir}
}

// Distill projects acquired context onto the neutral pipeline bundle.
func (d *Distiller) Distill(ctx context.Context, b *Bundle) (*Brief, error) {
	return (&core.Distiller[Ref]{Agent: d.Agent, Dir: d.Dir}).Distill(ctx, b.core())
}

var jsonObject = core.JSONObject
