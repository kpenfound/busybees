package review

import core "github.com/kpenfound/busybees/core/review"

type Finding = core.Finding
type Findings = core.Findings
type LineRange = core.LineRange

const FindingsFile = core.FindingsFile
const SideNew = core.SideNew
const SideOld = core.SideOld

var ParseFindings = core.ParseFindings
var WriteFindings = core.WriteFindings
var ReadFindings = core.ReadFindings
