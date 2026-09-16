package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ripley/internal/toolchain"
)

func appScriptTestToolchain(t *testing.T) toolchain.Toolchain {
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
	return toolchain.Toolchain{Root: filepath.Join(root, "toolchain")}
}

func TestApplicationShellScriptPhasesPreserveTargetOrder(t *testing.T) {
	preID := "111111111111111111111111"
	sourcesID := "222222222222222222222222"
	postID := "333333333333333333333333"
	target := xcodeTarget{BuildPhases: []string{preID, sourcesID, postID}}
	objects := map[string]pbxObject{
		preID: {
			ID:      preID,
			Comment: "Generate Config",
			Body: `
				isa = PBXShellScriptBuildPhase;
				name = "Generate Config";
				shellPath = /bin/sh;
				shellScript = "printf 'first\nsecond\n' > \"$SCRIPT_OUTPUT_FILE_0\"";
				inputPaths = ("${SRCROOT}/input.txt",);
				outputPaths = ("${DERIVED_SOURCES_DIR}/generated.txt",);
			`,
		},
		sourcesID: {ID: sourcesID, Body: `isa = PBXSourcesBuildPhase;`},
		postID: {
			ID:      postID,
			Comment: "Post Build",
			Body: `
				isa = PBXShellScriptBuildPhase;
				name = "Post Build";
				shellPath = /bin/sh;
				shellScript = "echo post";
			`,
		},
	}
	phases := applicationShellScriptPhases(target, objects, t.TempDir())
	if len(phases) != 2 {
		t.Fatalf("got %d phases, want 2", len(phases))
	}
	if phases[0].Name != "Generate Config" || !phases[0].BeforeSources || phases[0].Order != 0 {
		t.Fatalf("unexpected pre phase: %+v", phases[0])
	}
	if !strings.Contains(phases[0].Script, "first\nsecond\n") {
		t.Fatalf("multiline shell script was not decoded: %q", phases[0].Script)
	}
	if len(phases[0].InputPaths) != 1 || phases[0].InputPaths[0] != "${SRCROOT}/input.txt" {
		t.Fatalf("unexpected input paths: %#v", phases[0].InputPaths)
	}
	if phases[1].Name != "Post Build" || phases[1].BeforeSources || phases[1].Order != 2 {
		t.Fatalf("unexpected post phase: %+v", phases[1])
	}
}

func TestRunApplicationShellPhaseProvidesXcodeEnvironment(t *testing.T) {
	tc := appScriptTestToolchain(t)
	root := t.TempDir()
	iosRoot := filepath.Join(root, "ios")
	buildDir := filepath.Join(root, "build", "ripley_ios")
	if err := os.MkdirAll(iosRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(iosRoot, "input.txt")
	if err := os.WriteFile(input, []byte("app-phase"), 0o644); err != nil {
		t.Fatal(err)
	}
	project := project{Root: root, BuildDir: buildDir}
	config := iosProject{
		IOSRoot:        iosRoot,
		XcodeProject:   filepath.Join(iosRoot, "Runner.xcodeproj"),
		TargetName:     "Runner",
		ProductName:    "Runner",
		ExecutableName: "Runner",
		ModuleName:     "Runner",
		BundleID:       "example.app",
		MinOS:          "15.0",
		Configuration:  "Release",
		FlutterRoot:    filepath.Join(root, "flutter"),
		BuildSettings:  map[string]string{},
	}
	phase := xcodeShellScriptPhase{
		Name:        "Generate App Marker",
		ShellPath:   "/bin/sh",
		InputPaths:  []string{"$(SRCROOT)/input.txt"},
		OutputPaths: []string{"$(DERIVED_SOURCES_DIR)/app-marker.txt"},
		Script: `set -eu

test "$TARGET_NAME" = Runner
test "$CONFIGURATION" = Release
test "$PLATFORM_NAME" = iphoneos
test "$SCRIPT_INPUT_FILE_0" = "$SRCROOT/input.txt"
mkdir -p "$(dirname "$SCRIPT_OUTPUT_FILE_0")"
cat "$SCRIPT_INPUT_FILE_0" > "$SCRIPT_OUTPUT_FILE_0"
`,
	}
	if err := runApplicationShellPhase(tc, project, config, phase); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(buildDir, ".app_build", "DerivedSources", "app-marker.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "app-phase" {
		t.Fatalf("unexpected marker: %q", data)
	}
}

func TestPostApplicationPhaseSeesBuiltExecutable(t *testing.T) {
	tc := appScriptTestToolchain(t)
	root := t.TempDir()
	iosRoot := filepath.Join(root, "ios")
	buildDir := filepath.Join(root, "build", "ripley_ios")
	appDir := filepath.Join(buildDir, "Runner.app")
	if err := os.MkdirAll(iosRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(appDir, "Runner")
	if err := os.WriteFile(executable, []byte("fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := project{Root: root, BuildDir: buildDir}
	config := iosProject{
		IOSRoot:        iosRoot,
		XcodeProject:   filepath.Join(iosRoot, "Runner.xcodeproj"),
		TargetName:     "Runner",
		ProductName:    "Runner",
		ExecutableName: "Runner",
		ModuleName:     "Runner",
		BundleID:       "example.app",
		MinOS:          "15.0",
		Configuration:  "Release",
		FlutterRoot:    filepath.Join(root, "flutter"),
		BuildSettings:  map[string]string{},
	}
	phase := xcodeShellScriptPhase{
		Name:      "Verify Built App",
		ShellPath: "/bin/sh",
		Script: `set -eu

test -x "$TARGET_BUILD_DIR/$EXECUTABLE_PATH"
printf '%s\n' "$FULL_PRODUCT_NAME" > "$TARGET_BUILD_DIR/app-post-marker.txt"
`,
	}
	if err := runApplicationShellPhase(tc, project, config, phase); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(buildDir, "app-post-marker.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "Runner.app" {
		t.Fatalf("unexpected post marker: %q", data)
	}
}

func TestStandardXcodePhasesStayInternal(t *testing.T) {
	root := t.TempDir()
	if !applicationScriptPhaseHandledInternally(xcodeShellScriptPhase{Script: `/bin/sh "$FLUTTER_ROOT/packages/flutter_tools/bin/xcode_backend.sh" build`}, root) {
		t.Fatal("Flutter xcode_backend phase must not execute twice")
	}
	if !applicationScriptPhaseHandledInternally(xcodeShellScriptPhase{Name: "[CP] Embed Pods Frameworks"}, root) {
		t.Fatal("CocoaPods embed phase must stay internal")
	}
	if applicationScriptPhaseHandledInternally(xcodeShellScriptPhase{Name: "Generate Config", Script: "echo ok"}, root) {
		t.Fatal("custom app script phase must execute")
	}
}

// A project may run Flutter's build through its own wrapper script rather than
// naming xcode_backend.sh inline (immich does exactly this). Matching only the
// phase's own text made ripley execute a second, redundant Flutter build.
func TestWrappedFlutterBackendPhaseStaysInternal(t *testing.T) {
	root := t.TempDir()
	scripts := filepath.Join(root, "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(scripts, "xcode_flutter_build.sh")
	body := "#!/bin/bash\n/bin/sh \"$FLUTTER_ROOT/packages/flutter_tools/bin/xcode_backend.sh\" build 2>&1 | awk '{print}'\nexit \"${PIPESTATUS[0]}\"\n"
	if err := os.WriteFile(wrapper, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	phase := xcodeShellScriptPhase{Name: "Run Script", Script: "/bin/bash \"$SRCROOT/scripts/xcode_flutter_build.sh\"\n"}
	if !applicationScriptPhaseHandledInternally(phase, root) {
		t.Fatal("a phase whose wrapper script runs xcode_backend.sh must stay internal")
	}
	// A wrapper that does something else entirely must still run.
	other := filepath.Join(scripts, "generate.sh")
	if err := os.WriteFile(other, []byte("#!/bin/bash\necho generated\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	custom := xcodeShellScriptPhase{Name: "Generate", Script: "/bin/bash \"$SRCROOT/scripts/generate.sh\"\n"}
	if applicationScriptPhaseHandledInternally(custom, root) {
		t.Fatal("an unrelated wrapper script must still execute")
	}
}

func TestApplicationShellScriptPhaseParsesDependencyFile(t *testing.T) {
	phaseID := "444444444444444444444444"
	sourcesID := "555555555555555555555555"
	target := xcodeTarget{BuildPhases: []string{phaseID, sourcesID}}
	objects := map[string]pbxObject{
		phaseID: {
			ID:      phaseID,
			Comment: "Generate With Deps",
			Body: `
				isa = PBXShellScriptBuildPhase;
				name = "Generate With Deps";
				alwaysOutOfDate = 1;
				dependencyFile = "$(DERIVED_FILE_DIR)/phase.d";
				shellPath = /bin/bash;
				shellScript = "echo deps";
				inputFileListPaths = ("${SRCROOT}/in.xcfilelist",);
				outputFileListPaths = ("${SRCROOT}/out.xcfilelist",);
			`,
		},
		sourcesID: {ID: sourcesID, Body: `isa = PBXSourcesBuildPhase;`},
	}
	phases := applicationShellScriptPhases(target, objects, t.TempDir())
	if len(phases) != 1 {
		t.Fatalf("got %d phases, want 1", len(phases))
	}
	phase := phases[0]
	if phase.DependencyFile != "$(DERIVED_FILE_DIR)/phase.d" {
		t.Fatalf("dependency file not parsed: %q", phase.DependencyFile)
	}
	if !phase.AlwaysOutOfDate {
		t.Fatal("alwaysOutOfDate not parsed")
	}
	if phase.ShellPath != "/bin/bash" {
		t.Fatalf("unexpected shell path: %q", phase.ShellPath)
	}
	if len(phase.InputFileListPaths) != 1 || phase.InputFileListPaths[0] != "${SRCROOT}/in.xcfilelist" {
		t.Fatalf("unexpected input file lists: %#v", phase.InputFileListPaths)
	}
	if len(phase.OutputFileListPaths) != 1 || phase.OutputFileListPaths[0] != "${SRCROOT}/out.xcfilelist" {
		t.Fatalf("unexpected output file lists: %#v", phase.OutputFileListPaths)
	}
}

func TestApplicationPhaseStageSplit(t *testing.T) {
	cases := []struct {
		phase xcodeShellScriptPhase
		want  applicationScriptStage
	}{
		{xcodeShellScriptPhase{BeforeSources: true}, applicationScriptBeforeFlutter},
		{xcodeShellScriptPhase{BeforeSources: true, AfterFlutterBuild: true}, applicationScriptAfterFlutter},
		{xcodeShellScriptPhase{}, applicationScriptAfterSources},
	}
	for _, item := range cases {
		if got := applicationPhaseStage(item.phase); got != item.want {
			t.Fatalf("stage for %+v = %v, want %v", item.phase, got, item.want)
		}
	}
}

func TestApplicationShellPhaseStagesFollowFlutterBuildPhase(t *testing.T) {
	beforeID := "666666666666666666666666"
	flutterID := "777777777777777777777777"
	afterID := "888888888888888888888888"
	sourcesID := "999999999999999999999999"
	postID := "aaaaaaaaaaaaaaaaaaaaaaaa"
	target := xcodeTarget{BuildPhases: []string{beforeID, flutterID, afterID, sourcesID, postID}}
	// Real pbxproj files put one field per line, which is what the parser keys on.
	scriptBody := func(name, script string) string {
		return "\n\t\t\tisa = PBXShellScriptBuildPhase;\n\t\t\tname = \"" + name + "\";\n\t\t\tshellScript = \"" + script + "\";\n\t\t"
	}
	objects := map[string]pbxObject{
		beforeID:  {ID: beforeID, Body: scriptBody("Before", "echo before")},
		flutterID: {ID: flutterID, Body: scriptBody("Run Script", `/bin/sh \"$FLUTTER_ROOT/packages/flutter_tools/bin/xcode_backend.sh\" build`)},
		afterID:   {ID: afterID, Body: scriptBody("After", "echo after")},
		sourcesID: {ID: sourcesID, Body: "\n\t\t\tisa = PBXSourcesBuildPhase;\n\t\t"},
		postID:    {ID: postID, Body: scriptBody("Post", "echo post")},
	}
	stages := make(map[string]applicationScriptStage)
	for _, phase := range applicationShellScriptPhases(target, objects, t.TempDir()) {
		stages[phase.Name] = applicationPhaseStage(phase)
	}
	if stages["Before"] != applicationScriptBeforeFlutter {
		t.Fatalf("Before phase stage = %v", stages["Before"])
	}
	if stages["After"] != applicationScriptAfterFlutter {
		t.Fatalf("After phase stage = %v", stages["After"])
	}
	if stages["Post"] != applicationScriptAfterSources {
		t.Fatalf("Post phase stage = %v", stages["Post"])
	}
}

func TestRunApplicationShellPhaseSkipsWhenUpToDate(t *testing.T) {
	tc := appScriptTestToolchain(t)
	root := t.TempDir()
	iosRoot := filepath.Join(root, "ios")
	buildDir := filepath.Join(root, "build", "ripley_ios")
	if err := os.MkdirAll(iosRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(iosRoot, "input.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(root, "runs.txt")
	project := project{Root: root, BuildDir: buildDir}
	config := iosProject{
		IOSRoot:        iosRoot,
		XcodeProject:   filepath.Join(iosRoot, "Runner.xcodeproj"),
		TargetName:     "Runner",
		ProductName:    "Runner",
		ExecutableName: "Runner",
		ModuleName:     "Runner",
		BundleID:       "example.app",
		MinOS:          "15.0",
		Configuration:  "Release",
		FlutterRoot:    filepath.Join(root, "flutter"),
		BuildSettings:  map[string]string{"RUN_COUNTER": counter},
	}
	phase := xcodeShellScriptPhase{
		Name:        "Incremental",
		ShellPath:   "/bin/sh",
		InputPaths:  []string{"$(SRCROOT)/input.txt"},
		OutputPaths: []string{"$(TARGET_BUILD_DIR)/incremental.txt"},
		Script: `set -eu
printf 'x' >> "$RUN_COUNTER"
mkdir -p "$(dirname "$SCRIPT_OUTPUT_FILE_0")"
cat "$SCRIPT_INPUT_FILE_0" > "$SCRIPT_OUTPUT_FILE_0"
`,
	}
	runs := func() int {
		data, err := os.ReadFile(counter)
		if err != nil {
			return 0
		}
		return len(strings.TrimSpace(string(data)))
	}
	for i := 0; i < 3; i++ {
		if err := runApplicationShellPhase(tc, project, config, phase); err != nil {
			t.Fatal(err)
		}
	}
	if got := runs(); got != 1 {
		t.Fatalf("phase executed %d times, want 1 (later runs should be up to date)", got)
	}

	// A changed input must re-run the phase.
	if err := os.WriteFile(filepath.Join(iosRoot, "input.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(iosRoot, "input.txt"), future, future); err != nil {
		t.Fatal(err)
	}
	if err := runApplicationShellPhase(tc, project, config, phase); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 2 {
		t.Fatalf("phase executed %d times after input change, want 2", got)
	}
	data, err := os.ReadFile(filepath.Join(buildDir, "incremental.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "v2" {
		t.Fatalf("output not regenerated: %q", data)
	}

	// alwaysOutOfDate phases ignore the cache entirely.
	always := phase
	always.AlwaysOutOfDate = true
	if err := runApplicationShellPhase(tc, project, config, always); err != nil {
		t.Fatal(err)
	}
	if got := runs(); got != 3 {
		t.Fatalf("alwaysOutOfDate phase was skipped (%d runs)", got)
	}
}

func TestRunApplicationShellPhaseReportsMissingMacOSTool(t *testing.T) {
	tc := appScriptTestToolchain(t)
	root := t.TempDir()
	iosRoot := filepath.Join(root, "ios")
	if err := os.MkdirAll(iosRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	project := project{Root: root, BuildDir: filepath.Join(root, "build", "ripley_ios")}
	config := iosProject{
		IOSRoot:        iosRoot,
		XcodeProject:   filepath.Join(iosRoot, "Runner.xcodeproj"),
		TargetName:     "Runner",
		ProductName:    "Runner",
		ExecutableName: "Runner",
		ModuleName:     "Runner",
		BundleID:       "example.app",
		MinOS:          "15.0",
		Configuration:  "Release",
		FlutterRoot:    filepath.Join(root, "flutter"),
		BuildSettings:  map[string]string{},
	}
	phase := xcodeShellScriptPhase{
		Name:      "Compile Asset Catalog",
		ShellPath: "/bin/sh",
		Script:    "set -eu\nactool --compile \"$TARGET_BUILD_DIR\" Assets.xcassets\n",
	}
	// The diagnostic must depend on the phase's own PATH, not on whether the
	// machine running the tests happens to have Xcode installed.
	empty := filepath.Join(root, "empty-bin")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", empty)
	err := runApplicationShellPhase(tc, project, config, phase)
	if err == nil {
		t.Fatal("phase calling a macOS-only tool must fail, not be skipped")
	}
	if !strings.Contains(err.Error(), "actool") {
		t.Fatalf("diagnostic did not name the missing tool: %v", err)
	}
	if !strings.Contains(err.Error(), "requires unavailable macOS tools") {
		t.Fatalf("missing macOS tool diagnostic: %v", err)
	}
}
