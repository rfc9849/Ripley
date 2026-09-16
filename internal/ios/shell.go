package ios

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// ResolveScriptShell returns the interpreter that should actually run a build
// phase's script.
//
// Xcode declares most run-script phases with `/bin/sh`, but on macOS `/bin/sh`
// IS bash running in sh mode, so it accepts arrays, `[[ ]]`, `PIPESTATUS` and
// `function name() ( ... )`. On Linux `/bin/sh` is usually dash, which rejects
// all of those. Flutter's own `xcode_backend.sh` is a concrete example: it
// declares `function follow_links() (` and dies under dash with
// `Syntax error: "(" unexpected`.
//
// Mapping an `sh` request onto bash is therefore the faithful reproduction of
// Xcode's environment, not a workaround. Any other declared interpreter
// (`/usr/bin/ruby`, `/usr/bin/python3`, ...) is honoured exactly, falling back
// to a PATH lookup when the macOS absolute path does not exist on Linux.
func ResolveScriptShell(shellPath string) (string, error) {
	if strings.TrimSpace(shellPath) == "" {
		shellPath = "/bin/sh"
	}
	if filepath.Base(shellPath) == "sh" && !shellIsBash(shellPath) {
		if bash, err := exec.LookPath("bash"); err == nil {
			return bash, nil
		}
	}
	if st, err := os.Stat(shellPath); err == nil && !st.IsDir() {
		return shellPath, nil
	}
	resolved, err := exec.LookPath(filepath.Base(shellPath))
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// shellIsBash reports whether an `sh` path is really bash, in which case it
// already provides Xcode's semantics and must not be substituted.
func shellIsBash(shellPath string) bool {
	resolved, err := filepath.EvalSymlinks(shellPath)
	if err != nil {
		return false
	}
	return strings.Contains(filepath.Base(resolved), "bash")
}

// flutterBackendRE matches an invocation of Flutter's xcode_backend.sh, whose
// `build` step ripley performs itself (kernel, AOT, assets) and whose
// `embed_and_thin` step ripley performs during assembly.
var flutterBackendRE = regexp.MustCompile(`xcode_backend\.sh`)

// scriptFileRefRE matches a script phase that delegates to another script in
// the project, e.g. `/bin/bash "$SRCROOT/scripts/xcode_flutter_build.sh"`.
var scriptFileRefRE = regexp.MustCompile(`\$[{(]?SRCROOT[)}]?/([A-Za-z0-9_./-]+\.sh)`)

// ScriptInvokesFlutterBackend reports whether a build phase ultimately runs
// Flutter's xcode_backend.sh, following one level of project wrapper script.
//
// Matching only the phase's own text is not enough: immich's "Run Script" phase
// body is `/bin/bash "$SRCROOT/scripts/xcode_flutter_build.sh"`, and that
// wrapper is what calls xcode_backend.sh. Missing the indirection makes ripley run a
// second, redundant Flutter build inside the phase.
//
// wantEmbed selects which half of the backend is being looked for: the
// `embed_and_thin` step rather than the `build` step.
func ScriptInvokesFlutterBackend(script, srcRoot string, wantEmbed bool) bool {
	if matchesFlutterBackend(script, wantEmbed) {
		return true
	}
	if srcRoot == "" {
		return false
	}
	for _, match := range scriptFileRefRE.FindAllStringSubmatch(script, -1) {
		path := filepath.Join(srcRoot, filepath.FromSlash(match[1]))
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if matchesFlutterBackend(string(data), wantEmbed) {
			return true
		}
	}
	return false
}

func matchesFlutterBackend(script string, wantEmbed bool) bool {
	if !flutterBackendRE.MatchString(script) {
		return false
	}
	embeds := strings.Contains(script, "embed_and_thin") || strings.Contains(script, "embed ")
	return embeds == wantEmbed
}
