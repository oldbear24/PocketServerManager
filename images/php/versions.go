// Package php holds the manifest of the PHP runtime images, versions.json: the
// PHP versions an image is built for, with the dates their support ends, and
// the Debian release of the official images they start from. It is the one
// list for CI, which builds an image per version while the version has
// security support (.github/workflows/php.yml), and for the panel, which warns
// before support ends (internal/php).
package php

import _ "embed"

// Manifest is versions.json.
//
//go:embed versions.json
var Manifest []byte
