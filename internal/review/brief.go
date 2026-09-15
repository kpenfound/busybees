package review

import core "github.com/kpenfound/busybees/core/review"

// Brief retains the existing reference JSON while core owns its content.
type Brief = core.Brief[Ref]

var WriteBrief = core.WriteBrief[Ref]
var ReadBrief = core.ReadBrief[Ref]

type Point = core.Point
type TouchedArea = core.TouchedArea

const BriefFile = core.BriefFile

var Sizes = core.Sizes
