package views

import "embed"

// Assets holds the embedded GUI static files (CSS, JS), served under /assets/.
//
//go:embed assets
var Assets embed.FS
