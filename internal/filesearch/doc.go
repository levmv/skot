// Package filesearch provides rooted file enumeration and text search.
//
// Paths accepted and returned by the package are relative to the search root.
// Recursive traversal does not follow symbolic links and always excludes Git
// metadata directories. Repository ignore rules are local to the search root;
// parent and user-global ignore files are never read. Ignore rules and direct
// globs are matched bytewise and case-sensitively; Git's core.ignoreCase
// setting is not consulted. Brace expansion is bounded to 128 alternatives
// per pattern. A search regexp, direct glob, or ignore-file line is rejected
// if it exceeds 65,535 bytes. An ignore file is also rejected if it exceeds
// 8 MiB or expands beyond 65,536 patterns or 1 MiB of pattern text.
package filesearch
