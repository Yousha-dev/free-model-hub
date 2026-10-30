// This file provides the embedded console page.
//
// The page is pure static HTML (containing no accounts, quotas, or credentials), fetching data on demand via management APIs;
// Dynamic data is rendered uniformly via textContent to prevent injecting upstream content as HTML.
//
// The page is extracted from the Go source code to be stored separately: tens of thousands of lines of HTML/JS strings in .go files
// cannot be highlighted/formatted by editors and make the code files unreadable.

package admin

import (
	_ "embed"
	"strings"
)

//go:embed console.html
var page string

// Page returns the console page (injecting the base path of the management API).
func Page(basePath string) string {
	return strings.NewReplacer("__BASE__", basePath).Replace(page)
}
