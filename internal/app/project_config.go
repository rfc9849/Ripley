package app

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"ripley/internal/ios"

	"gopkg.in/yaml.v3"
)

type xcodeShellScriptPhase struct {
	Name                               string
	ShellPath                          string
	Script                             string
	InputPaths                         []string
	OutputPaths                        []string
	InputFileListPaths                 []string
	OutputFileListPaths                []string
	Order                              int
	BeforeSources                      bool
	AfterFlutterBuild                  bool
	DependencyFile                     string
	AlwaysOutOfDate                    bool
	RunOnlyForDeploymentPostprocessing bool
}

type iosProject struct {
	Root              string
	IOSRoot           string
	XcodeProject      string
	TargetName        string
	ProductName       string
	ExecutableName    string
	ModuleName        string
	BundleID          string
	OriginalBundleID  string
	MinOS             string
	Configuration     string
	Flavor            string
	InfoPlist         string
	Entitlements      string
	SourceDir         string
	BridgingHeader    string
	FlutterRoot       string
	FlutterTarget     string
	BuildName         string
	BuildNumber       string
	BuildSettings     map[string]string
	ShellScriptPhases []xcodeShellScriptPhase
	// SyncedSources holds absolute paths contributed by Xcode 16
	// PBXFileSystemSynchronizedRootGroup folder-synced groups, which list no
	// per-file PBXFileReference and are therefore invisible to the legacy
	// Sources build phase.
	SyncedSources []string
	// SourceFiles holds absolute paths listed one PBXBuildFile at a time by the
	// target's legacy PBXSourcesBuildPhase, including sources nested in
	// subdirectories of the target group.
	SourceFiles []string
}

type pbxObject struct {
	ID      string
	Comment string
	Body    string
}

type xcodeTarget struct {
	ID                     string
	Name                   string
	BuildConfigurationList string
	BuildPhases            []string
}

type xcodeBuildConfiguration struct {
	ID                string
	Name              string
	BaseConfiguration string
	Settings          map[string]string
}

var (
	pbxObjectStartRE = regexp.MustCompile(`(?m)^\s*([A-Fa-f0-9]{24}) /\* (.*?) \*/ = \{`)
	buildVariableRE  = regexp.MustCompile(`\$\(([^)]+)\)|\$\{([^}]+)\}`)
)

func applyBundleIDOverride(config *iosProject, bundleID string) {
	bundleID = strings.TrimSpace(bundleID)
	if bundleID == "" || bundleID == config.BundleID {
		return
	}
	if config.OriginalBundleID == "" {
		config.OriginalBundleID = config.BundleID
	}
	config.BundleID = bundleID
	if config.BuildSettings != nil {
		config.BuildSettings["PRODUCT_BUNDLE_IDENTIFIER"] = bundleID
	}
}

func overriddenChildBundleID(config iosProject, child string) string {
	original := strings.TrimSpace(config.OriginalBundleID)
	effective := strings.TrimSpace(config.BundleID)
	if original == "" || effective == "" || original == effective {
		return child
	}
	if child == original {
		return effective
	}
	if strings.HasPrefix(child, original+".") {
		return effective + strings.TrimPrefix(child, original)
	}
	return child
}

func resolveIOSProject(root, mode, flavor, flutterTargetOverride string) (iosProject, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return iosProject{}, err
	}
	iosRoot := filepath.Join(abs, "ios")
	projectPath, err := findXcodeProject(iosRoot)
	if err != nil {
		return iosProject{}, err
	}
	pbxPath := filepath.Join(projectPath, "project.pbxproj")
	data, err := os.ReadFile(pbxPath)
	if err != nil {
		return iosProject{}, err
	}
	objects, err := parsePBXObjects(string(data))
	if err != nil {
		return iosProject{}, fmt.Errorf("parse %s: %w", pbxPath, err)
	}

	target, err := selectApplicationTarget(projectPath, objects, flavor)
	if err != nil {
		return iosProject{}, err
	}
	configs, err := configurationsForList(objects, target.BuildConfigurationList)
	if err != nil {
		return iosProject{}, err
	}
	configuration, err := selectBuildConfiguration(configs, mode, flavor)
	if err != nil {
		return iosProject{}, fmt.Errorf("target %s: %w", target.Name, err)
	}

	buildName, buildNumber, err := readPubspecVersion(abs)
	if err != nil {
		return iosProject{}, err
	}
	settings := map[string]string{
		"SRCROOT":                 iosRoot,
		"SOURCE_ROOT":             iosRoot,
		"PROJECT_DIR":             iosRoot,
		"PROJECT_NAME":            strings.TrimSuffix(filepath.Base(projectPath), ".xcodeproj"),
		"TARGET_NAME":             target.Name,
		"CONFIGURATION":           configuration.Name,
		"SDKROOT":                 "iphoneos",
		"PLATFORM_NAME":           "iphoneos",
		"EFFECTIVE_PLATFORM_NAME": "-iphoneos",
		"FLUTTER_BUILD_NAME":      buildName,
		"FLUTTER_BUILD_NUMBER":    buildNumber,
	}
	if language := projectDevelopmentRegion(objects); language != "" {
		settings["DEVELOPMENT_LANGUAGE"] = language
	}

	if projectConfig, ok, err := projectBuildConfiguration(objects, configuration.Name, mode); err != nil {
		return iosProject{}, err
	} else if ok {
		if err := applyBuildConfiguration(settings, iosRoot, objects, projectConfig); err != nil {
			return iosProject{}, err
		}
	}
	if err := applyBuildConfiguration(settings, iosRoot, objects, configuration); err != nil {
		return iosProject{}, err
	}
	expandAllBuildSettings(settings)

	productName := firstNonEmpty(settings["PRODUCT_NAME"], target.Name)
	settings["PRODUCT_NAME"] = productName
	executableName := firstNonEmpty(settings["EXECUTABLE_NAME"], productName)
	settings["EXECUTABLE_NAME"] = executableName
	moduleName := firstNonEmpty(settings["PRODUCT_MODULE_NAME"], xcodeModuleName(productName))
	settings["PRODUCT_MODULE_NAME"] = moduleName
	expandAllBuildSettings(settings)

	bundleID := strings.TrimSpace(settings["PRODUCT_BUNDLE_IDENTIFIER"])
	if bundleID == "" {
		return iosProject{}, fmt.Errorf("target %s configuration %s has no PRODUCT_BUNDLE_IDENTIFIER", target.Name, configuration.Name)
	}
	minOS := strings.TrimSpace(settings["IPHONEOS_DEPLOYMENT_TARGET"])
	if minOS == "" {
		return iosProject{}, fmt.Errorf("target %s configuration %s has no IPHONEOS_DEPLOYMENT_TARGET", target.Name, configuration.Name)
	}
	infoSetting := strings.TrimSpace(settings["INFOPLIST_FILE"])
	if infoSetting == "" {
		if strings.EqualFold(settings["GENERATE_INFOPLIST_FILE"], "YES") {
			return iosProject{}, fmt.Errorf("target %s uses GENERATE_INFOPLIST_FILE; ripley currently requires the Flutter project's concrete Info.plist", target.Name)
		}
		return iosProject{}, fmt.Errorf("target %s configuration %s has no INFOPLIST_FILE", target.Name, configuration.Name)
	}
	infoPlist := resolveProjectPath(iosRoot, expandBuildValue(infoSetting, settings))
	if _, err := os.Stat(infoPlist); err != nil {
		return iosProject{}, fmt.Errorf("Info.plist %s: %w", infoPlist, err)
	}

	entitlements := ""
	if value := strings.TrimSpace(settings["CODE_SIGN_ENTITLEMENTS"]); value != "" {
		entitlements = resolveProjectPath(iosRoot, expandBuildValue(value, settings))
		if _, err := os.Stat(entitlements); err != nil {
			return iosProject{}, fmt.Errorf("entitlements %s: %w", entitlements, err)
		}
	}
	bridgingHeader := ""
	if value := strings.TrimSpace(settings["SWIFT_OBJC_BRIDGING_HEADER"]); value != "" {
		bridgingHeader = resolveProjectPath(iosRoot, expandBuildValue(value, settings))
	}

	flutterRoot, err := findProjectFlutterRoot(abs, settings)
	if err != nil {
		return iosProject{}, err
	}
	flutterTargetSetting := strings.TrimSpace(flutterTargetOverride)
	if flutterTargetSetting == "" {
		flutterTargetSetting = strings.TrimSpace(settings["FLUTTER_TARGET"])
	}
	if flutterTargetSetting == "" {
		flutterTargetSetting = filepath.Join("lib", "main.dart")
	}
	flutterTarget := expandHome(flutterTargetSetting)
	if !filepath.IsAbs(flutterTarget) {
		flutterTarget = filepath.Join(abs, filepath.FromSlash(flutterTarget))
	}
	flutterTarget = filepath.Clean(flutterTarget)
	info, err := os.Stat(flutterTarget)
	if err != nil {
		return iosProject{}, fmt.Errorf("Flutter target %s: %w", flutterTarget, err)
	}
	if info.IsDir() {
		return iosProject{}, fmt.Errorf("Flutter target %s is a directory", flutterTarget)
	}
	// Keep shell phases and the frontend server on the same target. Prefer a
	// project-relative spelling so remote builds can forward the setting without
	// leaking the caller's absolute workspace path.
	settings["FLUTTER_TARGET"] = flutterTarget
	if rel, relErr := filepath.Rel(abs, flutterTarget); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		settings["FLUTTER_TARGET"] = filepath.ToSlash(rel)
	}

	resolvedBuildName := firstNonEmpty(strings.TrimSpace(settings["FLUTTER_BUILD_NAME"]), buildName)
	resolvedBuildNumber := firstNonEmpty(strings.TrimSpace(settings["FLUTTER_BUILD_NUMBER"]), buildNumber)

	sourceDir, err := resolveNativeSourceDir(iosRoot, infoPlist, bridgingHeader)
	if err != nil {
		return iosProject{}, err
	}
	syncedSources, err := targetSynchronizedSources(objects, target, iosRoot)
	if err != nil {
		return iosProject{}, err
	}
	sourceFiles, err := targetSourceFiles(objects, target, iosRoot)
	if err != nil {
		return iosProject{}, err
	}
	shellScriptPhases := applicationShellScriptPhases(target, objects, iosRoot)

	return iosProject{
		Root:              abs,
		IOSRoot:           iosRoot,
		XcodeProject:      projectPath,
		TargetName:        target.Name,
		ProductName:       productName,
		ExecutableName:    executableName,
		ModuleName:        moduleName,
		BundleID:          bundleID,
		OriginalBundleID:  bundleID,
		MinOS:             minOS,
		Configuration:     configuration.Name,
		Flavor:            flavor,
		InfoPlist:         infoPlist,
		SourceDir:         sourceDir,
		BridgingHeader:    bridgingHeader,
		FlutterRoot:       flutterRoot,
		FlutterTarget:     flutterTarget,
		BuildName:         resolvedBuildName,
		BuildNumber:       resolvedBuildNumber,
		BuildSettings:     settings,
		ShellScriptPhases: shellScriptPhases,
		SyncedSources:     syncedSources,
		SourceFiles:       sourceFiles,
	}, nil
}

func projectDevelopmentRegion(objects map[string]pbxObject) string {
	for _, object := range objects {
		if pbxField(object.Body, "isa") == "PBXProject" {
			return pbxField(object.Body, "developmentRegion")
		}
	}
	return ""
}

func resolveNativeSourceDir(iosRoot, infoPlist, bridgingHeader string) (string, error) {
	candidates := []string{filepath.Dir(infoPlist)}
	if bridgingHeader != "" {
		candidates = append([]string{filepath.Dir(bridgingHeader)}, candidates...)
	}
	seen := make(map[string]bool)
	for _, dir := range candidates {
		dir = filepath.Clean(dir)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if dirHasNativeSources(dir) {
			return dir, nil
		}
	}
	var appDelegateDirs []string
	err := filepath.WalkDir(iosRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "Pods" || d.Name() == ".symlinks" || d.Name() == "build" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if name == "AppDelegate.swift" || name == "AppDelegate.m" || name == "AppDelegate.mm" {
			appDelegateDirs = append(appDelegateDirs, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(appDelegateDirs) == 1 {
		return appDelegateDirs[0], nil
	}
	if len(appDelegateDirs) > 1 {
		return "", fmt.Errorf("multiple native app source directories found: %s", strings.Join(appDelegateDirs, ", "))
	}
	return filepath.Dir(infoPlist), nil
}

func dirHasNativeSources(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		switch filepath.Ext(entry.Name()) {
		case ".swift", ".m", ".mm":
			return true
		}
	}
	return false
}

func findXcodeProject(iosRoot string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(iosRoot, "*.xcodeproj"))
	if err != nil {
		return "", err
	}
	var projects []string
	for _, path := range matches {
		if fileExists(filepath.Join(path, "project.pbxproj")) {
			projects = append(projects, path)
		}
	}
	sort.Strings(projects)
	if len(projects) == 0 {
		return "", fmt.Errorf("no .xcodeproj found under %s", iosRoot)
	}
	if len(projects) > 1 {
		return "", fmt.Errorf("multiple .xcodeproj files found under %s: %s", iosRoot, strings.Join(projects, ", "))
	}
	return projects[0], nil
}

func parsePBXObjects(text string) (map[string]pbxObject, error) {
	objects := make(map[string]pbxObject)
	for _, loc := range pbxObjectStartRE.FindAllStringSubmatchIndex(text, -1) {
		startBrace := strings.IndexByte(text[loc[0]:loc[1]], '{') + loc[0]
		end, err := matchingBrace(text, startBrace)
		if err != nil {
			return nil, err
		}
		id := text[loc[2]:loc[3]]
		objects[id] = pbxObject{
			ID:      id,
			Comment: text[loc[4]:loc[5]],
			Body:    text[startBrace+1 : end],
		}
	}
	if len(objects) == 0 {
		return nil, errors.New("no PBX objects found")
	}
	return objects, nil
}

func matchingBrace(text string, open int) (int, error) {
	depth := 0
	inString := false
	escaped := false
	for i := open; i < len(text); i++ {
		c := text[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, errors.New("unterminated PBX object")
}

func pbxField(body, key string) string {
	re := regexp.MustCompile(`(?m)(?:^|;)\s*` + regexp.QuoteMeta(key) + `\s*=\s*([^;]+);`)
	match := re.FindStringSubmatch(body)
	if match == nil {
		return ""
	}
	return cleanPBXValue(match[1])
}

func pbxObjectIDField(body, key string) string {
	value := pbxField(body, key)
	if value == "" {
		return ""
	}
	return strings.Fields(value)[0]
}

func pbxArrayField(body, key string) []string {
	re := regexp.MustCompile(`(?s)(?:^|\n)\s*` + regexp.QuoteMeta(key) + `\s*=\s*\((.*?)\);`)
	match := re.FindStringSubmatch(body)
	if match == nil {
		return nil
	}
	block := match[1]
	var values []string
	var current strings.Builder
	inString := false
	escaped := false
	for _, r := range block {
		if inString {
			current.WriteRune(r)
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
				continue
			}
			if r == '"' {
				inString = false
			}
			continue
		}
		if r == '"' {
			inString = true
			current.WriteRune(r)
			continue
		}
		if r == ',' {
			if value := cleanPBXValue(current.String()); value != "" {
				values = append(values, value)
			}
			current.Reset()
			continue
		}
		current.WriteRune(r)
	}
	if value := cleanPBXValue(current.String()); value != "" {
		values = append(values, value)
	}
	return values
}

func pbxObjectIDArrayField(body, key string) []string {
	values := pbxArrayField(body, key)
	out := make([]string, 0, len(values))
	for _, value := range values {
		fields := strings.Fields(value)
		if len(fields) == 0 || len(fields[0]) != 24 {
			continue
		}
		valid := true
		for _, r := range fields[0] {
			if !((r >= '0' && r <= '9') || (r >= 'A' && r <= 'F') || (r >= 'a' && r <= 'f')) {
				valid = false
				break
			}
		}
		if valid {
			out = append(out, fields[0])
		}
	}
	return out
}

func pbxQuotedField(body, key string) string {
	re := regexp.MustCompile(`(?m)(?:^|;)\s*` + regexp.QuoteMeta(key) + `\s*=\s*("(?:\\.|[^"\\])*")\s*;`)
	match := re.FindStringSubmatch(body)
	if match == nil {
		return pbxField(body, key)
	}
	decoded, err := strconv.Unquote(match[1])
	if err != nil {
		return pbxField(body, key)
	}
	return decoded
}

// applicationShellScriptPhases records the target's run-script phases together
// with their position relative to native Sources and Flutter's own build phase.
// srcRoot is the directory $(SRCROOT) expands to, needed to follow a phase that
// delegates to a wrapper script in the project.
func applicationShellScriptPhases(target xcodeTarget, objects map[string]pbxObject, srcRoot string) []xcodeShellScriptPhase {
	sourcesIndex := len(target.BuildPhases)
	flutterBuildIndex := -1
	for index, id := range target.BuildPhases {
		object, ok := objects[id]
		if !ok {
			continue
		}
		if pbxField(object.Body, "isa") == "PBXSourcesBuildPhase" && sourcesIndex == len(target.BuildPhases) {
			sourcesIndex = index
		}
		if pbxField(object.Body, "isa") == "PBXShellScriptBuildPhase" {
			script := pbxQuotedField(object.Body, "shellScript")
			if ios.ScriptInvokesFlutterBackend(script, srcRoot, false) {
				flutterBuildIndex = index
			}
		}
	}
	var phases []xcodeShellScriptPhase
	for index, id := range target.BuildPhases {
		object, ok := objects[id]
		if !ok || pbxField(object.Body, "isa") != "PBXShellScriptBuildPhase" {
			continue
		}
		name := pbxField(object.Body, "name")
		if name == "" {
			name = object.Comment
		}
		phases = append(phases, xcodeShellScriptPhase{
			Name:                               name,
			ShellPath:                          firstNonEmpty(pbxField(object.Body, "shellPath"), "/bin/sh"),
			Script:                             pbxQuotedField(object.Body, "shellScript"),
			InputPaths:                         pbxArrayField(object.Body, "inputPaths"),
			OutputPaths:                        pbxArrayField(object.Body, "outputPaths"),
			InputFileListPaths:                 pbxArrayField(object.Body, "inputFileListPaths"),
			OutputFileListPaths:                pbxArrayField(object.Body, "outputFileListPaths"),
			Order:                              index,
			BeforeSources:                      index < sourcesIndex,
			AfterFlutterBuild:                  index < sourcesIndex && flutterBuildIndex >= 0 && index > flutterBuildIndex,
			DependencyFile:                     pbxQuotedField(object.Body, "dependencyFile"),
			AlwaysOutOfDate:                    pbxField(object.Body, "alwaysOutOfDate") == "1",
			RunOnlyForDeploymentPostprocessing: pbxField(object.Body, "runOnlyForDeploymentPostprocessing") == "1",
		})
	}
	return phases
}

// targetSourceFiles resolves a target's legacy PBXSourcesBuildPhase into the
// absolute paths of the files it compiles, in build-phase order. srcRoot is the
// directory $(SRCROOT) expands to, the one holding the .xcodeproj.
//
// This is the per-file counterpart of targetSynchronizedSources: a target lists
// its sources either one PBXBuildFile at a time here or by folder there, and an
// Xcode 16 project such as immich uses both at once. Only the phase's own file
// list is consulted, so a file no target compiles - a test, an extension source
// or a sample sitting in the same tree - stays out of the build, and a source in
// a subdirectory of the target group is found where a top-level glob of the
// target directory would miss it. A target with no Sources phase yields nil and
// no error.
func targetSourceFiles(objects map[string]pbxObject, target xcodeTarget, srcRoot string) ([]string, error) {
	phase := ""
	for _, id := range target.BuildPhases {
		object, ok := objects[id]
		if !ok {
			continue
		}
		if pbxEntryValue(pbxEntries(object.Body), "isa") == "PBXSourcesBuildPhase" {
			phase = id
			break
		}
	}
	if phase == "" {
		return nil, nil
	}
	absRoot, err := filepath.Abs(srcRoot)
	if err != nil {
		return nil, err
	}
	parents := pbxGroupParents(objects)

	var out []string
	seen := make(map[string]bool)
	for _, buildFileID := range pbxEntryIDs(pbxEntries(objects[phase].Body), "files") {
		buildFile, ok := objects[buildFileID]
		if !ok {
			return nil, fmt.Errorf("Sources build phase of target %s references missing PBXBuildFile %s", target.Name, buildFileID)
		}
		fileRef := pbxEntryID(pbxEntries(buildFile.Body), "fileRef")
		if fileRef == "" {
			// A Swift package product entry (productRef) compiles no local file.
			continue
		}
		paths, err := pbxBuildFileSources(objects, parents, fileRef, absRoot, nil)
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", target.Name, err)
		}
		for _, path := range paths {
			if seen[path] {
				continue
			}
			seen[path] = true
			out = append(out, path)
		}
	}
	return out, nil
}

// pbxBuildFileSources expands one build file's fileRef into absolute paths. A
// PBXFileReference contributes itself; a group reference contributes its
// children in order, which is how a PBXVariantGroup of localized files reaches
// the compiler as its members rather than as the group. A reference anchored
// outside the project tree contributes nothing.
func pbxBuildFileSources(objects map[string]pbxObject, parents map[string]string, id, absRoot string, stack map[string]bool) ([]string, error) {
	object, ok := objects[id]
	if !ok {
		return nil, fmt.Errorf("build file references missing object %s", id)
	}
	fields := pbxEntries(object.Body)
	switch pbxEntryValue(fields, "isa") {
	case "PBXFileReference":
		path, anchored, err := resolvePBXReferencePath(objects, parents, id, absRoot)
		if err != nil || !anchored {
			return nil, err
		}
		return []string{path}, nil
	case "PBXGroup", "PBXVariantGroup", "XCVersionGroup":
		if stack[id] {
			return nil, fmt.Errorf("group %s contains itself", id)
		}
		if stack == nil {
			stack = make(map[string]bool)
		}
		stack[id] = true
		defer delete(stack, id)
		var out []string
		for _, child := range pbxEntryIDs(fields, "children") {
			paths, err := pbxBuildFileSources(objects, parents, child, absRoot, stack)
			if err != nil {
				return nil, err
			}
			out = append(out, paths...)
		}
		return out, nil
	default:
		// PBXReferenceProxy and friends name another project's product, not a
		// file this target compiles from source.
		return nil, nil
	}
}

// resolvePBXReferencePath walks a file reference up its parent PBXGroup chain to
// an absolute path the way resolveSynchronizedGroupDir resolves a folder-synced
// group: every node contributes its path and its sourceTree decides where the
// chain stops. Reports false for a reference anchored to a build product or an
// SDK, which is never a file compiled from the project tree.
func resolvePBXReferencePath(objects map[string]pbxObject, parents map[string]string, id, absRoot string) (string, bool, error) {
	var segments []string
	seen := make(map[string]bool)
	for current := id; current != ""; current = parents[current] {
		if seen[current] {
			return "", false, fmt.Errorf("file reference %s has a cyclic parent chain", id)
		}
		seen[current] = true
		object, ok := objects[current]
		if !ok {
			return "", false, fmt.Errorf("file reference %s references missing group %s", id, current)
		}
		fields := pbxEntries(object.Body)
		if path := pbxEntryValue(fields, "path"); path != "" {
			segments = append(segments, filepath.FromSlash(path))
		}
		switch tree := pbxEntryValue(fields, "sourceTree"); tree {
		case "", "<group>":
			continue
		case "SOURCE_ROOT", "SRCROOT", "PROJECT_DIR":
			return joinReversed(absRoot, segments), true, nil
		case "<absolute>":
			return joinReversed("", segments), true, nil
		case "BUILT_PRODUCTS_DIR", "SDKROOT", "DEVELOPER_DIR":
			return "", false, nil
		default:
			return "", false, fmt.Errorf("file reference %s uses unsupported sourceTree %q", id, tree)
		}
	}
	return joinReversed(absRoot, segments), true, nil
}

func cleanPBXValue(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.Index(value, " /*"); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
		value = strings.ReplaceAll(value, `\"`, `"`)
		value = strings.ReplaceAll(value, `\\`, `\`)
	}
	return value
}

func selectApplicationTarget(projectPath string, objects map[string]pbxObject, flavor string) (xcodeTarget, error) {
	var targets []xcodeTarget
	for _, object := range objects {
		if pbxField(object.Body, "isa") != "PBXNativeTarget" || pbxField(object.Body, "productType") != "com.apple.product-type.application" {
			continue
		}
		targets = append(targets, xcodeTarget{
			ID:                     object.ID,
			Name:                   pbxField(object.Body, "name"),
			BuildConfigurationList: pbxObjectIDField(object.Body, "buildConfigurationList"),
			BuildPhases:            pbxObjectIDArrayField(object.Body, "buildPhases"),
		})
	}
	if len(targets) == 0 {
		return xcodeTarget{}, errors.New("Xcode project has no application target")
	}
	if flavor != "" {
		if targetID := sharedSchemeTargetID(projectPath, flavor); targetID != "" {
			for _, target := range targets {
				if target.ID == targetID {
					return target, nil
				}
			}
		}
	}
	if len(targets) == 1 {
		return targets[0], nil
	}
	var names []string
	for _, target := range targets {
		names = append(names, target.Name)
	}
	sort.Strings(names)
	return xcodeTarget{}, fmt.Errorf("multiple application targets found (%s); use a flavor whose shared Xcode scheme selects one", strings.Join(names, ", "))
}

func sharedSchemeTargetID(projectPath, flavor string) string {
	paths := []string{
		filepath.Join(projectPath, "xcshareddata", "xcschemes", flavor+".xcscheme"),
		filepath.Join(filepath.Dir(projectPath), filepath.Base(projectPath), "xcshareddata", "xcschemes", flavor+".xcscheme"),
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		re := regexp.MustCompile(`BlueprintIdentifier\s*=\s*"([A-Fa-f0-9]{24})"`)
		if match := re.FindStringSubmatch(string(data)); match != nil {
			return match[1]
		}
	}
	return ""
}

func configurationsForList(objects map[string]pbxObject, listID string) ([]xcodeBuildConfiguration, error) {
	list, ok := objects[listID]
	if !ok {
		return nil, fmt.Errorf("build configuration list %s not found", listID)
	}
	re := regexp.MustCompile(`(?s)buildConfigurations\s*=\s*\((.*?)\);`)
	match := re.FindStringSubmatch(list.Body)
	if match == nil {
		return nil, fmt.Errorf("build configuration list %s has no configurations", listID)
	}
	idRE := regexp.MustCompile(`([A-Fa-f0-9]{24})`)
	ids := idRE.FindAllString(match[1], -1)
	configs := make([]xcodeBuildConfiguration, 0, len(ids))
	for _, id := range ids {
		object, ok := objects[id]
		if !ok || pbxField(object.Body, "isa") != "XCBuildConfiguration" {
			continue
		}
		configs = append(configs, xcodeBuildConfiguration{
			ID:                id,
			Name:              pbxField(object.Body, "name"),
			BaseConfiguration: pbxObjectIDField(object.Body, "baseConfigurationReference"),
			Settings:          parsePBXBuildSettings(object.Body),
		})
	}
	return configs, nil
}

func selectBuildConfiguration(configs []xcodeBuildConfiguration, mode, flavor string) (xcodeBuildConfiguration, error) {
	base := strings.ToUpper(mode[:1]) + mode[1:]
	wanted := base
	if flavor != "" {
		wanted += "-" + flavor
	}
	for _, config := range configs {
		if config.Name == wanted {
			return config, nil
		}
	}
	if flavor == "" {
		var candidates []xcodeBuildConfiguration
		for _, config := range configs {
			if strings.HasPrefix(config.Name, base+"-") {
				candidates = append(candidates, config)
			}
		}
		if len(candidates) == 1 {
			return candidates[0], nil
		}
	}
	var names []string
	for _, config := range configs {
		names = append(names, config.Name)
	}
	sort.Strings(names)
	return xcodeBuildConfiguration{}, fmt.Errorf("configuration %q not found; available: %s", wanted, strings.Join(names, ", "))
}

func projectBuildConfiguration(objects map[string]pbxObject, wanted, mode string) (xcodeBuildConfiguration, bool, error) {
	for _, object := range objects {
		if pbxField(object.Body, "isa") != "PBXProject" {
			continue
		}
		listID := pbxObjectIDField(object.Body, "buildConfigurationList")
		configs, err := configurationsForList(objects, listID)
		if err != nil {
			return xcodeBuildConfiguration{}, false, err
		}
		for _, config := range configs {
			if config.Name == wanted {
				return config, true, nil
			}
		}
		base := strings.ToUpper(mode[:1]) + mode[1:]
		for _, config := range configs {
			if config.Name == base {
				return config, true, nil
			}
		}
		return xcodeBuildConfiguration{}, false, nil
	}
	return xcodeBuildConfiguration{}, false, errors.New("PBXProject object not found")
}

func parsePBXBuildSettings(body string) map[string]string {
	settings := make(map[string]string)
	index := strings.Index(body, "buildSettings = {")
	if index < 0 {
		return settings
	}
	open := index + strings.IndexByte(body[index:], '{')
	close, err := matchingBrace(body, open)
	if err != nil {
		return settings
	}
	block := body[open+1 : close]
	var statement strings.Builder
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if statement.Len() > 0 {
			statement.WriteByte(' ')
		}
		statement.WriteString(trimmed)
		if !strings.HasSuffix(trimmed, ";") {
			continue
		}
		text := strings.TrimSuffix(statement.String(), ";")
		statement.Reset()
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			continue
		}
		key = cleanPBXValue(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "(") {
			continue
		}
		applySetting(settings, key, cleanPBXValue(value), "=")
	}
	return settings
}

func applyBuildConfiguration(settings map[string]string, iosRoot string, objects map[string]pbxObject, config xcodeBuildConfiguration) error {
	if config.BaseConfiguration != "" {
		path, err := resolvePBXFileReference(iosRoot, objects, config.BaseConfiguration)
		if err != nil {
			return err
		}
		if path != "" {
			if err := loadXCConfig(path, settings, make(map[string]bool)); err != nil {
				return err
			}
		}
	}
	for key, value := range config.Settings {
		applySetting(settings, key, value, "=")
	}
	return nil
}

func resolvePBXFileReference(iosRoot string, objects map[string]pbxObject, id string) (string, error) {
	object, ok := objects[id]
	if !ok {
		return "", fmt.Errorf("base configuration file reference %s not found", id)
	}
	path := firstNonEmpty(pbxField(object.Body, "path"), pbxField(object.Body, "name"))
	if path == "" {
		return "", nil
	}
	direct := resolveProjectPath(iosRoot, path)
	if fileExists(direct) {
		return direct, nil
	}
	var matches []string
	err := filepath.WalkDir(iosRoot, func(candidate string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() && filepath.Base(candidate) == filepath.Base(path) {
			matches = append(matches, candidate)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("ambiguous xcconfig %q: %s", path, strings.Join(matches, ", "))
	}
	// `Pods/Target Support Files/...` is not authored by the project: CocoaPods
	// generates it during `pod install` and it is normally gitignored, so a fresh
	// clone never has it. ripley resolves the pod graph itself and supplies those
	// build settings from its own resolution, so the reference is satisfied
	// without the file. A project-authored xcconfig that is genuinely missing is
	// still an error, because nothing else can supply its settings.
	if isGeneratedPodSupportFile(path) {
		return "", nil
	}
	return "", fmt.Errorf("xcconfig %q referenced by Xcode project does not exist", path)
}

// isGeneratedPodSupportFile reports whether an xcconfig reference names a
// CocoaPods-generated target support file rather than a file the project ships.
func isGeneratedPodSupportFile(path string) bool {
	slashed := filepath.ToSlash(path)
	return strings.Contains(slashed, "Target Support Files/")
}

func loadXCConfig(path string, settings map[string]string, seen map[string]bool) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if seen[abs] {
		return nil
	}
	seen[abs] = true
	file, err := os.Open(abs)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var logical string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasSuffix(line, "\\") {
			logical += strings.TrimSuffix(line, "\\")
			continue
		}
		line = strings.TrimSpace(logical + line)
		logical = ""
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.HasPrefix(line, "#include") {
			optional := strings.HasPrefix(line, "#include?")
			start := strings.IndexByte(line, '"')
			end := strings.LastIndexByte(line, '"')
			if start < 0 || end <= start {
				return fmt.Errorf("%s: invalid include %q", abs, line)
			}
			include := expandBuildValue(line[start+1:end], settings)
			includePath := include
			if !filepath.IsAbs(includePath) {
				includePath = filepath.Join(filepath.Dir(abs), filepath.FromSlash(includePath))
			}
			if _, err := os.Stat(includePath); err != nil {
				if optional && errors.Is(err, os.ErrNotExist) {
					continue
				}
				return fmt.Errorf("%s includes %s: %w", abs, includePath, err)
			}
			if err := loadXCConfig(includePath, settings, seen); err != nil {
				return err
			}
			continue
		}
		if index := strings.Index(line, "//"); index >= 0 {
			line = strings.TrimSpace(line[:index])
		}
		if line == "" {
			continue
		}
		operator := "="
		index := strings.Index(line, "=")
		if index < 0 {
			continue
		}
		if index > 0 && (line[index-1] == '?' || line[index-1] == '+') {
			operator = line[index-1 : index+1]
			index--
		}
		key := strings.TrimSpace(line[:index])
		valueStart := index + len(operator)
		value := strings.TrimSpace(line[valueStart:])
		applySetting(settings, key, cleanPBXValue(value), operator)
	}
	return scanner.Err()
}

func applySetting(settings map[string]string, rawKey, value, operator string) {
	key := strings.TrimSpace(strings.Trim(rawKey, `"`))
	if bracket := strings.IndexByte(key, '['); bracket >= 0 {
		condition := key[bracket:]
		key = key[:bracket]
		if strings.Contains(condition, "sdk=") && !strings.Contains(condition, "iphoneos") {
			return
		}
	}
	value = strings.ReplaceAll(value, "$(inherited)", settings[key])
	value = strings.ReplaceAll(value, "${inherited}", settings[key])
	switch operator {
	case "?=":
		if _, exists := settings[key]; exists {
			return
		}
		settings[key] = value
	case "+=":
		if settings[key] == "" {
			settings[key] = value
		} else {
			settings[key] += " " + value
		}
	default:
		settings[key] = value
	}
}

func expandAllBuildSettings(settings map[string]string) {
	for iteration := 0; iteration < 12; iteration++ {
		changed := false
		for key, value := range settings {
			expanded := expandBuildValue(value, settings)
			if expanded != value {
				settings[key] = expanded
				changed = true
			}
		}
		if !changed {
			break
		}
	}
}

func expandBuildValue(value string, settings map[string]string) string {
	return buildVariableRE.ReplaceAllStringFunc(value, func(token string) string {
		match := buildVariableRE.FindStringSubmatch(token)
		name := match[1]
		if name == "" {
			name = match[2]
		}
		if replacement, ok := settings[name]; ok {
			return replacement
		}
		return token
	})
}

func resolveProjectPath(iosRoot, value string) string {
	value = strings.TrimSpace(strings.Trim(value, `"`))
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(iosRoot, filepath.FromSlash(value)))
}

func readPubspecVersion(root string) (string, string, error) {
	data, err := os.ReadFile(filepath.Join(root, "pubspec.yaml"))
	if err != nil {
		return "", "", err
	}
	var spec struct {
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return "", "", fmt.Errorf("parse pubspec.yaml: %w", err)
	}
	name, number, found := strings.Cut(strings.TrimSpace(spec.Version), "+")
	if name == "" {
		name = "1.0.0"
	}
	if !found || number == "" {
		number = "1"
	}
	return name, number, nil
}

func findProjectFlutterRoot(projectRoot string, settings map[string]string) (string, error) {
	packageConfig := findProjectPackageConfig(projectRoot)
	if data, err := os.ReadFile(packageConfig); err == nil {
		var config struct {
			Packages []struct {
				Name    string `json:"name"`
				RootURI string `json:"rootUri"`
			} `json:"packages"`
		}
		if json.Unmarshal(data, &config) == nil {
			for _, pkg := range config.Packages {
				if pkg.Name != "flutter" {
					continue
				}
				root, err := packageRootFromURI(packageConfig, pkg.RootURI)
				if err == nil {
					candidate := filepath.Clean(filepath.Join(root, "..", ".."))
					if fileExists(filepath.Join(candidate, "bin", "flutter")) || fileExists(filepath.Join(candidate, "bin", "cache", "flutter.version.json")) {
						return candidate, nil
					}
				}
			}
		}
	}
	if root := strings.TrimSpace(settings["FLUTTER_ROOT"]); root != "" {
		root = expandHome(root)
		if filepath.IsAbs(root) && (fileExists(filepath.Join(root, "bin", "flutter")) || fileExists(filepath.Join(root, "bin", "cache", "flutter.version.json"))) {
			return filepath.Clean(root), nil
		}
	}
	return findFlutterRoot()
}

func packageRootFromURI(configPath, rootURI string) (string, error) {
	u, err := url.Parse(rootURI)
	if err != nil {
		return "", err
	}
	if u.Scheme == "file" {
		path, err := url.PathUnescape(u.Path)
		if err != nil {
			return "", err
		}
		return filepath.Clean(filepath.FromSlash(path)), nil
	}
	decoded, err := url.PathUnescape(rootURI)
	if err != nil {
		return "", err
	}
	return filepath.Abs(filepath.Join(filepath.Dir(configPath), filepath.FromSlash(decoded)))
}

func xcodeModuleName(productName string) string {
	var b strings.Builder
	for _, r := range productName {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	name := b.String()
	if name == "" {
		return "App"
	}
	if name[0] >= '0' && name[0] <= '9' {
		return "_" + name
	}
	return name
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
