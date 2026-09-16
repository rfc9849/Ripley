package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	buildRuleProjectID  = "0000000000000000000000A1"
	buildRuleTargetID   = "0000000000000000000000A2"
	buildRuleProtoID    = "0000000000000000000000A3"
	buildRuleCSourceID  = "0000000000000000000000A4"
	buildRuleShadowID   = "0000000000000000000000A5"
	buildRuleInternalID = "0000000000000000000000A6"
)

// buildRuleFixture writes a project.pbxproj fragment carrying the given
// PBXBuildRule bodies and returns the parsed objects plus the application
// target. ruleIDs are attached to the target in order; projectRuleIDs to the
// PBXProject.
func buildRuleFixture(t *testing.T, bodies map[string]string, targetRuleIDs, projectRuleIDs []string) (map[string]pbxObject, xcodeTarget) {
	t.Helper()
	var rules strings.Builder
	for id, body := range bodies {
		fmt.Fprintf(&rules, "\t%s /* PBXBuildRule */ = {\n\t\tisa = PBXBuildRule;\n%s\t};\n", id, body)
	}
	quote := func(ids []string) string {
		if len(ids) == 0 {
			return "()"
		}
		return "(\n\t\t\t" + strings.Join(ids, ",\n\t\t\t") + ",\n\t\t)"
	}
	pbx := fmt.Sprintf(`// !$*UTF8*$!
{
objects = {
	%s /* Project object */ = {
		isa = PBXProject;
		buildRules = %s;
		developmentRegion = en;
	};
	%s /* Runner */ = {
		isa = PBXNativeTarget;
		buildRules = %s;
		name = Runner;
		productType = "com.apple.product-type.application";
	};
%s};
}
`, buildRuleProjectID, quote(projectRuleIDs), buildRuleTargetID, quote(targetRuleIDs), rules.String())

	objects, err := parsePBXObjects(pbx)
	if err != nil {
		t.Fatalf("parse fixture project: %v", err)
	}
	target, err := selectApplicationTarget("Runner.xcodeproj", objects, "")
	if err != nil {
		t.Fatalf("select application target: %v", err)
	}
	return objects, target
}

func TestParseBuildRulesReadsCustomRules(t *testing.T) {
	bodies := map[string]string{
		buildRuleProtoID: `		compilerSpec = com.apple.compilers.proxy.script;
		dependencyFile = "$(DERIVED_FILE_DIR)/$(INPUT_FILE_BASE).d";
		filePatterns = "*.proto *.protodef";
		fileType = pattern.proxy;
		inputFiles = (
			"$(SRCROOT)/tools/protoc-shim",
		);
		isEditable = 1;
		name = "Protobuf codegen";
		outputFiles = (
			"$(DERIVED_FILE_DIR)/$(INPUT_FILE_BASE).pb.c",
			"$(DERIVED_FILE_DIR)/$(INPUT_FILE_BASE).pb.h",
		);
		runOncePerArchitecture = 0;
		script = "protoc-shim \"$INPUT_FILE_PATH\"\n";
`,
		buildRuleCSourceID: `		compilerSpec = com.apple.compilers.proxy.script;
		fileType = sourcecode.c.c;
		inputFiles = (
		);
		outputFiles = (
			"$(OBJECT_FILE_DIR)/$(CURRENT_ARCH)/$(INPUT_FILE_BASE).o",
		);
		runOncePerArchitecture = 1;
		script = "clang -c \"$INPUT_FILE_PATH\" -o \"$SCRIPT_OUTPUT_FILE_0\"\n";
`,
	}
	// Target order decides precedence, so keep the proto rule first.
	objects, target := buildRuleFixture(t, bodies, []string{buildRuleProtoID, buildRuleCSourceID}, nil)

	settings := map[string]string{
		"SRCROOT":          "/src/ios",
		"DERIVED_FILE_DIR": "/must/not/leak",
		"OBJECT_FILE_DIR":  "/must/not/leak",
		"CURRENT_ARCH":     "must-not-leak",
	}
	rules, err := parseBuildRules(objects, target, settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(rules))
	}

	proto := rules[0]
	if proto.Name != "Protobuf codegen" {
		t.Errorf("rule name = %q, want %q", proto.Name, "Protobuf codegen")
	}
	if proto.Scope != buildRuleTargetScope {
		t.Errorf("rule scope = %v, want target", proto.Scope)
	}
	if !proto.IsScript() {
		t.Errorf("compilerSpec %q was not recognised as a script rule", proto.CompilerSpec)
	}
	if !proto.matchesPattern() {
		t.Errorf("fileType %q was not recognised as a pattern rule", proto.FileType)
	}
	if want := []string{"*.proto", "*.protodef"}; !equalStrings(proto.FilePatterns, want) {
		t.Errorf("filePatterns = %v, want %v", proto.FilePatterns, want)
	}
	if proto.RunOncePerArchitecture {
		t.Error("runOncePerArchitecture = true, want false")
	}
	// $(SRCROOT) is a real build setting and must be expanded at parse time.
	if want := []string{"/src/ios/tools/protoc-shim"}; !equalStrings(proto.InputFiles, want) {
		t.Errorf("inputFiles = %v, want %v", proto.InputFiles, want)
	}
	// Per-file variables must survive parse-time expansion untouched, otherwise
	// the outputs would be frozen to whatever the project settings happen to say.
	for _, output := range append(append([]string(nil), proto.OutputFiles...), proto.DependencyFile) {
		if strings.Contains(output, "must") {
			t.Errorf("per-file variable was expanded at parse time: %q", output)
		}
		if !strings.Contains(output, "$(DERIVED_FILE_DIR)") {
			t.Errorf("declared output lost its per-file variable: %q", output)
		}
	}
	if !strings.Contains(proto.Script, `protoc-shim "$INPUT_FILE_PATH"`) {
		t.Errorf("script body not decoded: %q", proto.Script)
	}

	native := rules[1]
	if native.FileType != "sourcecode.c.c" {
		t.Errorf("fileType = %q, want sourcecode.c.c", native.FileType)
	}
	if native.matchesPattern() {
		t.Error("a sourcecode.c.c rule must not be treated as a pattern rule")
	}
	if !native.RunOncePerArchitecture {
		t.Error("runOncePerArchitecture = false, want true")
	}
}

func TestParseBuildRulesRejectsIncompleteRules(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "no selector",
			body: "\t\tcompilerSpec = com.apple.compilers.proxy.script;\n\t\tscript = \"true\\n\";\n",
			want: "declares neither fileType nor filePatterns",
		},
		{
			name: "no compiler spec",
			body: "\t\tfileType = sourcecode.c.c;\n\t\tscript = \"true\\n\";\n",
			want: "declares no compilerSpec",
		},
		{
			name: "empty script",
			body: "\t\tcompilerSpec = com.apple.compilers.proxy.script;\n\t\tfileType = sourcecode.c.c;\n\t\tscript = \"\";\n",
			want: "script rule with an empty script",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objects, target := buildRuleFixture(t, map[string]string{buildRuleProtoID: tc.body}, []string{buildRuleProtoID}, nil)
			_, err := parseBuildRules(objects, target, map[string]string{})
			if err == nil {
				t.Fatal("incomplete rule was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestMatchBuildRulesPrecedence(t *testing.T) {
	// A project-level catch-all, then two target rules: the later target rule
	// claims .proto away from the earlier one, and the project rule loses to
	// both. Xcode searches project rules first and the last match wins.
	bodies := map[string]string{
		buildRuleShadowID: `		compilerSpec = com.apple.compilers.proxy.script;
		fileType = file;
		outputFiles = ("$(DERIVED_FILE_DIR)/catchall.txt");
		script = "true\n";
`,
		buildRuleProtoID: `		compilerSpec = com.apple.compilers.proxy.script;
		filePatterns = "*.proto";
		fileType = pattern.proxy;
		name = "First proto rule";
		outputFiles = ("$(DERIVED_FILE_DIR)/first.c");
		script = "true\n";
`,
		buildRuleCSourceID: `		compilerSpec = com.apple.compilers.proxy.script;
		filePatterns = "*.proto";
		fileType = pattern.proxy;
		name = "Winning proto rule";
		outputFiles = ("$(DERIVED_FILE_DIR)/second.c");
		script = "true\n";
`,
	}
	objects, target := buildRuleFixture(t, bodies,
		[]string{buildRuleProtoID, buildRuleCSourceID}, []string{buildRuleShadowID})

	rules, err := parseBuildRules(objects, target, map[string]string{"SRCROOT": "/src/ios"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatalf("got %d rules, want 3", len(rules))
	}
	if rules[0].Scope != buildRuleProjectScope {
		t.Fatalf("project rule must be searched first, got scope %v", rules[0].Scope)
	}

	matches, err := matchBuildRules(rules, []string{"/src/ios/api/user.proto", "/src/ios/README.md"}, "/src/ios")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("got %d matches, want 2", len(matches))
	}
	if got := matches[0].Rule.Name; got != "Winning proto rule" {
		t.Errorf("user.proto claimed by %q, want the last matching target rule", got)
	}
	// The catch-all `file` rule still claims anything no other rule wants.
	if got := matches[1].Input; got != filepath.Clean("/src/ios/README.md") {
		t.Errorf("catch-all rule claimed %q", got)
	}
	if matches[1].Rule.Scope != buildRuleProjectScope {
		t.Errorf("catch-all match scope = %v, want project", matches[1].Rule.Scope)
	}
}

func TestMatchBuildRulesFileTypeConformance(t *testing.T) {
	bodies := map[string]string{
		buildRuleCSourceID: `		compilerSpec = com.apple.compilers.proxy.script;
		fileType = sourcecode.c;
		outputFiles = ("$(DERIVED_FILE_DIR)/out.o");
		script = "true\n";
`,
	}
	objects, target := buildRuleFixture(t, bodies, []string{buildRuleCSourceID}, nil)
	rules, err := parseBuildRules(objects, target, map[string]string{"SRCROOT": "/src"})
	if err != nil {
		t.Fatal(err)
	}
	// sourcecode.c.c and sourcecode.c.objc both refine sourcecode.c; Swift and
	// Metal do not.
	sources := []string{"/src/a.c", "/src/b.m", "/src/c.h", "/src/d.swift", "/src/e.metal"}
	matches, err := matchBuildRules(rules, sources, "/src")
	if err != nil {
		t.Fatal(err)
	}
	var claimed []string
	for _, match := range matches {
		claimed = append(claimed, filepath.Base(match.Input))
	}
	if want := []string{"a.c", "b.m", "c.h"}; !equalStrings(claimed, want) {
		t.Fatalf("claimed %v, want %v", claimed, want)
	}
}

func TestMatchBuildRulesRejectsUnsupportedCompilerSpec(t *testing.T) {
	bodies := map[string]string{
		buildRuleInternalID: `		compilerSpec = com.apple.compilers.model.coredata;
		fileType = wrapper.xcdatamodel;
`,
	}
	objects, target := buildRuleFixture(t, bodies, []string{buildRuleInternalID}, nil)
	rules, err := parseBuildRules(objects, target, map[string]string{"SRCROOT": "/src"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = matchBuildRules(rules, []string{"/src/Model.xcdatamodel"}, "/src")
	if err == nil {
		t.Fatal("a rule claiming files with an unrunnable built-in compiler was silently ignored")
	}
	for _, want := range []string{"com.apple.compilers.model.coredata", "Model.xcdatamodel", "wrapper.xcdatamodel"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}

	// A rule that only restates behaviour ripley already implements leaves the file
	// to the normal pipeline instead of failing.
	stock := map[string]string{
		buildRuleCSourceID: "\t\tcompilerSpec = com.apple.compilers.llvm.clang.1_0;\n\t\tfileType = sourcecode.c;\n",
	}
	objects, target = buildRuleFixture(t, stock, []string{buildRuleCSourceID}, nil)
	rules, err = parseBuildRules(objects, target, map[string]string{"SRCROOT": "/src"})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := matchBuildRules(rules, []string{"/src/a.c"}, "/src")
	if err != nil {
		t.Fatalf("stock clang rule was rejected: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("stock clang rule produced %d script matches, want 0", len(matches))
	}
}

func TestRunBuildRulesExecutesScriptRule(t *testing.T) {
	root := t.TempDir()
	srcRoot := filepath.Join(root, "ios")
	workDir := filepath.Join(root, "build", "rules")
	proto := filepath.Join(srcRoot, "api", "en.lproj", "greeter.proto")
	mustWriteTestFile(t, proto, "message Greeting { string text = 1; }\n")
	mustWriteTestFile(t, filepath.Join(srcRoot, "extra", "template.txt"), "template\n")

	// The script asserts the rule environment itself: if any variable is wrong
	// the rule fails and the generated .c file is never written.
	script := `set -eu
test "$INPUT_FILE_PATH" = "$SCRIPT_INPUT_FILE"
test "$INPUT_FILE_NAME" = greeter.proto
test "$INPUT_FILE_BASE" = greeter
test "$INPUT_FILE_SUFFIX" = .proto
test "$INPUT_FILE_DIR" = "$(dirname "$INPUT_FILE_PATH")"
test "$INPUT_FILE_REGION_PATH_COMPONENT" = "en.lproj/"
test "$SCRIPT_OUTPUT_FILE_COUNT" = 3
test "$CURRENT_ARCH" = arm64
test "$arch" = arm64
test -f "$SCRIPT_INPUT_FILE_0"
test -d "$DERIVED_FILE_DIR"
test -d "$OBJECT_FILE_DIR"
test "$RIPLEY_RULE_BASE_ENV" = inherited
printf 'const char *greeting(void) { return "%s"; }\n' "$INPUT_FILE_BASE" > "$SCRIPT_OUTPUT_FILE_0"
printf 'const char *greeting(void);\n' > "$SCRIPT_OUTPUT_FILE_1"
printf 'generated for %s\n' "$INPUT_FILE_NAME" > "$SCRIPT_OUTPUT_FILE_2"
printf '%s: %s\n' "$SCRIPT_OUTPUT_FILE_0" "$INPUT_FILE_PATH" > "$DERIVED_FILE_DIR/$INPUT_FILE_BASE.d"
`
	bodies := map[string]string{
		buildRuleProtoID: fmt.Sprintf(`		compilerSpec = com.apple.compilers.proxy.script;
		dependencyFile = "$(DERIVED_FILE_DIR)/$(INPUT_FILE_BASE).d";
		filePatterns = "*.proto";
		fileType = pattern.proxy;
		inputFiles = (
			"$(SRCROOT)/extra/template.txt",
		);
		name = "Protobuf codegen";
		outputFiles = (
			"$(DERIVED_FILE_DIR)/$(INPUT_FILE_BASE).pb.c",
			"$(DERIVED_FILE_DIR)/$(INPUT_FILE_BASE).pb.h",
			"$(DERIVED_FILE_DIR)/$(INPUT_FILE_BASE).json",
		);
		runOncePerArchitecture = 1;
		script = %s;
`, pbxQuoteForTest(script)),
	}
	objects, target := buildRuleFixture(t, bodies, []string{buildRuleProtoID}, nil)
	env := map[string]string{
		"SRCROOT":              srcRoot,
		"ARCHS":                "arm64",
		"RIPLEY_RULE_BASE_ENV": "inherited",
		"PATH":                 os.Getenv("PATH"),
	}
	rules, err := parseBuildRules(objects, target, env)
	if err != nil {
		t.Fatal(err)
	}

	outputs, err := runBuildRules(rules, []string{proto, filepath.Join(srcRoot, "extra", "template.txt")}, env, workDir)
	if err != nil {
		t.Fatal(err)
	}

	derived := filepath.Join(workDir, "DerivedSources", "arm64")
	wantSource := filepath.Join(derived, "greeter.pb.c")
	if !equalStrings(outputs.Sources, []string{wantSource}) {
		t.Fatalf("Sources = %v, want [%s]", outputs.Sources, wantSource)
	}
	if want := []string{filepath.Join(derived, "greeter.pb.h")}; !equalStrings(outputs.Headers, want) {
		t.Fatalf("Headers = %v, want %v", outputs.Headers, want)
	}
	if want := []string{derived}; !equalStrings(outputs.HeaderSearchPaths, want) {
		t.Fatalf("HeaderSearchPaths = %v, want %v", outputs.HeaderSearchPaths, want)
	}
	if want := []string{filepath.Join(derived, "greeter.json")}; !equalStrings(outputs.Resources, want) {
		t.Fatalf("Resources = %v, want %v", outputs.Resources, want)
	}
	// Only the .proto was claimed; template.txt is a declared rule input, not a
	// rule-claimed source, so the caller must still compile it normally.
	if want := []string{proto}; !equalStrings(outputs.ClaimedInputs, want) {
		t.Fatalf("ClaimedInputs = %v, want %v", outputs.ClaimedInputs, want)
	}
	generated, err := os.ReadFile(wantSource)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(generated), `return "greeter"`) {
		t.Fatalf("generated source did not come from the rule: %q", generated)
	}

	// A second run with nothing touched is skipped via the recorded signature.
	if err := os.WriteFile(wantSource, []byte("// clobbered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runBuildRules(rules, []string{proto}, env, workDir); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(wantSource); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(data), "clobbered") {
		t.Error("up-to-date rule re-ran even though no input changed")
	}

	// Touching the input makes the rule out of date again.
	if err := os.WriteFile(proto, []byte("message Greeting { string text = 2; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runBuildRules(rules, []string{proto}, env, workDir); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(wantSource); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(data), "clobbered") {
		t.Error("rule did not re-run after its input changed")
	}
}

func TestRunBuildRulesReportsMissingOutputAndScriptFailure(t *testing.T) {
	root := t.TempDir()
	srcRoot := filepath.Join(root, "ios")
	input := filepath.Join(srcRoot, "gen", "thing.codegen")
	mustWriteTestFile(t, input, "x\n")
	env := map[string]string{"SRCROOT": srcRoot, "PATH": os.Getenv("PATH")}

	run := func(t *testing.T, script string) error {
		t.Helper()
		bodies := map[string]string{
			buildRuleProtoID: fmt.Sprintf("\t\tcompilerSpec = com.apple.compilers.proxy.script;\n\t\tfilePatterns = \"*.codegen\";\n\t\tfileType = pattern.proxy;\n\t\tname = Codegen;\n\t\toutputFiles = (\n\t\t\t\"$(DERIVED_FILE_DIR)/$(INPUT_FILE_BASE).c\",\n\t\t);\n\t\tscript = %s;\n", pbxQuoteForTest(script)),
		}
		objects, target := buildRuleFixture(t, bodies, []string{buildRuleProtoID}, nil)
		rules, err := parseBuildRules(objects, target, env)
		if err != nil {
			t.Fatal(err)
		}
		_, err = runBuildRules(rules, []string{input}, env, filepath.Join(t.TempDir(), "rules"))
		return err
	}

	err := run(t, "exit 3\n")
	if err == nil {
		t.Fatal("a failing rule script was reported as success")
	}
	if !strings.Contains(err.Error(), "Codegen") || !strings.Contains(err.Error(), "thing.codegen") {
		t.Errorf("failure error %q does not name the rule and the input", err)
	}

	err = run(t, "true\n")
	if err == nil {
		t.Fatal("a rule that produced none of its declared outputs was reported as success")
	}
	if !strings.Contains(err.Error(), "did not produce its declared output") {
		t.Errorf("missing-output error %q is not actionable", err)
	}
}

func TestRunBuildRulesRequiresDeclaredOutputs(t *testing.T) {
	root := t.TempDir()
	srcRoot := filepath.Join(root, "ios")
	input := filepath.Join(srcRoot, "a.proto")
	mustWriteTestFile(t, input, "x\n")

	bodies := map[string]string{
		buildRuleProtoID: "\t\tcompilerSpec = com.apple.compilers.proxy.script;\n\t\tfilePatterns = \"*.proto\";\n\t\tfileType = pattern.proxy;\n\t\tname = Silent;\n\t\tscript = \"true\\n\";\n",
	}
	objects, target := buildRuleFixture(t, bodies, []string{buildRuleProtoID}, nil)
	env := map[string]string{"SRCROOT": srcRoot, "PATH": os.Getenv("PATH")}
	rules, err := parseBuildRules(objects, target, env)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runBuildRules(rules, []string{input}, env, filepath.Join(root, "rules"))
	if err == nil {
		t.Fatal("a rule with no declared outputs was accepted")
	}
	if !strings.Contains(err.Error(), "declares no outputFiles") {
		t.Errorf("error %q does not explain the missing outputFiles", err)
	}
}

func TestRunBuildRulesRunsOncePerArchitecture(t *testing.T) {
	root := t.TempDir()
	srcRoot := filepath.Join(root, "ios")
	input := filepath.Join(srcRoot, "shader.gen")
	mustWriteTestFile(t, input, "x\n")

	script := "set -eu\nprintf 'int %s_marker;\\n' \"$CURRENT_ARCH\" > \"$SCRIPT_OUTPUT_FILE_0\"\n"
	bodies := map[string]string{
		buildRuleProtoID: fmt.Sprintf("\t\tcompilerSpec = com.apple.compilers.proxy.script;\n\t\tfilePatterns = \"*.gen\";\n\t\tfileType = pattern.proxy;\n\t\tname = PerArch;\n\t\toutputFiles = (\n\t\t\t\"$(DERIVED_FILE_DIR)/$(INPUT_FILE_BASE).c\",\n\t\t);\n\t\trunOncePerArchitecture = 1;\n\t\tscript = %s;\n", pbxQuoteForTest(script)),
	}
	objects, target := buildRuleFixture(t, bodies, []string{buildRuleProtoID}, nil)
	env := map[string]string{"SRCROOT": srcRoot, "ARCHS": "arm64 x86_64", "PATH": os.Getenv("PATH")}
	rules, err := parseBuildRules(objects, target, env)
	if err != nil {
		t.Fatal(err)
	}
	outputs, err := runBuildRules(rules, []string{input}, env, filepath.Join(root, "rules"))
	if err != nil {
		t.Fatal(err)
	}
	// One generated source per architecture, each in its own DERIVED_FILE_DIR.
	want := []string{
		filepath.Join(root, "rules", "DerivedSources", "arm64", "shader.c"),
		filepath.Join(root, "rules", "DerivedSources", "x86_64", "shader.c"),
	}
	if !equalStrings(outputs.Sources, want) {
		t.Fatalf("Sources = %v, want %v", outputs.Sources, want)
	}
	for _, arch := range []string{"arm64", "x86_64"} {
		data, err := os.ReadFile(filepath.Join(root, "rules", "DerivedSources", arch, "shader.c"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), arch+"_marker") {
			t.Errorf("%s output was not generated with CURRENT_ARCH=%s: %q", arch, arch, data)
		}
	}
}

// pbxQuoteForTest renders a Go string as a pbxproj double-quoted literal.
func pbxQuoteForTest(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
