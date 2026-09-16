package ios

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeScriptPhaseFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScriptPhaseUpToDateRules(t *testing.T) {
	root := t.TempDir()
	input := writeScriptPhaseFile(t, filepath.Join(root, "in.txt"), "in")
	output := writeScriptPhaseFile(t, filepath.Join(root, "out.txt"), "out")
	state := filepath.Join(root, "phase.state")

	base := ScriptPhaseIO{
		Name:      "Phase",
		Script:    "echo hi",
		ShellPath: "/bin/sh",
		Inputs:    []string{input},
		Outputs:   []string{output},
		StateFile: state,
	}

	// No recorded state yet.
	upToDate, err := ScriptPhaseUpToDate(base)
	if err != nil {
		t.Fatal(err)
	}
	if upToDate {
		t.Fatal("phase without recorded state must run")
	}

	if err := RecordScriptPhaseState(base); err != nil {
		t.Fatal(err)
	}
	upToDate, err = ScriptPhaseUpToDate(base)
	if err != nil {
		t.Fatal(err)
	}
	if !upToDate {
		t.Fatal("phase with fresh outputs and recorded state should be skipped")
	}

	// A phase declaring no outputs can never be proven up to date.
	noOutputs := base
	noOutputs.Outputs = nil
	if err := RecordScriptPhaseState(noOutputs); err != nil {
		t.Fatal(err)
	}
	upToDate, err = ScriptPhaseUpToDate(noOutputs)
	if err != nil {
		t.Fatal(err)
	}
	if upToDate {
		t.Fatal("phase without declared outputs must always run")
	}

	// alwaysOutOfDate wins over everything.
	always := base
	always.AlwaysOutOfDate = true
	if err := RecordScriptPhaseState(always); err != nil {
		t.Fatal(err)
	}
	upToDate, err = ScriptPhaseUpToDate(always)
	if err != nil {
		t.Fatal(err)
	}
	if upToDate {
		t.Fatal("alwaysOutOfDate phase must always run")
	}

	// A newer input invalidates the phase.
	if err := RecordScriptPhaseState(base); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(input, future, future); err != nil {
		t.Fatal(err)
	}
	upToDate, err = ScriptPhaseUpToDate(base)
	if err != nil {
		t.Fatal(err)
	}
	if upToDate {
		t.Fatal("phase with a newer input must run")
	}

	// A missing output invalidates the phase.
	if err := os.Chtimes(input, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := RecordScriptPhaseState(base); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(output); err != nil {
		t.Fatal(err)
	}
	upToDate, err = ScriptPhaseUpToDate(base)
	if err != nil {
		t.Fatal(err)
	}
	if upToDate {
		t.Fatal("phase with a missing output must run")
	}
}

func TestScriptPhaseSignatureTracksDeclaredGraph(t *testing.T) {
	root := t.TempDir()
	output := writeScriptPhaseFile(t, filepath.Join(root, "out.txt"), "out")
	base := ScriptPhaseIO{
		Name:      "Phase",
		Script:    "echo hi",
		ShellPath: "/bin/sh",
		Outputs:   []string{output},
		StateFile: filepath.Join(root, "phase.state"),
	}
	if err := RecordScriptPhaseState(base); err != nil {
		t.Fatal(err)
	}
	for _, mutated := range []ScriptPhaseIO{
		func() ScriptPhaseIO { c := base; c.Script = "echo other"; return c }(),
		func() ScriptPhaseIO { c := base; c.ShellPath = "/bin/bash"; return c }(),
		func() ScriptPhaseIO { c := base; c.Inputs = []string{"/tmp/new"}; return c }(),
		func() ScriptPhaseIO { c := base; c.DependencyFile = "/tmp/phase.d"; return c }(),
	} {
		upToDate, err := ScriptPhaseUpToDate(mutated)
		if err != nil {
			t.Fatal(err)
		}
		if upToDate {
			t.Fatalf("signature ignored a declared-graph change: %+v", mutated)
		}
	}
	// Input order must not matter.
	ordered := base
	ordered.Inputs = []string{"/tmp/a", "/tmp/b"}
	if err := RecordScriptPhaseState(ordered); err != nil {
		t.Fatal(err)
	}
	reordered := base
	reordered.Inputs = []string{"/tmp/b", "/tmp/a"}
	if reordered.signature() != ordered.signature() {
		t.Fatal("signature must not depend on declared path ordering")
	}
}

func TestParseMakefileDependencies(t *testing.T) {
	root := t.TempDir()
	path := writeScriptPhaseFile(t, filepath.Join(root, "phase.d"), strings.Join([]string{
		"# a comment",
		"/build/out.h: /src/a.h \\",
		"  /src/with\\ space.h /src/b.h",
		"/build/other.h: /src/c.h",
		"/src/a.h:",
		"",
	}, "\n"))
	deps, err := parseMakefileDependencies(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/src/a.h", "/src/with space.h", "/src/b.h", "/src/c.h"}
	if len(deps) != len(want) {
		t.Fatalf("got %#v, want %#v", deps, want)
	}
	for i, value := range want {
		if deps[i] != value {
			t.Fatalf("dependency %d = %q, want %q (all: %#v)", i, deps[i], value, deps)
		}
	}
}

func TestMissingMacOSToolsFindsRealInvocations(t *testing.T) {
	root := t.TempDir()
	// An absolute tool path is resolved on disk rather than through PATH, so use
	// a path that cannot exist on either macOS or the Linux build host.
	plistBuddy := filepath.Join(root, "absent", "usr", "libexec", "PlistBuddy")
	script := `set -eu
if command -v xcrun >/dev/null 2>&1; then
  echo have-xcrun
fi
LOG=/tmp/log actool --compile "$OUT" Assets.xcassets > "$LOG"
` + plistBuddy + ` -c "Print" "$PLIST"
mkdir -p "$(dirname "$OUT")"
printf '%s\n' done | tee "$OUT"
`
	// Compare tool names only; the human-readable descriptions contain ordinary
	// words ("asset", "list") that would make substring assertions meaningless.
	// ("asset" literally contains "set".)
	reported := make(map[string]bool)
	for _, entry := range MissingMacOSTools(script, root) {
		name := entry
		if open := strings.IndexByte(entry, '('); open > 0 {
			name = strings.TrimSpace(entry[:open])
		}
		reported[name] = true
	}
	for _, want := range []string{"actool", "PlistBuddy"} {
		if !reported[want] {
			t.Fatalf("expected %s in %v", want, reported)
		}
	}
	// `command -v xcrun` only probes for the tool, so it must not be reported.
	if reported["xcrun"] {
		t.Fatalf("existence probe was reported as a required tool: %v", reported)
	}
	// Non-macOS commands must never be reported.
	for _, unwanted := range []string{"mkdir", "printf", "dirname", "echo", "tee", "set", "if", "LOG"} {
		if reported[unwanted] {
			t.Fatalf("portable command %s reported as macOS-only: %v", unwanted, reported)
		}
	}
}

func TestMissingMacOSToolsRespectsSearchPath(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "lipo"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "lipo -info \"$BINARY\"\notool -L \"$BINARY\"\n"
	missing := MissingMacOSTools(script, binDir)
	joined := strings.Join(missing, ", ")
	if strings.Contains(joined, "lipo") {
		t.Fatalf("tool present on the phase PATH was reported missing: %q", joined)
	}
	if !strings.Contains(joined, "otool") {
		t.Fatalf("expected otool to be reported: %q", joined)
	}
}

func TestAnnotateScriptPhaseErrorOnlyAddsRealDiagnostics(t *testing.T) {
	root := t.TempDir()
	base := os.ErrDeadlineExceeded
	if got := AnnotateScriptPhaseError(nil, "actool", root); got != nil {
		t.Fatalf("nil error must stay nil, got %v", got)
	}
	plain := AnnotateScriptPhaseError(base, "printf hello\n", root)
	if plain.Error() != base.Error() {
		t.Fatalf("portable script must not gain a diagnostic: %v", plain)
	}
	annotated := AnnotateScriptPhaseError(base, "actool --compile out\n", root)
	if !strings.Contains(annotated.Error(), "requires unavailable macOS tools") {
		t.Fatalf("missing diagnostic: %v", annotated)
	}
	if !strings.Contains(annotated.Error(), base.Error()) {
		t.Fatalf("annotation dropped the underlying error: %v", annotated)
	}
}

func TestReadScriptFileListExpandsEntries(t *testing.T) {
	root := t.TempDir()
	path := writeScriptPhaseFile(t, filepath.Join(root, "inputs.xcfilelist"), strings.Join([]string{
		"# leading comment",
		"$(SRCROOT)/a.h",
		"",
		"${SRCROOT}/b.h",
	}, "\n"))
	values := map[string]string{"SRCROOT": "/src"}
	entries, err := ReadScriptFileList(path, func(value string) string {
		return expandXcodeVariables(value, values)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/src/a.h", "/src/b.h"}
	if len(entries) != len(want) {
		t.Fatalf("got %#v, want %#v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, entries[i], want[i])
		}
	}
}
