package ios

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ripley/internal/toolchain"
)

func scriptTestToolchain(t *testing.T) toolchain.Toolchain {
	t.Helper()
	root := t.TempDir()
	sdk := filepath.Join(root, "iPhoneOS26.5.sdk")
	if err := os.MkdirAll(sdk, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdk, "SDKSettings.json"), []byte(`{"Version":"26.5"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	swift := filepath.Join(root, "swift")
	if err := os.MkdirAll(filepath.Join(swift, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIPLEY_IOS_SDK", sdk)
	t.Setenv("RIPLEY_SWIFT", swift)
	return toolchain.Toolchain{Root: filepath.Join(root, "ripley-toolchain")}
}

func TestRunPodShellPhaseProvidesXcodeEnvironment(t *testing.T) {
	tc := scriptTestToolchain(t)
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	projectRoot := filepath.Join(root, "app")
	sourceRoot := filepath.Join(root, "plugin")
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	for _, dir := range []string{podsRoot, projectRoot, sourceRoot, configDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "input.txt"), []byte("42"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "inputs.xcfilelist"), []byte("$(PODS_TARGET_SRCROOT)/input.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "outputs.xcfilelist"), []byte("$(DERIVED_SOURCES_DIR)/GeneratedValue.h\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := podTarget{
		Name:             "ScriptFixture",
		ProductName:      "ScriptFixture",
		ModuleName:       "ScriptFixture",
		ProductType:      "com.apple.product-type.library.static",
		DeploymentTarget: "15.0",
		BuildSettings: map[string]string{
			"PODS_TARGET_SRCROOT": sourceRoot,
			"EXPECTED_CWD":        projectRoot,
		},
	}
	phase := podShellScriptPhase{
		Name:                "Generate header",
		ShellPath:           "/bin/sh",
		InputPaths:          []string{"$(PODS_TARGET_SRCROOT)/input.txt"},
		OutputPaths:         []string{"$(DERIVED_SOURCES_DIR)/GeneratedValue.h"},
		InputFileListPaths:  []string{"$(PODS_TARGET_SRCROOT)/inputs.xcfilelist"},
		OutputFileListPaths: []string{"$(PODS_TARGET_SRCROOT)/outputs.xcfilelist"},
		Script: `set -eu

test "$SCRIPT_INPUT_FILE_COUNT" = 1
test "$SCRIPT_OUTPUT_FILE_COUNT" = 1
test "$SCRIPT_INPUT_FILE_LIST_COUNT" = 1
test "$SCRIPT_OUTPUT_FILE_LIST_COUNT" = 1
test "$SCRIPT_INPUT_FILE_0" = "$PODS_TARGET_SRCROOT/input.txt"
test "$SCRIPT_INPUT_FILE_LIST_0" = "$PODS_TARGET_SRCROOT/inputs.xcfilelist"
test "$SCRIPT_OUTPUT_FILE_LIST_0" = "$PODS_TARGET_SRCROOT/outputs.xcfilelist"
test "$BUILT_PRODUCTS_DIR" = "$TARGET_BUILD_DIR"
test "$CONFIGURATION_BUILD_DIR" = "$TARGET_BUILD_DIR"
test "$(pwd -P)" = "$(cd "$EXPECTED_CWD" && pwd -P)"
mkdir -p "$(dirname "$SCRIPT_OUTPUT_FILE_0")"
printf '#define GENERATED_VALUE %s\n' "$(cat "$SCRIPT_INPUT_FILE_0")" > "$SCRIPT_OUTPUT_FILE_0"
printf '%s\n' "$TARGET_NAME:$CONFIGURATION:$PLATFORM_NAME" > "$TARGET_BUILD_DIR/phase-env.txt"
`,
	}
	if err := runPodShellPhase(tc, podManifest{PodsRoot: podsRoot, ProjectRoot: projectRoot}, target, configDir, "15.0", phase); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(configDir, target.Name, "DerivedSources", "GeneratedValue.h")
	data, err := os.ReadFile(generated)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "#define GENERATED_VALUE 42\n" {
		t.Fatalf("unexpected generated header: %q", data)
	}
	envMarker, err := os.ReadFile(filepath.Join(configDir, target.Name, "phase-env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(envMarker)) != "ScriptFixture:Release:iphoneos" {
		t.Fatalf("unexpected environment marker: %q", envMarker)
	}
}

func TestRunPodShellPhasesPreservesPreAndPostSourceOrder(t *testing.T) {
	tc := scriptTestToolchain(t)
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	if err := os.MkdirAll(podsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "order.txt")
	target := podTarget{
		Name:        "OrderingFixture",
		ProductName: "OrderingFixture",
		ProductType: "com.apple.product-type.library.static",
		BuildSettings: map[string]string{
			"ORDER_MARKER": marker,
		},
		ShellScriptPhases: []podShellScriptPhase{
			{Name: "post-b", Order: 6, Script: `printf '%s\n' post-b >> "$ORDER_MARKER"`},
			{Name: "pre-b", Order: 3, BeforeSources: true, Script: `printf '%s\n' pre-b >> "$ORDER_MARKER"`},
			{Name: "post-a", Order: 5, Script: `printf '%s\n' post-a >> "$ORDER_MARKER"`},
			{Name: "pre-a", Order: 2, BeforeSources: true, Script: `printf '%s\n' pre-a >> "$ORDER_MARKER"`},
			{Name: "headers-b", Order: 1, BeforeSources: true, BeforeHeaders: true, Script: `printf '%s\n' headers-b >> "$ORDER_MARKER"`},
			{Name: "headers-a", Order: 0, BeforeSources: true, BeforeHeaders: true, Script: `printf '%s\n' headers-a >> "$ORDER_MARKER"`},
		},
	}
	manifest := podManifest{PodsRoot: podsRoot}
	for _, stage := range []podScriptStage{podScriptBeforeHeaders, podScriptAfterHeaders, podScriptAfterSources} {
		if err := runPodShellPhases(tc, manifest, target, configDir, "15.0", stage); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	want := "headers-a\nheaders-b\npre-a\npre-b\npost-a\npost-b"
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("unexpected phase order:\n%s", got)
	}
}

func TestKnownCocoaPodsScriptPhaseIsHandledInternally(t *testing.T) {
	if !podScriptPhaseHandledInternally(podShellScriptPhase{Name: "[CP] Copy XCFrameworks"}) {
		t.Fatal("Copy XCFrameworks should remain an internal ripley operation")
	}
	if !podScriptPhaseHandledInternally(podShellScriptPhase{Name: "Copy generated compatibility header"}) {
		t.Fatal("Swift compatibility header phase should remain an internal ripley operation")
	}
	if podScriptPhaseHandledInternally(podShellScriptPhase{Name: "Generate Sources"}) {
		t.Fatal("custom phases must not be swallowed")
	}
}

func TestRunPodShellPhasesSplitsHeaderAndSourceStages(t *testing.T) {
	tc := scriptTestToolchain(t)
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	if err := os.MkdirAll(podsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "stages.txt")
	target := podTarget{
		Name:          "StageFixture",
		ProductName:   "StageFixture",
		ProductType:   "com.apple.product-type.library.static",
		BuildSettings: map[string]string{"STAGE_MARKER": marker},
		ShellScriptPhases: []podShellScriptPhase{
			{Name: "after-sources", Order: 5, Script: `printf '%s\n' after-sources >> "$STAGE_MARKER"`},
			{Name: "after-headers", Order: 2, BeforeSources: true, Script: `printf '%s\n' after-headers >> "$STAGE_MARKER"`},
			{Name: "before-headers", Order: 0, BeforeSources: true, BeforeHeaders: true, Script: `printf '%s\n' before-headers >> "$STAGE_MARKER"`},
		},
	}
	manifest := podManifest{PodsRoot: podsRoot}
	for _, stage := range []podScriptStage{podScriptBeforeHeaders, podScriptAfterHeaders, podScriptAfterSources} {
		if err := runPodShellPhases(tc, manifest, target, configDir, "15.0", stage); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "before-headers\nafter-headers\nafter-sources" {
		t.Fatalf("unexpected stage order:\n%s", got)
	}
}

func TestPodPhaseStageMapsRecordedPBXOrder(t *testing.T) {
	cases := []struct {
		phase podShellScriptPhase
		want  podScriptStage
	}{
		{podShellScriptPhase{BeforeSources: true, BeforeHeaders: true}, podScriptBeforeHeaders},
		{podShellScriptPhase{BeforeSources: true}, podScriptAfterHeaders},
		{podShellScriptPhase{}, podScriptAfterSources},
		// :any resolves through the real index, so a phase placed after Sources
		// must not be pulled forward even if it claims to precede headers.
		{podShellScriptPhase{BeforeHeaders: true}, podScriptAfterSources},
	}
	for _, tc := range cases {
		if got := podPhaseStage(tc.phase); got != tc.want {
			t.Fatalf("stage for %+v = %d, want %d", tc.phase, got, tc.want)
		}
	}
}

func TestRunPodShellPhaseSkipsWhenOutputsAreUpToDate(t *testing.T) {
	tc := scriptTestToolchain(t)
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	sourceRoot := filepath.Join(root, "plugin")
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	for _, dir := range []string{podsRoot, sourceRoot, configDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	input := filepath.Join(sourceRoot, "input.txt")
	if err := os.WriteFile(input, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := podTarget{
		Name:          "IncrementalFixture",
		ProductName:   "IncrementalFixture",
		ProductType:   "com.apple.product-type.library.static",
		BuildSettings: map[string]string{"PODS_TARGET_SRCROOT": sourceRoot},
	}
	counter := filepath.Join(root, "runs.txt")
	phase := podShellScriptPhase{
		Name:        "Generate Once",
		ShellPath:   "/bin/sh",
		InputPaths:  []string{"$(PODS_TARGET_SRCROOT)/input.txt"},
		OutputPaths: []string{"$(DERIVED_SOURCES_DIR)/generated.h"},
		Script: `set -eu
mkdir -p "$(dirname "$SCRIPT_OUTPUT_FILE_0")"
cat "$SCRIPT_INPUT_FILE_0" > "$SCRIPT_OUTPUT_FILE_0"
printf 'x\n' >> "$RUN_COUNTER"
`,
		BeforeSources: true,
	}
	target.BuildSettings["RUN_COUNTER"] = counter
	manifest := podManifest{PodsRoot: podsRoot}

	runs := func() int {
		data, err := os.ReadFile(counter)
		if err != nil {
			return 0
		}
		return len(strings.Fields(string(data)))
	}
	if err := runPodShellPhase(tc, manifest, target, configDir, "15.0", phase); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 1 {
		t.Fatalf("first build ran the phase %d times, want 1", got)
	}
	if err := runPodShellPhase(tc, manifest, target, configDir, "15.0", phase); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 1 {
		t.Fatalf("phase re-ran while up to date (%d runs)", got)
	}

	// Touching a declared input invalidates the phase.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(input, future, future); err != nil {
		t.Fatal(err)
	}
	if err := runPodShellPhase(tc, manifest, target, configDir, "15.0", phase); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 2 {
		t.Fatalf("phase did not re-run after its input changed (%d runs)", got)
	}

	// Editing the script itself also invalidates it.
	changed := phase
	changed.Script = phase.Script + "printf 'edited\n' > /dev/null\n"
	if err := runPodShellPhase(tc, manifest, target, configDir, "15.0", changed); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 3 {
		t.Fatalf("phase did not re-run after the script changed (%d runs)", got)
	}
}

func TestRunPodShellPhaseAlwaysOutOfDateIgnoresState(t *testing.T) {
	tc := scriptTestToolchain(t)
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	if err := os.MkdirAll(podsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(root, "runs.txt")
	target := podTarget{
		Name:          "AlwaysFixture",
		ProductName:   "AlwaysFixture",
		ProductType:   "com.apple.product-type.library.static",
		BuildSettings: map[string]string{"RUN_COUNTER": counter},
	}
	phase := podShellScriptPhase{
		Name:            "Always",
		ShellPath:       "/bin/sh",
		OutputPaths:     []string{"$(TARGET_BUILD_DIR)/always.txt"},
		AlwaysOutOfDate: true,
		Script: `set -eu
mkdir -p "$(dirname "$SCRIPT_OUTPUT_FILE_0")"
printf 'ok\n' > "$SCRIPT_OUTPUT_FILE_0"
printf 'x\n' >> "$RUN_COUNTER"
`,
	}
	manifest := podManifest{PodsRoot: podsRoot}
	for range 3 {
		if err := runPodShellPhase(tc, manifest, target, configDir, "15.0", phase); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Fields(string(data))); got != 3 {
		t.Fatalf("alwaysOutOfDate phase ran %d times, want 3", got)
	}
}

func TestRunPodShellPhaseHonoursDependencyFile(t *testing.T) {
	tc := scriptTestToolchain(t)
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	sourceRoot := filepath.Join(root, "plugin")
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	for _, dir := range []string{podsRoot, sourceRoot, configDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	hidden := filepath.Join(sourceRoot, "hidden.h")
	if err := os.WriteFile(hidden, []byte("// hidden"), 0o644); err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(root, "runs.txt")
	target := podTarget{
		Name:        "DependencyFixture",
		ProductName: "DependencyFixture",
		ProductType: "com.apple.product-type.library.static",
		BuildSettings: map[string]string{
			"PODS_TARGET_SRCROOT": sourceRoot,
			"RUN_COUNTER":         counter,
		},
	}
	phase := podShellScriptPhase{
		Name:           "Generate With Deps",
		ShellPath:      "/bin/sh",
		OutputPaths:    []string{"$(DERIVED_SOURCES_DIR)/dep-generated.h"},
		DependencyFile: "$(DERIVED_FILE_DIR)/phase.d",
		Script: `set -eu
mkdir -p "$(dirname "$SCRIPT_OUTPUT_FILE_0")"
printf 'generated\n' > "$SCRIPT_OUTPUT_FILE_0"
printf '%s: %s\n' "$SCRIPT_OUTPUT_FILE_0" "$PODS_TARGET_SRCROOT/hidden.h" > "$DERIVED_FILE_DIR/phase.d"
printf 'x\n' >> "$RUN_COUNTER"
`,
	}
	manifest := podManifest{PodsRoot: podsRoot}
	runs := func() int {
		data, err := os.ReadFile(counter)
		if err != nil {
			return 0
		}
		return len(strings.Fields(string(data)))
	}
	if err := runPodShellPhase(tc, manifest, target, configDir, "15.0", phase); err != nil {
		t.Fatal(err)
	}
	if err := runPodShellPhase(tc, manifest, target, configDir, "15.0", phase); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 1 {
		t.Fatalf("phase re-ran while dependencies were unchanged (%d runs)", got)
	}
	// The dependency is not a declared input; only the dependency file exposes it.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(hidden, future, future); err != nil {
		t.Fatal(err)
	}
	if err := runPodShellPhase(tc, manifest, target, configDir, "15.0", phase); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 2 {
		t.Fatalf("phase ignored a changed dependency-file entry (%d runs)", got)
	}
}

func TestRunPodShellPhaseReportsUnavailableMacOSTool(t *testing.T) {
	tc := scriptTestToolchain(t)
	root := t.TempDir()
	podsRoot := filepath.Join(root, "Pods")
	configDir := filepath.Join(root, "build", "Release-iphoneos")
	if err := os.MkdirAll(podsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	phase := podShellScriptPhase{
		Name:      "Compile Asset Catalog",
		ShellPath: "/bin/sh",
		Script: `set -eu
actool --compile "$TARGET_BUILD_DIR" Assets.xcassets
`,
	}
	// The diagnostic must depend on the phase's own PATH, not on whether the
	// machine running the tests happens to have Xcode installed.
	empty := filepath.Join(root, "empty-bin")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", empty)
	target := podTarget{
		Name:          "ToolFixture",
		ProductName:   "ToolFixture",
		ProductType:   "com.apple.product-type.library.static",
		BuildSettings: map[string]string{},
	}
	err := runPodShellPhase(tc, podManifest{PodsRoot: podsRoot}, target, configDir, "15.0", phase)
	if err == nil {
		t.Fatal("expected the phase to fail rather than be skipped silently")
	}
	if !strings.Contains(err.Error(), "requires unavailable macOS tools") || !strings.Contains(err.Error(), "actool") {
		t.Fatalf("error lacks an actionable macOS-tool diagnostic: %v", err)
	}
}
