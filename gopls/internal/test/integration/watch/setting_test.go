// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package watch

import (
	"fmt"
	"testing"

	. "golang.org/x/tools/gopls/internal/test/integration"
)

func TestSubdirWatchPatterns(t *testing.T) {
	const files = `
-- go.mod --
module mod.test

go 1.18
-- subdir/subdir.go --
package subdir
`

	tests := []struct {
		clientName          string
		subdirWatchPatterns string
		wantWatched         bool
	}{
		{"other client", "on", true},
		{"other client", "off", false},
		{"other client", "auto", false},
		{"Visual Studio Code", "auto", true},
	}

	for _, test := range tests {
		t.Run(fmt.Sprintf("%s_%s", test.clientName, test.subdirWatchPatterns), func(t *testing.T) {
			WithOptions(
				ClientName(test.clientName),
				Settings{
					"subdirWatchPatterns": test.subdirWatchPatterns,
				},
			).Run(t, files, func(t *testing.T, env *Env) {
				var expectation Expectation
				if test.wantWatched {
					expectation = FileWatchMatching("subdir")
				} else {
					expectation = NoFileWatchMatching("subdir")
				}
				env.OnceMet(
					InitialWorkspaceLoad,
					expectation,
				)
			})
		})
	}
}

// TestInternalFileWatcher verifies that when the internalFileWatcher setting
// is enabled, the server detects file changes on disk using its own fsnotify
// watcher instead of relying on the client's didChangeWatchedFiles
// notifications. Since no client-side watch patterns are registered, the fake
// editor's didChangeWatchedFiles notification carries no matching events. The
// only way for the server to learn about the file change is through the
// internal watcher.
func TestInternalFileWatcher(t *testing.T) {
	const files = `
-- go.mod --
module mod.test

go 1.18
-- a/a.go --
package a
`
	WithOptions(
		Settings{
			"internalFileWatcher": true,
		},
	).Run(t, files, func(t *testing.T, env *Env) {
		env.OnceMet(InitialWorkspaceLoad)

		// Write a new file with an unused variable diagnostic.
		// The fake editor sends an empty didChangeWatchedFiles to the server
		// (no client-side watch patterns match), so the server can only learn
		// about this file through the internal fsnotify watcher.
		env.WriteWorkspaceFile("a/b.go", "package a\n\nfunc _() {\n\tx := 1\n}\n")
		env.Await(Diagnostics(env.AtRegexp("a/b.go", "x")))

		// Fix the diagnostic and verify it clears.
		env.WriteWorkspaceFile("a/b.go", "package a\n\nfunc _() {\n}\n")
		env.Await(NoDiagnostics(ForFile("a/b.go")))
	})
}

// This test checks that we surface errors for invalid subdir watch patterns,
// as the triple of ("off"|"on"|"auto") may be confusing to users inclined to
// use (true|false) or some other truthy value.
func TestSubdirWatchPatterns_BadValues(t *testing.T) {
	tests := []struct {
		badValue    any
		wantMessage string
	}{
		{true, "invalid type bool (want string)"},
		{false, "invalid type bool (want string)"},
		{"yes", `invalid option "yes"`},
	}

	for _, test := range tests {
		t.Run(fmt.Sprint(test.badValue), func(t *testing.T) {
			WithOptions(
				Settings{
					"subdirWatchPatterns": test.badValue,
				},
			).Run(t, "", func(t *testing.T, env *Env) {
				env.OnceMet(
					InitialWorkspaceLoad,
					ShownMessage(test.wantMessage),
				)
			})
		})
	}
}
