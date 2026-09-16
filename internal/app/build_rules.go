package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"ripley/internal/ios"
)

// buildRuleScriptSpec is the compilerSpec Xcode writes for a custom
// "Run Script" build rule. Every other compilerSpec names one of Xcode's
// built-in compilers.
const buildRuleScriptSpec = "com.apple.compilers.proxy.script"

// buildRuleUndefinedArch is the CURRENT_ARCH value Xcode exports for a task that
// is not run once per architecture.
const buildRuleUndefinedArch = "undefined_arch"

// nativeBuildRuleSpecs are Xcode built-in compilerSpecs whose behaviour ripley's own
// pipeline already reproduces for the file types they claim, so a rule that only
// re-states the stock behaviour is left for the normal pipeline instead of being
// rejected. Anything outside this table is reported, never dropped: an
// Xcode-internal compiler ripley cannot run must fail loudly rather than drop files.
var nativeBuildRuleSpecs = map[string]bool{
	"com.apple.compilers.llvm.clang.1_0":      true, // clang: ripley compiles C/C++/Objective-C itself
	"com.apple.xcode.tools.swift.compiler":    true, // swiftc: ripley compiles Swift itself
	"com.apple.compilers.pbxcp":               true, // copy files: ripley copies project resources itself
	"com.apple.build-tasks.copy-plist-file":   true, // ripley rewrites and copies plists itself
	"com.apple.build-tasks.copy-strings-file": true, // ripley copies .strings itself
	"com.apple.compilers.assetcatalog":        true, // ripley compiles .xcassets itself
	"com.apple.xcode.tools.ibtool.compiler":   true, // ripley lowers storyboards/xibs itself
}

// buildRulePerFileVariables are resolved per matched input file when a rule runs.
// parseBuildRules must leave them untouched even when the project's build
// settings happen to define them, otherwise a rule's declared outputs would be
// frozen to the wrong directory at parse time.
var buildRulePerFileVariables = []string{
	"INPUT_FILE_PATH",
	"INPUT_FILE_NAME",
	"INPUT_FILE_BASE",
	"INPUT_FILE_SUFFIX",
	"INPUT_FILE_DIR",
	"INPUT_FILE_REGION_PATH_COMPONENT",
	"SCRIPT_INPUT_FILE",
	"DERIVED_FILE_DIR",
	"DERIVED_SOURCES_DIR",
	"OBJECT_FILE_DIR",
	"CURRENT_ARCH",
	"arch",
	"variant",
}

// buildRuleScope records where a rule was declared. Xcode searches a target's own
// rules after the project-wide ones, and the last match wins, so target rules
// override project rules for the same files.
type buildRuleScope int

const (
	buildRuleProjectScope buildRuleScope = iota
	buildRuleTargetScope
)

func (s buildRuleScope) String() string {
	if s == buildRuleTargetScope {
		return "target"
	}
	return "project"
}

// xcodeBuildRule is a PBXBuildRule with every path already expanded against the
// target's resolved build settings, except for the per-file variables that only
// have a value while the rule is running against one input file.
type xcodeBuildRule struct {
	ID                     string
	Name                   string
	Scope                  buildRuleScope
	Order                  int
	FileType               string
	FilePatterns           []string
	CompilerSpec           string
	Script                 string
	InputFiles             []string
	OutputFiles            []string
	InputFileListPaths     []string
	OutputFileListPaths    []string
	DependencyFile         string
	RunOncePerArchitecture bool
}

// IsScript reports whether the rule runs a shell script rather than one of
// Xcode's built-in compilers.
func (r xcodeBuildRule) IsScript() bool { return r.CompilerSpec == buildRuleScriptSpec }

// matchesPattern reports whether the rule selects files by glob rather than by
// file type. Xcode stores "Files matching pattern" rules as `pattern.proxy`.
func (r xcodeBuildRule) matchesPattern() bool {
	return strings.HasPrefix(r.FileType, "pattern.")
}

func (r xcodeBuildRule) describe() string {
	name := r.Name
	if name == "" {
		if r.matchesPattern() {
			name = strings.Join(r.FilePatterns, " ")
		} else {
			name = r.FileType
		}
	}
	return fmt.Sprintf("%s-level build rule %q", r.Scope, name)
}

// buildRuleOutputs is the classified result of running a target's custom build
// rules. Every path is absolute and cleaned, and each slice is deduplicated in
// first-seen order.
//
// The split is deliberate: the caller must feed Sources into the native compile,
// pass Objects straight to the linker, copy Resources into the bundle, and add
// HeaderSearchPaths to the compile so generated headers resolve. ClaimedInputs is
// exactly the set of input files a rule consumed and MUST be subtracted from the
// caller's own source discovery so nothing is compiled twice.
type buildRuleOutputs struct {
	Sources           []string
	Headers           []string
	HeaderSearchPaths []string
	Objects           []string
	Resources         []string
	ClaimedInputs     []string
}

func (o *buildRuleOutputs) merge(other buildRuleOutputs) {
	o.Sources = append(o.Sources, other.Sources...)
	o.Headers = append(o.Headers, other.Headers...)
	o.HeaderSearchPaths = append(o.HeaderSearchPaths, other.HeaderSearchPaths...)
	o.Objects = append(o.Objects, other.Objects...)
	o.Resources = append(o.Resources, other.Resources...)
	o.ClaimedInputs = append(o.ClaimedInputs, other.ClaimedInputs...)
}

func (o *buildRuleOutputs) dedupe() {
	for _, list := range []*[]string{&o.Sources, &o.Headers, &o.HeaderSearchPaths, &o.Objects, &o.Resources, &o.ClaimedInputs} {
		*list = uniquePaths(*list)
	}
}

// Empty reports whether the rules produced nothing and claimed nothing.
func (o buildRuleOutputs) Empty() bool {
	return len(o.Sources) == 0 && len(o.Headers) == 0 && len(o.Objects) == 0 &&
		len(o.Resources) == 0 && len(o.ClaimedInputs) == 0
}

// buildRuleMatch pairs an input file with the rule that claimed it.
type buildRuleMatch struct {
	Rule  xcodeBuildRule
	Input string
}

// projectBuildRules re-reads the project file and returns the custom build rules
// that apply to the resolved application target, in Xcode search order.
func projectBuildRules(config iosProject) ([]xcodeBuildRule, error) {
	pbxPath := filepath.Join(config.XcodeProject, "project.pbxproj")
	data, err := os.ReadFile(pbxPath)
	if err != nil {
		return nil, err
	}
	objects, err := parsePBXObjects(string(data))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", pbxPath, err)
	}
	target, err := selectApplicationTarget(config.XcodeProject, objects, config.Flavor)
	if err != nil {
		return nil, err
	}
	return parseBuildRules(objects, target, config.BuildSettings)
}

// parseBuildRules collects the PBXBuildRule objects reachable from the target,
// preceded by any declared on the PBXProject, and expands their declared paths
// against the resolved build settings. The returned order is Xcode's search
// order: later rules win, so a target rule overrides a project rule.
func parseBuildRules(objects map[string]pbxObject, target xcodeTarget, settings map[string]string) ([]xcodeBuildRule, error) {
	// Per-file variables must survive parse-time expansion untouched.
	deferred := make(map[string]string, len(settings))
	for key, value := range settings {
		deferred[key] = value
	}
	for _, name := range buildRulePerFileVariables {
		delete(deferred, name)
	}
	expand := func(value string) string {
		if value == "" {
			return ""
		}
		return expandApplicationScriptValue(value, deferred)
	}
	expandAll := func(values []string) []string {
		if len(values) == 0 {
			return nil
		}
		out := make([]string, 0, len(values))
		for _, value := range values {
			if expanded := strings.TrimSpace(expand(value)); expanded != "" {
				out = append(out, expanded)
			}
		}
		return out
	}

	var rules []xcodeBuildRule
	sources := []struct {
		scope buildRuleScope
		ids   []string
	}{
		{buildRuleProjectScope, projectScopeBuildRuleIDs(objects)},
		{buildRuleTargetScope, pbxObjectIDArrayField(objects[target.ID].Body, "buildRules")},
	}
	for _, source := range sources {
		for _, id := range source.ids {
			object, ok := objects[id]
			if !ok {
				return nil, fmt.Errorf("target %s references build rule %s, which is missing from the project file", target.Name, id)
			}
			if pbxField(object.Body, "isa") != "PBXBuildRule" {
				continue
			}
			name := firstNonEmpty(pbxQuotedField(object.Body, "name"), object.Comment)
			if name == "PBXBuildRule" {
				name = ""
			}
			rule := xcodeBuildRule{
				ID:                     id,
				Name:                   name,
				Scope:                  source.scope,
				Order:                  len(rules),
				FileType:               expand(pbxField(object.Body, "fileType")),
				FilePatterns:           strings.Fields(expand(pbxQuotedField(object.Body, "filePatterns"))),
				CompilerSpec:           pbxField(object.Body, "compilerSpec"),
				Script:                 pbxQuotedField(object.Body, "script"),
				InputFiles:             expandAll(pbxArrayField(object.Body, "inputFiles")),
				OutputFiles:            expandAll(pbxArrayField(object.Body, "outputFiles")),
				InputFileListPaths:     expandAll(pbxArrayField(object.Body, "inputFileListPaths")),
				OutputFileListPaths:    expandAll(pbxArrayField(object.Body, "outputFileListPaths")),
				DependencyFile:         expand(pbxQuotedField(object.Body, "dependencyFile")),
				RunOncePerArchitecture: pbxField(object.Body, "runOncePerArchitecture") == "1",
			}
			if rule.FileType == "" && len(rule.FilePatterns) == 0 {
				return nil, fmt.Errorf("target %s: %s declares neither fileType nor filePatterns", target.Name, rule.describe())
			}
			if rule.CompilerSpec == "" {
				return nil, fmt.Errorf("target %s: %s declares no compilerSpec", target.Name, rule.describe())
			}
			if rule.IsScript() && strings.TrimSpace(rule.Script) == "" {
				return nil, fmt.Errorf("target %s: %s is a script rule with an empty script", target.Name, rule.describe())
			}
			rules = append(rules, rule)
		}
	}
	return rules, nil
}

// projectScopeBuildRuleIDs returns build rules declared on the PBXProject object.
// Xcode's UI only edits target rules, but the file format allows project-wide
// ones and they are searched before the target's.
func projectScopeBuildRuleIDs(objects map[string]pbxObject) []string {
	for _, object := range objects {
		if pbxField(object.Body, "isa") == "PBXProject" {
			return pbxObjectIDArrayField(object.Body, "buildRules")
		}
	}
	return nil
}

// matchBuildRules assigns each source file to the rule that claims it. Xcode
// searches the rule list in order and the last match wins, which is why target
// rules (appended after project rules) override project rules.
//
// A file claimed by a rule whose compilerSpec is a built-in compiler ripley already
// reproduces is left unclaimed for the normal pipeline. Any other built-in
// compiler is an error: a rule that claims files is never silently ignored.
func matchBuildRules(rules []xcodeBuildRule, sources []string, srcRoot string) ([]buildRuleMatch, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	var matches []buildRuleMatch
	for _, source := range sources {
		path := absBuildRulePath(srcRoot, source)
		claimed := -1
		for index, rule := range rules {
			if buildRuleClaims(rule, path, srcRoot) {
				claimed = index
			}
		}
		if claimed < 0 {
			continue
		}
		rule := rules[claimed]
		if !rule.IsScript() {
			if nativeBuildRuleSpecs[rule.CompilerSpec] {
				continue
			}
			return nil, fmt.Errorf("%s claims %s (file type %s) with Xcode built-in compiler %q, which ripley cannot run on Linux; replace it with a script build rule or remove the rule",
				rule.describe(), filepath.Base(path), fileTypeForPath(path), rule.CompilerSpec)
		}
		matches = append(matches, buildRuleMatch{Rule: rule, Input: path})
	}
	return matches, nil
}

// buildRuleClaims reports whether the rule selects the given file, by glob for a
// `pattern.*` rule and by file-type conformance otherwise.
func buildRuleClaims(rule xcodeBuildRule, path, srcRoot string) bool {
	if rule.matchesPattern() {
		return matchesFilePatterns(rule.FilePatterns, path, srcRoot)
	}
	if rule.FileType == "" {
		return false
	}
	return fileTypeConforms(fileTypeForPath(path), rule.FileType)
}

// matchesFilePatterns applies Xcode's space-separated fnmatch list. A pattern
// without a slash matches the file name; one with a slash matches the path, both
// absolute and relative to SRCROOT.
func matchesFilePatterns(patterns []string, path, srcRoot string) bool {
	if len(patterns) == 0 {
		return false
	}
	candidates := []string{filepath.Base(path)}
	slashed := []string{filepath.ToSlash(path)}
	if srcRoot != "" {
		if relative, err := filepath.Rel(srcRoot, path); err == nil && !strings.HasPrefix(relative, "..") {
			slashed = append(slashed, filepath.ToSlash(relative))
		}
	}
	for _, pattern := range patterns {
		pattern = filepath.ToSlash(strings.Trim(pattern, `"`))
		if pattern == "" {
			continue
		}
		targets := candidates
		if strings.Contains(pattern, "/") {
			targets = slashed
		}
		for _, target := range targets {
			if ok, err := filepath.Match(pattern, target); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// fileTypeConforms implements the part of Apple's type hierarchy that build-rule
// matching needs: Xcode's identifiers are dotted refinements, so
// `sourcecode.c.objc` conforms to `sourcecode.c`, and `file` is the catch-all
// every file conforms to. Pattern rules never reach here; they match by glob.
func fileTypeConforms(fileType, ruleType string) bool {
	ruleType = strings.TrimSpace(ruleType)
	switch ruleType {
	case "":
		return false
	case "file", "*":
		return true
	}
	if fileType == "" {
		return false
	}
	return fileType == ruleType || strings.HasPrefix(fileType, ruleType+".")
}

// buildRuleFileTypes maps a file extension to the type identifier Xcode assigns
// it, which is what a rule's `fileType` is matched against.
var buildRuleFileTypes = map[string]string{
	".c":                "sourcecode.c.c",
	".h":                "sourcecode.c.h",
	".pch":              "sourcecode.c.h",
	".m":                "sourcecode.c.objc",
	".mm":               "sourcecode.cpp.objcpp",
	".cpp":              "sourcecode.cpp.cpp",
	".cc":               "sourcecode.cpp.cpp",
	".cxx":              "sourcecode.cpp.cpp",
	".c++":              "sourcecode.cpp.cpp",
	".hpp":              "sourcecode.cpp.h",
	".hh":               "sourcecode.cpp.h",
	".hxx":              "sourcecode.cpp.h",
	".swift":            "sourcecode.swift",
	".metal":            "sourcecode.metal",
	".s":                "sourcecode.asm",
	".asm":              "sourcecode.asm.asm",
	".y":                "sourcecode.yacc",
	".ypp":              "sourcecode.yacc",
	".yy":               "sourcecode.yacc",
	".l":                "sourcecode.lex",
	".lpp":              "sourcecode.lex",
	".ll":               "sourcecode.lex",
	".modulemap":        "sourcecode.module-map",
	".js":               "sourcecode.javascript",
	".proto":            "text.proto",
	".plist":            "text.plist.xml",
	".entitlements":     "text.plist.entitlements",
	".strings":          "text.plist.strings",
	".stringsdict":      "text.plist.stringsdict",
	".xcconfig":         "text.xcconfig",
	".json":             "text.json",
	".xml":              "text.xml",
	".yaml":             "text.yaml",
	".yml":              "text.yaml",
	".md":               "net.daringfireball.markdown",
	".txt":              "text",
	".sh":               "text.script.sh",
	".rb":               "text.script.ruby",
	".py":               "text.script.python",
	".xib":              "file.xib",
	".nib":              "wrapper.nib",
	".storyboard":       "file.storyboard",
	".storyboardc":      "wrapper.storyboardc",
	".xcassets":         "folder.assetcatalog",
	".xcdatamodel":      "wrapper.xcdatamodel",
	".xcdatamodeld":     "wrapper.xcdatamodeld",
	".intentdefinition": "file.intentdefinition",
	".lproj":            "folder",
	".bundle":           "wrapper.cfbundle",
	".framework":        "wrapper.framework",
	".xcframework":      "wrapper.xcframework",
	".a":                "archive.ar",
	".dylib":            "compiled.mach-o.dylib",
	".o":                "compiled.mach-o.objfile",
	".tbd":              "sourcecode.text-based-dylib-definition",
	".png":              "image.png",
	".jpg":              "image.jpeg",
	".jpeg":             "image.jpeg",
	".gif":              "image.gif",
	".pdf":              "image.pdf",
	".air":              "compiled.air",
	".metallib":         "archive.metal-library",
}

// fileTypeForPath returns Xcode's type identifier for a path, falling back to
// the generic `file` type that only a catch-all rule matches.
func fileTypeForPath(path string) string {
	if fileType, ok := buildRuleFileTypes[strings.ToLower(filepath.Ext(path))]; ok {
		return fileType
	}
	return "file"
}

// buildRuleSourceExtensions are generated outputs ripley compiles into the app
// binary.
var buildRuleSourceExtensions = map[string]bool{
	".c": true, ".m": true, ".mm": true, ".cpp": true, ".cc": true,
	".cxx": true, ".c++": true, ".swift": true, ".s": true, ".asm": true,
}

// buildRuleHeaderExtensions are generated outputs that only need to be visible
// to the compiler's header search path.
var buildRuleHeaderExtensions = map[string]bool{
	".h": true, ".hpp": true, ".hh": true, ".hxx": true, ".pch": true, ".modulemap": true, ".inc": true,
}

// buildRuleObjectExtensions are generated outputs handed straight to the linker.
var buildRuleObjectExtensions = map[string]bool{".o": true, ".a": true}

// runBuildRules executes every custom script build rule that claims one of the
// given sources and returns the classified generated files.
//
// env is the target's resolved build-setting environment (the same map the Xcode
// script phases run with); it is overlaid on the process environment, then the
// per-file rule variables are overlaid on top. Generated files always land under
// workDir, which the caller owns: DERIVED_FILE_DIR, DERIVED_SOURCES_DIR and
// OBJECT_FILE_DIR are pointed there regardless of what env says, so a rule's
// declared outputs cannot escape into the source tree.
func runBuildRules(rules []xcodeBuildRule, sources []string, env map[string]string, workDir string) (buildRuleOutputs, error) {
	var outputs buildRuleOutputs
	matches, err := matchBuildRules(rules, sources, env["SRCROOT"])
	if err != nil {
		return buildRuleOutputs{}, err
	}
	if len(matches) == 0 {
		return outputs, nil
	}
	shell, err := buildRuleShell()
	if err != nil {
		return buildRuleOutputs{}, err
	}
	archs := strings.Fields(env["ARCHS"])
	if len(archs) == 0 {
		archs = []string{"arm64"}
	}
	for index, match := range matches {
		runArchs := []string{buildRuleUndefinedArch}
		if match.Rule.RunOncePerArchitecture {
			runArchs = archs
		}
		for _, arch := range runArchs {
			produced, err := runBuildRule(shell, match, arch, env, workDir, index)
			if err != nil {
				return buildRuleOutputs{}, err
			}
			outputs.merge(produced)
		}
		outputs.ClaimedInputs = append(outputs.ClaimedInputs, match.Input)
	}
	outputs.dedupe()
	return outputs, nil
}

func runBuildRule(shell string, match buildRuleMatch, arch string, env map[string]string, workDir string, index int) (buildRuleOutputs, error) {
	rule := match.Rule
	input := match.Input
	if _, err := os.Stat(input); err != nil {
		return buildRuleOutputs{}, fmt.Errorf("%s input %s: %w", rule.describe(), input, err)
	}

	derivedSources := filepath.Join(workDir, "DerivedSources")
	derivedFiles := derivedSources
	if rule.RunOncePerArchitecture {
		derivedFiles = filepath.Join(derivedSources, arch)
	}
	objectDir := filepath.Join(workDir, "Objects")
	scriptDir := filepath.Join(workDir, "rules")
	for _, dir := range []string{derivedSources, derivedFiles, objectDir, scriptDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return buildRuleOutputs{}, err
		}
	}

	values := buildRuleValues(env, rule, input, arch, derivedFiles, derivedSources, objectDir)
	expand := func(value string) string { return expandApplicationScriptValue(value, values) }
	srcRoot := values["SRCROOT"]

	declaredOutputs := make([]string, 0, len(rule.OutputFiles))
	for _, output := range rule.OutputFiles {
		declaredOutputs = append(declaredOutputs, absBuildRulePath(srcRoot, expand(output)))
	}
	inputs := []string{input}
	for _, extra := range rule.InputFiles {
		inputs = append(inputs, absBuildRulePath(srcRoot, expand(extra)))
	}
	for _, list := range rule.InputFileListPaths {
		list = absBuildRulePath(srcRoot, expand(list))
		inputs = append(inputs, list)
		entries, err := ios.ReadScriptFileList(list, expand)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return buildRuleOutputs{}, err
		}
		for _, entry := range entries {
			inputs = append(inputs, absBuildRulePath(srcRoot, entry))
		}
	}
	for _, list := range rule.OutputFileListPaths {
		list = absBuildRulePath(srcRoot, expand(list))
		inputs = append(inputs, list)
		entries, err := ios.ReadScriptFileList(list, expand)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return buildRuleOutputs{}, err
		}
		for _, entry := range entries {
			declaredOutputs = append(declaredOutputs, absBuildRulePath(srcRoot, entry))
		}
	}
	declaredOutputs = uniquePaths(declaredOutputs)
	if len(declaredOutputs) == 0 {
		return buildRuleOutputs{}, fmt.Errorf("%s claims %s but declares no outputFiles, so nothing it generates can enter the build; add the generated files to the rule's output files",
			rule.describe(), filepath.Base(input))
	}
	// Xcode exports the resolved output list to the script, so it must be
	// visible under the indexed variables the script actually reads.
	values["SCRIPT_OUTPUT_FILE_COUNT"] = strconv.Itoa(len(declaredOutputs))
	for position, output := range declaredOutputs {
		values[fmt.Sprintf("SCRIPT_OUTPUT_FILE_%d", position)] = output
	}
	for _, output := range declaredOutputs {
		if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
			return buildRuleOutputs{}, err
		}
	}

	label := fmt.Sprintf("%03d-%s-%s-%s.sh", index, safeScriptName(ruleScriptLabel(rule)), safeScriptName(filepath.Base(input)), safeScriptName(arch))
	scriptFile := filepath.Join(scriptDir, label)
	if err := os.WriteFile(scriptFile, []byte(rule.Script+"\n"), 0o755); err != nil {
		return buildRuleOutputs{}, err
	}

	phase := ios.ScriptPhaseIO{
		Name:           fmt.Sprintf("%s (%s)", rule.describe(), filepath.Base(input)),
		Script:         rule.Script,
		ShellPath:      shell,
		Inputs:         uniquePaths(inputs),
		Outputs:        declaredOutputs,
		DependencyFile: absBuildRulePath(srcRoot, expand(rule.DependencyFile)),
		StateFile:      scriptFile + ".state",
		SearchPath:     values["PATH"],
	}
	if rule.DependencyFile == "" {
		phase.DependencyFile = ""
	}
	upToDate, err := ios.ScriptPhaseUpToDate(phase)
	if err != nil {
		return buildRuleOutputs{}, err
	}
	if !upToDate {
		fmt.Printf("  rule %s: %s\n", ruleScriptLabel(rule), filepath.Base(input))
		cmd := exec.Command(shell, scriptFile)
		cmd.Dir = firstNonEmpty(srcRoot, workDir)
		cmd.Env = buildRuleEnviron(values)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			err = ios.AnnotateScriptPhaseError(err, rule.Script, values["PATH"])
			return buildRuleOutputs{}, fmt.Errorf("%s failed for %s: %w", rule.describe(), input, err)
		}
	}

	produced, err := classifyBuildRuleOutputs(rule, input, declaredOutputs)
	if err != nil {
		return buildRuleOutputs{}, err
	}
	if !upToDate {
		if err := ios.RecordScriptPhaseState(phase); err != nil {
			return buildRuleOutputs{}, err
		}
	}
	return produced, nil
}

// classifyBuildRuleOutputs verifies every declared output was produced and sorts
// it into the bucket the caller has to act on.
func classifyBuildRuleOutputs(rule xcodeBuildRule, input string, outputs []string) (buildRuleOutputs, error) {
	var produced buildRuleOutputs
	for _, output := range outputs {
		if _, err := os.Stat(output); err != nil {
			if os.IsNotExist(err) {
				return buildRuleOutputs{}, fmt.Errorf("%s ran for %s but did not produce its declared output %s", rule.describe(), filepath.Base(input), output)
			}
			return buildRuleOutputs{}, err
		}
		extension := strings.ToLower(filepath.Ext(output))
		switch {
		case buildRuleSourceExtensions[extension]:
			produced.Sources = append(produced.Sources, output)
		case buildRuleHeaderExtensions[extension]:
			produced.Headers = append(produced.Headers, output)
			produced.HeaderSearchPaths = append(produced.HeaderSearchPaths, filepath.Dir(output))
		case buildRuleObjectExtensions[extension]:
			produced.Objects = append(produced.Objects, output)
		case extension == ".metal":
			return buildRuleOutputs{}, fmt.Errorf("%s generated the Metal shader %s, and compiling Metal requires Apple's metal compiler, which does not exist outside Xcode; have the rule emit a compiled .metallib instead",
				rule.describe(), output)
		case extension == ".d":
			// Dependency files describe the run, they are not build inputs.
		default:
			produced.Resources = append(produced.Resources, output)
		}
	}
	return produced, nil
}

// buildRuleValues layers the per-file rule variables Xcode exports on top of the
// target's build-setting environment.
func buildRuleValues(env map[string]string, rule xcodeBuildRule, input, arch, derivedFileDir, derivedSourcesDir, objectFileDir string) map[string]string {
	values := make(map[string]string, len(env)+32)
	for key, value := range env {
		values[key] = value
	}
	name := filepath.Base(input)
	suffix := filepath.Ext(name)
	values["INPUT_FILE_PATH"] = input
	values["INPUT_FILE_NAME"] = name
	values["INPUT_FILE_BASE"] = strings.TrimSuffix(name, suffix)
	values["INPUT_FILE_SUFFIX"] = suffix
	values["INPUT_FILE_DIR"] = filepath.Dir(input)
	values["INPUT_FILE_REGION_PATH_COMPONENT"] = inputFileRegionPathComponent(input)
	values["SCRIPT_INPUT_FILE"] = input
	values["DERIVED_FILE_DIR"] = derivedFileDir
	values["DERIVED_SOURCES_DIR"] = derivedSourcesDir
	values["DERIVED_FILES_DIR"] = derivedFileDir
	values["OBJECT_FILE_DIR"] = objectFileDir
	values["OBJECT_FILE_DIR_normal"] = objectFileDir + "-normal"
	values["CURRENT_ARCH"] = arch
	values["arch"] = arch
	values["CURRENT_VARIANT"] = "normal"
	values["variant"] = "normal"
	values["BUILD_RULE_NAME"] = ruleScriptLabel(rule)
	values["SCRIPT_INPUT_FILE_COUNT"] = strconv.Itoa(len(rule.InputFiles))
	for position, path := range rule.InputFiles {
		values[fmt.Sprintf("SCRIPT_INPUT_FILE_%d", position)] = expandApplicationScriptValue(path, values)
	}
	values["SCRIPT_INPUT_FILE_LIST_COUNT"] = strconv.Itoa(len(rule.InputFileListPaths))
	for position, path := range rule.InputFileListPaths {
		values[fmt.Sprintf("SCRIPT_INPUT_FILE_LIST_%d", position)] = expandApplicationScriptValue(path, values)
	}
	values["SCRIPT_OUTPUT_FILE_LIST_COUNT"] = strconv.Itoa(len(rule.OutputFileListPaths))
	for position, path := range rule.OutputFileListPaths {
		values[fmt.Sprintf("SCRIPT_OUTPUT_FILE_LIST_%d", position)] = expandApplicationScriptValue(path, values)
	}
	return values
}

// inputFileRegionPathComponent is Xcode's "en.lproj/" component for a localized
// input file, and empty for everything else.
func inputFileRegionPathComponent(input string) string {
	parent := filepath.Base(filepath.Dir(input))
	if strings.HasSuffix(parent, ".lproj") {
		return parent + "/"
	}
	return ""
}

func ruleScriptLabel(rule xcodeBuildRule) string {
	if rule.Name != "" {
		return rule.Name
	}
	if rule.matchesPattern() && len(rule.FilePatterns) > 0 {
		return strings.Join(rule.FilePatterns, " ")
	}
	if rule.FileType != "" {
		return rule.FileType
	}
	return "PBXBuildRule"
}

// buildRuleEnviron merges the resolved values over the process environment.
func buildRuleEnviron(values map[string]string) []string {
	base := make(map[string]string, len(values)+64)
	for _, entry := range os.Environ() {
		if index := strings.IndexByte(entry, '='); index >= 0 {
			base[entry[:index]] = entry[index+1:]
		}
	}
	for key, value := range values {
		base[key] = value
	}
	keys := make([]string, 0, len(base))
	for key := range base {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+base[key])
	}
	return env
}

// buildRuleShell resolves the shell Xcode runs rule scripts with.
func buildRuleShell() (string, error) {
	if _, err := os.Stat("/bin/sh"); err == nil {
		return "/bin/sh", nil
	}
	path, err := exec.LookPath("sh")
	if err != nil {
		return "", errors.New("custom Xcode build rules need a POSIX shell, but no /bin/sh or sh was found on this host")
	}
	return path, nil
}

func absBuildRulePath(root, value string) string {
	value = strings.TrimSpace(strings.Trim(value, `"`))
	if value == "" {
		return ""
	}
	value = filepath.FromSlash(value)
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	if root == "" {
		absolute, err := filepath.Abs(value)
		if err != nil {
			return filepath.Clean(value)
		}
		return absolute
	}
	return filepath.Clean(filepath.Join(root, value))
}

func uniquePaths(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
