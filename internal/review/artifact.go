package review

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	core "github.com/kpenfound/busybees/core/review"
)

// The review artifact. Every review is a directory that holds what the
// review came to and what reopens it, so a review can be triaged after the
// sessions have ended, on another day, and an angle asked a question about
// what it found. The directories live under the storage path of the global
// configuration (Config.ResolvedStoragePath), one per review:
//
//	<storage>/<owner>/<name>/<number>/<started>/   ArtifactDir: one review of
//	                                               one pull request, started
//	                                               at <started> (UTC,
//	                                               20060102-150405)
//	  brief.json                                   the brief (brief.go): what
//	                                               the distiller made of the
//	                                               context, and its session id
//	  angles/<angle>.json                          one file per angle that ran
//	                                               (angles.go): its session
//	                                               id, the directory it ran
//	                                               in and its raw answer,
//	                                               enough to resume it
//	  checkout/                                    the pull request's head,
//	                                               cloned by a container
//	                                               (checkout.go) for the
//	                                               angles to run in
//	  scratch/                                     where the angles ran when
//	                                               there was no checkout,
//	                                               that one included
//	  findings.json                                the judge's list
//	                                               (findings.go, judge.go),
//	                                               with what the reviewer
//	                                               notes hid from it
//	                                               (noise.go)
//	  triage.json                                  what triage decided about
//	                                               each finding (triage.go),
//	                                               empty until it has
//
// Whichever of checkout/ or scratch/ the angles ran in also holds
// diff.patch, the pull request's diff, when it was gathered: core/review's
// writeDiff writes it once there, inside what the angle sessions' read-only
// tools can reach, so every angle's prompt can point at the one file
// instead of repeating the diff's text. It is not written, and the prompt
// falls back to the same message it gets when there was no diff at all,
// when the angles ran in the machine's own checkout instead (Angles.Dir): a
// review does not write into a working tree it did not make.
//
// The files are written one at a time as the review goes, by WriteBrief,
// Angles.Run, WriteFindings and WriteTriage, so a review that stopped after
// the angles has a brief and angle runs and nothing else; ReadArtifact reads
// whichever are there. LatestArtifactDir finds the newest review of a pull
// request, which is the one a resumed review reopens.

// TriageFile is the name the triage state is written under inside a
// review's artifact directory.
const TriageFile = core.TriageFile

// artifactStamp is the layout of the directory that names when a review
// started: sortable, and safe on every filesystem.
const artifactStamp = "20060102-150405"

// ArtifactDir is the directory of a review of ref started at started,
// under storage, the global configuration's resolved storage path. Nothing
// is created.
func ArtifactDir(storage string, ref Ref, started time.Time) string {
	return filepath.Join(pullRequestDir(storage, ref), started.UTC().Format(artifactStamp))
}

// LatestArtifactDir is the directory of the newest review of ref under
// storage, and an error when there has been none.
func LatestArtifactDir(storage string, ref Ref) (string, error) {
	dir := pullRequestDir(storage, ref)
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no review of %s under %s", ref, storage)
	}
	// ReadDir lists by name, and the names are timestamps.
	return filepath.Join(dir, names[len(names)-1]), nil
}

// pullRequestDir is the directory the reviews of one pull request are
// under: the repository's owner and name, then the number.
func pullRequestDir(storage string, ref Ref) string {
	return filepath.Join(storage, filepath.FromSlash(ref.Repo), strconv.Itoa(ref.Number))
}

// Artifact keeps the legacy reference shape for triage and verification.
type Artifact = core.Artifact[Ref]
type Triage = core.Triage
type Decision = core.Decision

var ReadArtifact = core.ReadArtifact[Ref]
var WriteTriage = core.WriteTriage
var ReadTriage = core.ReadTriage
