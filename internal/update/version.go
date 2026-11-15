// Package update tells whether a newer stable pcb release exists. It never
// downloads nor installs anything: it reads where GitHub's "latest release"
// page redirects, compares that tag with the installed version, keeps the
// answer for a day in update.json, and says how this install is updated.
package update

import (
	"regexp"
	"strconv"
)

// version is vMAJOR.MINOR.PATCH with an optional -PRERELEASE of dot-separated
// identifiers; a git describe suffix (-N-gSHA) or -dirty is refused apart.
var version = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

var describe = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+(?:-dirty)?$|-dirty$`)

// stable is vMAJOR.MINOR.PATCH alone.
var stable = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Checkable reports whether installed is a released version (vX.Y.Z or
// vX.Y.Z-PRERELEASE): a dev build (dev, a git describe, -dirty) is never
// checked.
func Checkable(installed string) bool {
	return version.MatchString(installed) && !describe.MatchString(installed)
}

// Newer reports whether latest, a stable version, is above installed. A
// pre-release sits below its own release (v0.1.0-rc.1 < v0.1.0); anything
// that is not a checkable installed version or a stable latest is false.
func Newer(installed, latest string) bool {
	if !Checkable(installed) || !stable.MatchString(latest) {
		return false
	}
	i, l := version.FindStringSubmatch(installed), stable.FindStringSubmatch(latest)
	for k := 1; k <= 3; k++ {
		a, _ := strconv.Atoi(i[k])
		b, _ := strconv.Atoi(l[k])
		if a != b {
			return b > a
		}
	}
	return i[4] != "" // same X.Y.Z: newer only if installed is its pre-release
}
