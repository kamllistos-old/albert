// Package albert holds the files embedded into the binary: the GUI (web/) and
// the container image recipe (docker/). It has no dependencies.
package albert

import "embed"

// Files contains the web/ and docker/ trees.
//
//go:embed web docker
var Files embed.FS
