package ios

// Swift Package Manager support for iOS builds without Xcode.
//
// Two independent sources of SwiftPM usage are handled:
//
//  1. Flutter's plugin integration. `flutter build` generates
//     ios/Flutter/ephemeral/Packages/FlutterGeneratedPluginSwiftPackage, a local
//     package whose dependencies are symlinks (in ../.packages) to each plugin's
//     ios/<name> Swift package, plus a FlutterFramework package wrapping
//     Flutter.xcframework as a binaryTarget.
//  2. Packages the app itself references from project.pbxproj via
//     XCLocalSwiftPackageReference / XCRemoteSwiftPackageReference, consumed by
//     XCSwiftPackageProductDependency entries.
//
// Manifests are read with `swift package dump-package`, which is the only
// faithful way to evaluate Package.swift (it is Swift code, not data). Targets
// are then compiled directly with swiftc/clang using the same iPhoneOS SDK,
// resource-dir and compat-lib handling as the CocoaPods path, so the result is
// ordinary arm64 Mach-O objects the caller links into RipleyPods.framework or the
// app host. Nothing in this file requires macOS.
//
// Invariant: BuildSwiftPMPackages never partially degrades. Either every
// reachable target of every referenced package is compiled and reported, or an
// error naming the offending package/target/plugin is returned.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"ripley/internal/toolchain"
)

// SwiftPMBuild is the result of compiling every SwiftPM package a project uses.
type SwiftPMBuild struct {
	// Frameworks are vendored binary artifacts (.framework directories chosen
	// from a binaryTarget's xcframework) that must be linked and, when dynamic,
	// embedded.
	Frameworks []string
	// Objects are compiled object files and static archives, in link order:
	// dependencies before dependents.
	Objects []string
	// Resources are bundle directories to copy into the app bundle.
	Resources []string
	// ModuleSearchPaths are -I / -F directories exposing the compiled
	// .swiftmodule and generated ObjC headers to downstream compiles.
	ModuleSearchPaths []string
	// Packages are the resolved package root directories that were built.
	Packages []string
	// LinkerLibraries are `-l<name>` libraries requested by linkerSettings.
	LinkerLibraries []string
	// LinkerFrameworks are `-framework <name>` system frameworks requested by
	// linkerSettings.
	LinkerFrameworks []string
	// MacroPlugins are `<executable>#<ModuleName>` entries for swiftc's
	// -load-plugin-executable, one per `.macro` target in the resolved graph.
	// They must reach the *app host* compile as well as the package targets: an
	// app's own Swift sources expand macros a package declares (immich's
	// ios/Runner/Schemas/Tables.swift applies @Table, whose implementation lives
	// in the StructuredQueriesMacros plugin), and without the flag swiftc reports
	// "external macro implementation type ... could not be found" followed by an
	// error for every declaration the expansion would have produced.
	MacroPlugins []string
}

const (
	flutterGeneratedPluginSwiftPackageName = "FlutterGeneratedPluginSwiftPackage"
	flutterFrameworkSwiftPackageName       = "FlutterFramework"
)

// BuildSwiftPMPackages compiles every Swift package the iOS project depends on.
//
// projectRoot is the Flutter project root (containing ios/), sourceDir is the
// native app source directory (ios/Runner). minOS is the app's deployment
// target. When the project uses no Swift packages at all — the common case —
// this returns a zero SwiftPMBuild and a nil error after a few stat calls.
func BuildSwiftPMPackages(tc toolchain.Toolchain, projectRoot, sourceDir, minOS string) (SwiftPMBuild, error) {
	iosRoot, err := swiftPMIOSRoot(projectRoot, sourceDir)
	if err != nil {
		return SwiftPMBuild{}, err
	}
	refs, err := discoverSwiftPMReferences(iosRoot)
	if err != nil {
		return SwiftPMBuild{}, err
	}
	if len(refs) == 0 {
		return SwiftPMBuild{}, nil
	}

	pins, err := loadPackageResolved(iosRoot)
	if err != nil {
		return SwiftPMBuild{}, err
	}
	resolver := &swiftPMResolver{
		tc:     tc,
		pins:   pins,
		cache:  swiftPMCacheDir(),
		loaded: make(map[string]*swiftPMPackage),
	}
	var roots []swiftPMRootPackage
	for _, ref := range refs {
		pkg, err := resolver.load(ref)
		if err != nil {
			return SwiftPMBuild{}, err
		}
		roots = append(roots, swiftPMRootPackage{Package: pkg, Products: ref.Products})
	}

	graph, err := newSwiftPMGraph(roots, resolver)
	if err != nil {
		return SwiftPMBuild{}, err
	}
	work := filepath.Join(projectRoot, "build", "ripley_ios", "swiftpm")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return SwiftPMBuild{}, err
	}
	return compileSwiftPMGraph(tc, graph, work, minOS)
}

// flutterPluginSwiftPMBuild is the statically compiled part of Flutter's
// generated plugin Swift package. ObjCHeaderDirs expose the compatibility
// headers GeneratedPluginRegistrant expects as <plugin/PluginClass.h>.
type flutterPluginSwiftPMBuild struct {
	Build          SwiftPMBuild
	ObjCHeaderDirs []string
}

// flutterPluginSwiftPackageRoot returns the package root Flutter would put
// under Flutter/ephemeral/Packages/.packages. Modern plugins commonly keep the
// manifest one level below ios/ (ios/<plugin>/Package.swift), while hand-written
// plugins sometimes put it directly in ios/.
func flutterPluginSwiftPackageRoot(plugin Plugin) (string, error) {
	candidates := []string{
		filepath.Join(plugin.IOSDir, plugin.Name),
		plugin.IOSDir,
	}
	matches, err := filepath.Glob(filepath.Join(plugin.IOSDir, "*", "Package.swift"))
	if err != nil {
		return "", err
	}
	for _, match := range matches {
		candidates = append(candidates, filepath.Dir(match))
	}
	seen := make(map[string]bool)
	var found []string
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if seen[candidate] || !fileExists(filepath.Join(candidate, "Package.swift")) {
			continue
		}
		seen[candidate] = true
		found = append(found, candidate)
	}
	if len(found) == 0 {
		return "", nil
	}
	if len(found) == 1 {
		return found[0], nil
	}
	preferred := filepath.Join(plugin.IOSDir, plugin.Name)
	for _, candidate := range found {
		if candidate == preferred {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("plugin %s has multiple iOS Swift packages: %s", plugin.Name, strings.Join(found, ", "))
}

func flutterPluginProduct(pkg *swiftPMPackage, plugin Plugin) (swiftPMProduct, error) {
	preferred := []string{strings.ReplaceAll(plugin.Name, "_", "-"), plugin.Name}
	for _, name := range preferred {
		for _, product := range pkg.Manifest.Products {
			if product.Name == name {
				return product, nil
			}
		}
	}
	if len(pkg.Manifest.Products) == 1 {
		return pkg.Manifest.Products[0], nil
	}
	return swiftPMProduct{}, fmt.Errorf("Swift package plugin %s has no product matching %q (products: %v)", plugin.Name, preferred[0], pkg.Manifest.Products)
}

// buildFlutterPluginSwiftPMPackages compiles package-only Flutter plugins using
// the same SwiftPM graph as app packages. Flutter's generated
// FlutterGeneratedPluginSwiftPackage links these products statically; ripley keeps
// that model and returns raw objects/resources rather than inventing dylibs.
func buildFlutterPluginSwiftPMPackages(tc toolchain.Toolchain, projectRoot string, plugins []Plugin, minOS string) (flutterPluginSwiftPMBuild, error) {
	if len(plugins) == 0 {
		return flutterPluginSwiftPMBuild{}, nil
	}
	iosRoot := filepath.Join(projectRoot, "ios")
	pins, err := loadPackageResolved(iosRoot)
	if err != nil {
		return flutterPluginSwiftPMBuild{}, err
	}
	resolver := &swiftPMResolver{
		tc:     tc,
		pins:   pins,
		cache:  swiftPMCacheDir(),
		loaded: make(map[string]*swiftPMPackage),
	}
	type rootPlugin struct {
		Plugin  Plugin
		Package *swiftPMPackage
		Product swiftPMProduct
	}
	var roots []swiftPMRootPackage
	var mapped []rootPlugin
	for _, plugin := range plugins {
		root, err := flutterPluginSwiftPackageRoot(plugin)
		if err != nil {
			return flutterPluginSwiftPMBuild{}, err
		}
		if root == "" {
			return flutterPluginSwiftPMBuild{}, fmt.Errorf("plugin %s has no podspec and no iOS Package.swift", plugin.Name)
		}
		ref := swiftPMReference{Name: plugin.Name, Path: root}
		pkg, err := resolver.load(ref)
		if err != nil {
			return flutterPluginSwiftPMBuild{}, fmt.Errorf("evaluate Swift package plugin %s: %w", plugin.Name, err)
		}
		product, err := flutterPluginProduct(pkg, plugin)
		if err != nil {
			return flutterPluginSwiftPMBuild{}, err
		}
		roots = append(roots, swiftPMRootPackage{Package: pkg, Products: []string{product.Name}})
		mapped = append(mapped, rootPlugin{Plugin: plugin, Package: pkg, Product: product})
	}
	graph, err := newSwiftPMGraph(roots, resolver)
	if err != nil {
		return flutterPluginSwiftPMBuild{}, err
	}
	work := filepath.Join(projectRoot, "build", "ripley_ios", "plugin_swiftpm")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return flutterPluginSwiftPMBuild{}, err
	}
	build, err := compileSwiftPMGraph(tc, graph, work, minOS)
	if err != nil {
		return flutterPluginSwiftPMBuild{}, err
	}

	// Flutter's GeneratedPluginRegistrant uses the CocoaPods-compatible include
	// spelling even for SwiftPM-only plugins. Xcode synthesizes that compatibility
	// view. Recreate it from SwiftPM's emitted Objective-C header so the existing
	// generated registrant can remain byte-for-byte untouched.
	headerRoot := filepath.Join(work, "registrant-headers")
	if err := os.RemoveAll(headerRoot); err != nil {
		return flutterPluginSwiftPMBuild{}, err
	}
	if err := os.MkdirAll(headerRoot, 0o755); err != nil {
		return flutterPluginSwiftPMBuild{}, err
	}
	var headerDirs []string
	for _, item := range mapped {
		class := item.Plugin.PluginClass
		if class == "" {
			class = defaultPluginClass(item.Plugin.Name)
		}
		var generated string
		for _, targetName := range item.Product.Targets {
			module := swiftPMModuleName(targetName)
			candidate := filepath.Join(work, "targets", item.Package.Manifest.Name, targetName, "module", module+"-Swift.h")
			if fileExists(candidate) {
				generated = candidate
				break
			}
		}
		if generated == "" {
			return flutterPluginSwiftPMBuild{}, fmt.Errorf("Swift package plugin %s produced no Objective-C compatibility header for product %s", item.Plugin.Name, item.Product.Name)
		}
		dir := filepath.Join(headerRoot, item.Plugin.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return flutterPluginSwiftPMBuild{}, err
		}
		if err := replaceFile(generated, filepath.Join(dir, class+".h")); err != nil {
			return flutterPluginSwiftPMBuild{}, err
		}
		headerDirs = append(headerDirs, headerRoot)
	}
	return flutterPluginSwiftPMBuild{Build: build, ObjCHeaderDirs: uniqueStrings(headerDirs)}, nil
}

func swiftPMIOSRoot(projectRoot, sourceDir string) (string, error) {
	candidates := []string{filepath.Join(projectRoot, "ios")}
	if sourceDir != "" {
		candidates = append(candidates, filepath.Dir(sourceDir))
	}
	for _, candidate := range candidates {
		if dirExists(candidate) {
			return filepath.Abs(candidate)
		}
	}
	if sourceDir != "" {
		return "", fmt.Errorf("cannot locate iOS project root from %s / %s", projectRoot, sourceDir)
	}
	return "", fmt.Errorf("cannot locate iOS project root under %s", projectRoot)
}

func swiftPMCacheDir() string {
	return filepath.Join(toolchain.RipleyHome(), "swiftpm")
}

// swiftPMReference is a package the project asked for, before its manifest is read.
type swiftPMReference struct {
	// Name is the reference's display name; for remote references it is the
	// repository basename.
	Name string
	// Path is set for local references (absolute).
	Path string
	// URL is set for remote references.
	URL string
	// Requirement describes what pbxproj asked for, used only in error text: a
	// pin from Package.resolved is what actually gets checked out.
	Requirement string
	// Products are the product names the app target actually consumes from this
	// package (from XCSwiftPackageProductDependency). Empty means "everything
	// the package vends", which is what Flutter's generated package implies.
	Products []string
}

func (r swiftPMReference) identity() string {
	if r.URL != "" {
		return swiftPMIdentityFromURL(r.URL)
	}
	return strings.ToLower(filepath.Base(r.Path))
}

func swiftPMIdentityFromURL(url string) string {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(url), "/"), ".git")
	if idx := strings.LastIndexAny(trimmed, "/:"); idx >= 0 {
		trimmed = trimmed[idx+1:]
	}
	return strings.ToLower(strings.TrimSuffix(trimmed, ".git"))
}

// discoverSwiftPMReferences finds every package the iOS project references:
// Flutter's generated plugin package plus pbxproj local/remote references.
func discoverSwiftPMReferences(iosRoot string) ([]swiftPMReference, error) {
	var refs []swiftPMReference
	index := make(map[string]int)
	add := func(ref swiftPMReference) int {
		key := ref.Path + "\x00" + ref.URL
		if at, ok := index[key]; ok {
			return at
		}
		index[key] = len(refs)
		refs = append(refs, ref)
		return len(refs) - 1
	}

	generated := filepath.Join(iosRoot, "Flutter", "ephemeral", "Packages", flutterGeneratedPluginSwiftPackageName)
	if fileExists(filepath.Join(generated, "Package.swift")) {
		add(swiftPMReference{Name: flutterGeneratedPluginSwiftPackageName, Path: generated})
	}

	pbxproj, err := findSwiftPMPbxproj(iosRoot)
	if err != nil {
		return nil, err
	}
	if pbxproj == "" {
		return refs, nil
	}
	data, err := os.ReadFile(pbxproj)
	if err != nil {
		return nil, err
	}
	text := string(data)
	// Cheap early out: pbxproj files without SwiftPM sections are the norm.
	if !strings.Contains(text, "SwiftPackageReference") {
		return refs, nil
	}
	local, remote, products, err := parseSwiftPackageReferences(text)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", pbxproj, err)
	}
	// byObjectID maps a pbxproj package-reference object id to its refs index so
	// XCSwiftPackageProductDependency entries can name the products actually
	// consumed; anything not named is not built.
	byObjectID := make(map[string]int)
	for id, rel := range local {
		abs := rel
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(iosRoot, rel)
		}
		abs = filepath.Clean(abs)
		if !fileExists(filepath.Join(abs, "Package.swift")) {
			return nil, fmt.Errorf("%s references local Swift package %q but %s has no Package.swift; run `flutter pub get` (or `flutter build ios --config-only`) to generate it", pbxproj, rel, abs)
		}
		byObjectID[id] = add(swiftPMReference{Name: filepath.Base(abs), Path: abs})
	}
	for id, ref := range remote {
		byObjectID[id] = add(ref)
	}
	for _, product := range products {
		if product.PackageID == "" {
			// Xcode omits `package` for a product of a local package reference;
			// it is resolved by product name once manifests are read.
			continue
		}
		at, ok := byObjectID[product.PackageID]
		if !ok {
			return nil, fmt.Errorf("%s: XCSwiftPackageProductDependency %q references unknown package object %s", pbxproj, product.ProductName, product.PackageID)
		}
		refs[at].Products = append(refs[at].Products, product.ProductName)
	}
	for i := range refs {
		refs[i].Products = uniqueStrings(refs[i].Products)
	}
	return refs, nil
}

func findSwiftPMPbxproj(iosRoot string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(iosRoot, "*.xcodeproj", "project.pbxproj"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", nil
	}
	sort.Strings(matches)
	for _, match := range matches {
		if filepath.Base(filepath.Dir(match)) == "Runner.xcodeproj" {
			return match, nil
		}
	}
	return matches[0], nil
}

var swiftPMQuotedRe = regexp.MustCompile(`^"(.*)"$`)

// pbxSwiftPMObject is one pbxproj object: its id and its balanced-brace body.
type pbxSwiftPMObject struct {
	ID   string
	Body string
}

// pbxObjectBodies returns the id and balanced-brace body of every pbxproj object
// whose `isa` equals isaName.
//
// A regex cannot do this: an object body contains nested dictionaries (an
// XCRemoteSwiftPackageReference always nests `requirement = { ... }`), so a
// `[^}]*` scan truncates the object at the first inner brace and loses the very
// field that describes what was asked for.
func pbxObjectBodies(text, isaName string) []pbxSwiftPMObject {
	var objects []pbxSwiftPMObject
	needle := "isa"
	for offset := 0; ; {
		idx := strings.Index(text[offset:], needle)
		if idx < 0 {
			return objects
		}
		idx += offset
		offset = idx + len(needle)
		rest := strings.TrimLeft(text[offset:], " \t\n\r")
		if !strings.HasPrefix(rest, "=") {
			continue
		}
		value := strings.TrimLeft(rest[1:], " \t\n\r")
		if !strings.HasPrefix(value, isaName) {
			continue
		}
		after := value[len(isaName):]
		if after == "" || !strings.ContainsRune(";\n\r \t", rune(after[0])) {
			continue
		}
		open := pbxEnclosingBrace(text, idx)
		if open < 0 {
			continue
		}
		close := pbxMatchingBrace(text, open)
		if close < 0 {
			continue
		}
		objects = append(objects, pbxSwiftPMObject{ID: pbxObjectID(text, open), Body: text[open+1 : close]})
		offset = close
	}
}

// pbxObjectID reads the object identifier that precedes the '{' at open, i.e.
// the `ID` in `ID /* comment */ = {`.
func pbxObjectID(text string, open int) string {
	head := text[:open]
	if eq := strings.LastIndex(head, "="); eq >= 0 {
		head = head[:eq]
	}
	// Drop a trailing /* comment */.
	if end := strings.LastIndex(head, "*/"); end >= 0 {
		if start := strings.LastIndex(head[:end], "/*"); start >= 0 {
			head = head[:start]
		}
	}
	fields := strings.Fields(head)
	if len(fields) == 0 {
		return ""
	}
	return strings.Trim(fields[len(fields)-1], ";")
}

// pbxEnclosingBrace finds the '{' that opens the object containing position pos.
func pbxEnclosingBrace(text string, pos int) int {
	depth := 0
	for i := pos - 1; i >= 0; i-- {
		switch text[i] {
		case '}':
			depth++
		case '{':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

// pbxMatchingBrace finds the '}' closing the '{' at open, skipping quoted
// strings so a brace inside a string literal cannot unbalance the scan.
func pbxMatchingBrace(text string, open int) int {
	depth := 0
	for i := open; i < len(text); i++ {
		switch text[i] {
		case '"':
			for i++; i < len(text); i++ {
				if text[i] == '\\' {
					i++
					continue
				}
				if text[i] == '"' {
					break
				}
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// pbxSwiftPMProductDependency is an XCSwiftPackageProductDependency: the product
// an app target consumes, and the package-reference object it comes from.
type pbxSwiftPMProductDependency struct {
	PackageID   string
	ProductName string
}

// parseSwiftPackageReferences extracts XCLocalSwiftPackageReference relative
// paths, XCRemoteSwiftPackageReference repository URLs and the
// XCSwiftPackageProductDependency entries from pbxproj text, keyed by object id.
//
// The objects are order-independent and may appear inside or outside the
// dedicated "Begin XCLocalSwiftPackageReference section" markers (Xcode emits
// the sections, but hand-edited projects and older Xcode versions do not always
// use them), so the whole file is scanned by isa.
func parseSwiftPackageReferences(text string) (localPaths map[string]string, remote map[string]swiftPMReference, products []pbxSwiftPMProductDependency, err error) {
	localPaths = make(map[string]string)
	remote = make(map[string]swiftPMReference)
	for _, object := range pbxObjectBodies(text, "XCLocalSwiftPackageReference") {
		path := pbxFieldValue(object.Body, "relativePath")
		if path == "" {
			return nil, nil, nil, errors.New("XCLocalSwiftPackageReference has an empty relativePath")
		}
		localPaths[object.ID] = path
	}
	for _, object := range pbxObjectBodies(text, "XCRemoteSwiftPackageReference") {
		url := pbxFieldValue(object.Body, "repositoryURL")
		if url == "" {
			return nil, nil, nil, errors.New("XCRemoteSwiftPackageReference has no repositoryURL")
		}
		requirement := strings.Join(strings.Fields(collapseSpace(pbxRequirementBlock(object.Body))), " ")
		remote[object.ID] = swiftPMReference{
			Name:        strings.TrimSuffix(filepath.Base(strings.TrimSuffix(url, "/")), ".git"),
			URL:         url,
			Requirement: requirement,
		}
	}
	for _, object := range pbxObjectBodies(text, "XCSwiftPackageProductDependency") {
		productName := pbxFieldValue(object.Body, "productName")
		if productName == "" {
			return nil, nil, nil, errors.New("XCSwiftPackageProductDependency has no productName")
		}
		// `package` is absent for a product of a local package reference whose
		// product name equals the package name (Flutter's generated package).
		products = append(products, pbxSwiftPMProductDependency{
			PackageID:   pbxFieldValue(object.Body, "package"),
			ProductName: productName,
		})
	}
	return localPaths, remote, products, nil
}

// pbxFieldValue reads a scalar field from a pbxproj object body.
//
// Xcode annotates object references with a trailing comment, as in
// `package = FEE0 /* XCRemoteSwiftPackageReference "sqlite-data" */;`, so the
// value is not necessarily adjacent to the semicolon. The key must start at a
// token boundary or `productName` would also satisfy a lookup for `name`.
func pbxFieldValue(body, key string) string {
	re := regexp.MustCompile(`(?:^|[^A-Za-z0-9_])` + regexp.QuoteMeta(key) + `\s*=\s*("(?:[^"\\]|\\.)*"|[^;\s]+)\s*(?:/\*[^*]*\*/\s*)?;`)
	match := re.FindStringSubmatch(body)
	if match == nil {
		return ""
	}
	return unquotePbxValue(match[1])
}

func pbxRequirementBlock(body string) string {
	idx := strings.Index(body, "requirement")
	if idx < 0 {
		return ""
	}
	rest := body[idx:]
	open := strings.Index(rest, "{")
	if open < 0 {
		return ""
	}
	depth := 0
	for i := open; i < len(rest); i++ {
		switch rest[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[open+1 : i]
			}
		}
	}
	return ""
}

func unquotePbxValue(value string) string {
	value = strings.TrimSpace(value)
	if match := swiftPMQuotedRe.FindStringSubmatch(value); match != nil {
		unescaped := strings.ReplaceAll(match[1], `\"`, `"`)
		return strings.ReplaceAll(unescaped, `\\`, `\`)
	}
	return value
}

func collapseSpace(value string) string {
	return strings.NewReplacer("\n", " ", "\t", " ", "\r", " ").Replace(value)
}

// packageResolvedPin is one entry of Package.resolved (v2/v3 format).
type packageResolvedPin struct {
	Identity string `json:"identity"`
	Kind     string `json:"kind"`
	Location string `json:"location"`
	State    struct {
		Revision string `json:"revision"`
		Version  string `json:"version"`
		Branch   string `json:"branch"`
	} `json:"state"`
}

type packageResolvedFile struct {
	Pins    []packageResolvedPin `json:"pins"`
	Version int                  `json:"version"`
	// v1 nested its pins under object.pins.
	Object struct {
		Pins []struct {
			Package       string `json:"package"`
			RepositoryURL string `json:"repositoryURL"`
			State         struct {
				Revision string `json:"revision"`
				Version  string `json:"version"`
				Branch   string `json:"branch"`
			} `json:"state"`
		} `json:"pins"`
	} `json:"object"`
}

// loadPackageResolved reads the workspace's Package.resolved pins, keyed by
// package identity. A missing file yields an empty map, not an error: projects
// with only local packages have none.
//
// A project can carry two Package.resolved files — one under
// <name>.xcodeproj/project.xcworkspace and one under a sibling <name>.xcworkspace
// — and they can disagree (immich's differ on three identities). Xcode resolves
// against the workspace it opens; when a CocoaPods-generated .xcworkspace exists
// it wraps the same project, so the project's own file is the one Xcode wrote
// last for a plain `xcodebuild -project` build. Read the sibling workspace first
// and let the project's file win on conflict.
func loadPackageResolved(iosRoot string) (map[string]packageResolvedPin, error) {
	pins := make(map[string]packageResolvedPin)
	workspaces, err := filepath.Glob(filepath.Join(iosRoot, "*.xcworkspace", "xcshareddata", "swiftpm", "Package.resolved"))
	if err != nil {
		return nil, err
	}
	projects, err := filepath.Glob(filepath.Join(iosRoot, "*.xcodeproj", "project.xcworkspace", "xcshareddata", "swiftpm", "Package.resolved"))
	if err != nil {
		return nil, err
	}
	sort.Strings(workspaces)
	sort.Strings(projects)
	candidates := append(workspaces, projects...)

	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var file packageResolvedFile
		if err := json.Unmarshal(data, &file); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, pin := range file.Pins {
			if pin.Identity == "" || pin.State.Revision == "" {
				continue
			}
			pins[strings.ToLower(pin.Identity)] = pin
		}
		for _, pin := range file.Object.Pins {
			if pin.State.Revision == "" {
				continue
			}
			identity := strings.ToLower(pin.Package)
			if pin.RepositoryURL != "" {
				identity = swiftPMIdentityFromURL(pin.RepositoryURL)
			}
			converted := packageResolvedPin{Identity: identity, Kind: "remoteSourceControl", Location: pin.RepositoryURL}
			converted.State.Revision = pin.State.Revision
			converted.State.Version = pin.State.Version
			converted.State.Branch = pin.State.Branch
			pins[identity] = converted
		}
	}
	return pins, nil
}

// swiftPMResolver turns references into on-disk package roots with parsed
// manifests, memoized by root directory.
type swiftPMResolver struct {
	tc     toolchain.Toolchain
	pins   map[string]packageResolvedPin
	cache  string
	loaded map[string]*swiftPMPackage
}

func (r *swiftPMResolver) load(ref swiftPMReference) (*swiftPMPackage, error) {
	root := ref.Path
	if root == "" {
		resolved, err := r.checkout(ref)
		if err != nil {
			return nil, err
		}
		root = resolved
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// A Flutter plugin package is reached through a symlink under
	// Packages/.packages; resolve it so two references to the same plugin share
	// one compiled module.
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	if pkg, ok := r.loaded[root]; ok {
		return pkg, nil
	}
	manifest, err := dumpSwiftPMManifest(r.tc, root)
	if err != nil {
		return nil, err
	}
	pkg := &swiftPMPackage{Root: root, Manifest: manifest, Reference: ref, Identity: ref.identity()}
	for i := range pkg.Manifest.Targets {
		target := &pkg.Manifest.Targets[i]
		for _, raw := range target.Dependencies {
			dep, err := decodeSwiftPMTargetDependency(raw)
			if err != nil {
				return nil, fmt.Errorf("package %s target %s: %w", manifest.Name, target.Name, err)
			}
			target.ParsedDependencies = append(target.ParsedDependencies, dep)
		}
		for _, raw := range target.PluginUsages {
			usage, err := decodeSwiftPMPluginUsage(raw)
			if err != nil {
				return nil, fmt.Errorf("package %s target %s: %w", manifest.Name, target.Name, err)
			}
			target.ParsedPluginUsages = append(target.ParsedPluginUsages, usage)
		}
	}
	r.loaded[root] = pkg
	return pkg, nil
}

// checkout materializes a remote package at its pinned revision under the
// cache. It refuses to resolve a floating requirement: without a pin the build
// would not be reproducible, and on an offline host it cannot work at all.
func (r *swiftPMResolver) checkout(ref swiftPMReference) (string, error) {
	identity := ref.identity()
	pin, ok := r.pins[identity]
	if !ok || pin.State.Revision == "" {
		return "", fmt.Errorf("remote Swift package %s (%s) has no pin in Package.resolved; requirement %q cannot be resolved offline. Commit ios/Runner.xcodeproj/project.xcworkspace/xcshareddata/swiftpm/Package.resolved (Xcode writes it on resolve) so the exact revision is known", ref.Name, ref.URL, ref.Requirement)
	}
	dir := filepath.Join(r.cache, identity+"-"+pin.State.Revision)
	if fileExists(filepath.Join(dir, "Package.swift")) {
		return dir, nil
	}
	location := pin.Location
	if location == "" {
		location = ref.URL
	}
	if err := os.MkdirAll(r.cache, 0o755); err != nil {
		return "", err
	}
	// Fetch into a temp dir so an interrupted clone never leaves a half-populated
	// cache entry that a later run would trust.
	staging, err := os.MkdirTemp(r.cache, "."+identity+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)

	if err := run(staging, nil, "git", "init", "--quiet"); err != nil {
		return "", fmt.Errorf("fetch Swift package %s: %w", ref.Name, err)
	}
	if err := run(staging, nil, "git", "remote", "add", "origin", location); err != nil {
		return "", fmt.Errorf("fetch Swift package %s: %w", ref.Name, err)
	}
	if err := run(staging, nil, "git", "fetch", "--quiet", "--depth", "1", "origin", pin.State.Revision); err != nil {
		return "", fmt.Errorf("fetch Swift package %s at pinned revision %s from %s: %w (no network, or the revision was force-pushed away)", ref.Name, pin.State.Revision, location, err)
	}
	if err := run(staging, nil, "git", "checkout", "--quiet", "FETCH_HEAD"); err != nil {
		return "", fmt.Errorf("check out Swift package %s at %s: %w", ref.Name, pin.State.Revision, err)
	}
	// Pinned packages can themselves vendor submodules (binary artifacts).
	if fileExists(filepath.Join(staging, ".gitmodules")) {
		if err := run(staging, nil, "git", "submodule", "update", "--init", "--recursive", "--depth", "1"); err != nil {
			return "", fmt.Errorf("initialize submodules of Swift package %s: %w", ref.Name, err)
		}
	}
	if !fileExists(filepath.Join(staging, "Package.swift")) {
		return "", fmt.Errorf("Swift package %s revision %s has no Package.swift at its root", ref.Name, pin.State.Revision)
	}
	if err := os.Rename(staging, dir); err != nil {
		// Lost a race with a concurrent build; the winner's tree is equivalent.
		if fileExists(filepath.Join(dir, "Package.swift")) {
			return dir, nil
		}
		return "", err
	}
	return dir, nil
}

// swiftPMPackage is a package root plus its evaluated manifest.
type swiftPMPackage struct {
	Root      string
	Identity  string
	Manifest  swiftPMManifest
	Reference swiftPMReference
	// Dependencies are the resolved package dependencies, filled on demand by
	// the graph builder as the reachable closure is walked.
	Dependencies       []*swiftPMPackage
	dependenciesLoaded bool
	// dependencyErrors records dependencies that could not be resolved. They are
	// only fatal if a reachable target actually needs one.
	dependencyErrors []error
}

// swiftPMManifest mirrors the JSON of `swift package dump-package`.
type swiftPMManifest struct {
	Name         string                     `json:"name"`
	Platforms    []swiftPMPlatform          `json:"platforms"`
	Products     []swiftPMProduct           `json:"products"`
	Targets      []swiftPMTarget            `json:"targets"`
	Dependencies []swiftPMPackageDependency `json:"dependencies"`
	ToolsVersion struct {
		Version string `json:"_version"`
	} `json:"toolsVersion"`
	CLanguageStandard   *string        `json:"cLanguageStandard"`
	CxxLanguageStandard *string        `json:"cxxLanguageStandard"`
	Traits              []swiftPMTrait `json:"traits"`
	// SwiftLanguageVersions is the package-level `swiftLanguageModes:`
	// declaration, dumped as bare version strings ("5", "6").
	SwiftLanguageVersions []string `json:"swiftLanguageVersions"`
}

// swiftPMTrait is a declared package trait: a named, optional feature that
// gates dependencies and target dependencies.
type swiftPMTrait struct {
	Name          string   `json:"name"`
	EnabledTraits []string `json:"enabledTraits"`
}

// enabledTraits is the set of traits active for this package in an app build.
//
// A consumer can request traits, but an Xcode project has no way to express
// that: pbxproj carries no trait information. So only the package's default
// trait set is enabled — the transitive closure of the trait named "default",
// which is empty when no such trait is declared. This matches what Xcode
// resolves, and is why immich's Package.resolved has no pin for swift-tagged:
// sqlite-data guards it behind the non-default trait "SQLiteDataTagged".
func (m swiftPMManifest) enabledTraits() map[string]bool {
	byName := make(map[string]swiftPMTrait, len(m.Traits))
	for _, trait := range m.Traits {
		byName[trait.Name] = trait
	}
	enabled := make(map[string]bool)
	var enable func(name string)
	enable = func(name string) {
		if enabled[name] {
			return
		}
		enabled[name] = true
		for _, child := range byName[name].EnabledTraits {
			enable(child)
		}
	}
	if _, ok := byName["default"]; ok {
		enable("default")
	}
	return enabled
}

type swiftPMPlatform struct {
	PlatformName string `json:"platformName"`
	Version      string `json:"version"`
}

type swiftPMProduct struct {
	Name    string          `json:"name"`
	Targets []string        `json:"targets"`
	Type    json.RawMessage `json:"type"`
}

// kind reports "library", "executable" or "plugin".
func (p swiftPMProduct) kind() string {
	var typ map[string]json.RawMessage
	if err := json.Unmarshal(p.Type, &typ); err != nil {
		return ""
	}
	for key := range typ {
		return key
	}
	return ""
}

type swiftPMTarget struct {
	Name              string                     `json:"name"`
	Type              string                     `json:"type"`
	Path              *string                    `json:"path"`
	Sources           []string                   `json:"sources"`
	Exclude           []string                   `json:"exclude"`
	PublicHeadersPath *string                    `json:"publicHeadersPath"`
	Dependencies      []json.RawMessage          `json:"dependencies"`
	Resources         []swiftPMResource          `json:"resources"`
	Settings          []swiftPMSetting           `json:"settings"`
	PluginUsages      []json.RawMessage          `json:"pluginUsages"`
	PluginCapability  map[string]json.RawMessage `json:"pluginCapability"`
	URL               *string                    `json:"url"`
	Checksum          *string                    `json:"checksum"`
	PackageAccess     bool                       `json:"packageAccess"`
	// Providers is set for `system` library targets (apt/brew hints).
	Providers []json.RawMessage `json:"providers"`

	// ParsedDependencies and ParsedPluginUsages are decoded from the raw JSON
	// once, when the package is loaded.
	ParsedDependencies []swiftPMTargetDependency
	ParsedPluginUsages []swiftPMPluginUsage
}

// swiftPMPluginUsage is a decoded `plugins:` entry of a target.
type swiftPMPluginUsage struct {
	Name        string
	PackageName string
}

// decodeSwiftPMPluginUsage handles {"plugin":["Name",null]} and
// {"plugin":["Name","package"]} shapes emitted by dump-package.
func decodeSwiftPMPluginUsage(raw json.RawMessage) (swiftPMPluginUsage, error) {
	var wrapper map[string][]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return swiftPMPluginUsage{}, err
	}
	for kind, values := range wrapper {
		if kind != "plugin" {
			return swiftPMPluginUsage{}, fmt.Errorf("unsupported plugin usage kind %q", kind)
		}
		if len(values) == 0 {
			return swiftPMPluginUsage{}, errors.New("empty plugin usage")
		}
		usage := swiftPMPluginUsage{}
		if err := json.Unmarshal(values[0], &usage.Name); err != nil {
			return swiftPMPluginUsage{}, err
		}
		if len(values) > 1 {
			// Second element is the package name, or null for a same-package plugin.
			_ = json.Unmarshal(values[1], &usage.PackageName)
		}
		return usage, nil
	}
	return swiftPMPluginUsage{}, errors.New("empty plugin usage")
}

type swiftPMResource struct {
	Path string                     `json:"path"`
	Rule map[string]json.RawMessage `json:"rule"`
}

func (r swiftPMResource) kind() string {
	for key := range r.Rule {
		return key
	}
	return ""
}

type swiftPMSetting struct {
	Tool string                     `json:"tool"`
	Kind map[string]json.RawMessage `json:"kind"`
}

// swiftPMTargetDependency is a decoded target dependency edge.
type swiftPMTargetDependency struct {
	// TargetName is set for byName/target edges.
	TargetName string
	// ProductName/PackageName are set for product edges.
	ProductName string
	PackageName string
	// RequiredTraits are the traits that must be enabled for this edge to exist.
	// A dependency guarded by `condition: .when(traits: [...])` is invisible
	// unless the consumer enables that trait, and Xcode does not resolve the
	// package behind it (so it has no Package.resolved pin).
	RequiredTraits []string
	// Platforms restricts the edge to the named platforms ("ios", "macos", ...).
	// Empty means every platform.
	Platforms []string
}

// swiftPMDependencyCondition is the trailing `condition` element of a target
// dependency: {"platformNames":["ios"],"traits":["SomeTrait"]}.
type swiftPMDependencyCondition struct {
	PlatformNames []string `json:"platformNames"`
	Traits        []string `json:"traits"`
}

// appliesToIOS reports whether the edge is active for an iOS device build with
// the given set of enabled traits.
func (d swiftPMTargetDependency) appliesToIOS(enabled map[string]bool) bool {
	for _, trait := range d.RequiredTraits {
		if !enabled[trait] {
			return false
		}
	}
	if len(d.Platforms) == 0 {
		return true
	}
	for _, platform := range d.Platforms {
		if strings.EqualFold(platform, "ios") {
			return true
		}
	}
	return false
}

// decodeSwiftPMTargetDependency handles the three encodings dump-package emits:
//
//	{"byName":["CLib",null]}
//	{"target":["Flutter",null]}
//	{"product":["FlutterFramework","FlutterFramework",null,null]}
func decodeSwiftPMTargetDependency(raw json.RawMessage) (swiftPMTargetDependency, error) {
	var wrapper map[string][]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return swiftPMTargetDependency{}, fmt.Errorf("decode target dependency %s: %w", raw, err)
	}
	str := func(value json.RawMessage) string {
		var out string
		if err := json.Unmarshal(value, &out); err != nil {
			return ""
		}
		return out
	}
	for kind, values := range wrapper {
		switch kind {
		case "byName", "target":
			if len(values) == 0 {
				return swiftPMTargetDependency{}, fmt.Errorf("empty %s target dependency", kind)
			}
			return swiftPMTargetDependency{TargetName: str(values[0])}, nil
		case "product":
			if len(values) < 2 {
				return swiftPMTargetDependency{}, errors.New("malformed product target dependency")
			}
			dep := swiftPMTargetDependency{ProductName: str(values[0]), PackageName: str(values[1])}
			// The 4th element is the condition, when present and non-null.
			if len(values) >= 4 {
				var condition swiftPMDependencyCondition
				if err := json.Unmarshal(values[3], &condition); err == nil {
					dep.RequiredTraits = condition.Traits
					dep.Platforms = condition.PlatformNames
				}
			}
			return dep, nil
		default:
			return swiftPMTargetDependency{}, fmt.Errorf("unsupported target dependency kind %q", kind)
		}
	}
	return swiftPMTargetDependency{}, errors.New("empty target dependency")
}

// decodeSwiftPMPackageDependency handles fileSystem (path) and sourceControl
// (git) package dependencies.
type swiftPMPackageDependency struct {
	FileSystem []struct {
		Identity string `json:"identity"`
		Name     string `json:"nameForTargetDependencyResolutionOnly"`
		Path     string `json:"path"`
	} `json:"fileSystem"`
	SourceControl []struct {
		Identity string `json:"identity"`
		Location struct {
			Remote []struct {
				URLString string `json:"urlString"`
			} `json:"remote"`
			Local []struct {
				Path string `json:"path"`
			} `json:"local"`
		} `json:"location"`
		Requirement map[string]json.RawMessage `json:"requirement"`
	} `json:"sourceControl"`
	Registry []struct {
		Identity string `json:"identity"`
	} `json:"registry"`
}

// references converts a manifest package dependency into resolvable references.
func (d swiftPMPackageDependency) references() ([]swiftPMReference, error) {
	var refs []swiftPMReference
	for _, entry := range d.FileSystem {
		name := entry.Name
		if name == "" {
			name = entry.Identity
		}
		refs = append(refs, swiftPMReference{Name: name, Path: entry.Path})
	}
	for _, entry := range d.SourceControl {
		requirement := ""
		for kind, value := range entry.Requirement {
			requirement = kind + " " + strings.Trim(string(value), "[]")
			break
		}
		switch {
		case len(entry.Location.Remote) > 0:
			refs = append(refs, swiftPMReference{Name: entry.Identity, URL: entry.Location.Remote[0].URLString, Requirement: requirement})
		case len(entry.Location.Local) > 0:
			refs = append(refs, swiftPMReference{Name: entry.Identity, Path: entry.Location.Local[0].Path, Requirement: requirement})
		default:
			return nil, fmt.Errorf("Swift package dependency %s has no usable location", entry.Identity)
		}
	}
	for _, entry := range d.Registry {
		return nil, fmt.Errorf("Swift package registry dependency %s is not supported; registries require a configured registry service", entry.Identity)
	}
	return refs, nil
}

// swiftPackageExecutable returns the `swift` driver next to swiftc.
func swiftPackageExecutable(tc toolchain.Toolchain) (string, error) {
	bin, err := tc.SwiftBin()
	if err != nil {
		return "", err
	}
	path := filepath.Join(bin, "swift")
	if !fileExists(path) {
		return "", fmt.Errorf("swift driver not found at %s (needed to evaluate Package.swift)", path)
	}
	return path, nil
}

// swiftPMDefaultCommandRunner shells out to the real `swift package` driver.
//
// TMPDIR is redirected onto the ripley cache: evaluating a manifest makes swiftpm
// write a VFS overlay to a temporary file, and on a builder whose /tmp is a
// small shared tmpfs that write can fail silently, leaving an empty overlay and
// an "expected mapping node" error that looks like a broken manifest.
func swiftPMDefaultCommandRunner(tc toolchain.Toolchain, dir string, args ...string) ([]byte, error) {
	swift, err := swiftPackageExecutable(tc)
	if err != nil {
		return nil, err
	}
	tmp := filepath.Join(swiftPMCacheDir(), "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	full := append([]string{"package"}, args...)
	fmt.Printf("+ %s %s\n", swift, strings.Join(full, " "))
	cmd := exec.Command(swift, full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

// swiftPMCommandRunner lets tests substitute the manifest dump without a Swift
// toolchain. It returns the raw stdout of `swift package <args...>`.
var swiftPMCommandRunner = swiftPMDefaultCommandRunner

// dumpSwiftPMManifest evaluates Package.swift with the Swift toolchain. The
// manifest is Swift code, so no textual parse is trustworthy; dump-package is
// the supported machine-readable form and works on Linux.
func dumpSwiftPMManifest(tc toolchain.Toolchain, root string) (swiftPMManifest, error) {
	// Keep SwiftPM's scratch/cache out of the package directory: plugin packages
	// live in the read-only pub cache, and a stray .build breaks `flutter clean`.
	scratch := filepath.Join(swiftPMCacheDir(), "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return swiftPMManifest{}, err
	}
	out, err := swiftPMCommandRunner(tc, root,
		"dump-package", "--package-path", root,
		"--cache-path", filepath.Join(scratch, "cache"),
		"--scratch-path", filepath.Join(scratch, "build"),
	)
	if err != nil {
		return swiftPMManifest{}, fmt.Errorf("evaluate %s: %w (swift package dump-package failed; a manifest declaring a swift-tools-version newer than the installed Swift toolchain cannot be read)", filepath.Join(root, "Package.swift"), err)
	}
	// dump-package may print warnings before the JSON object.
	idx := indexOfJSONObject(out)
	if idx < 0 {
		return swiftPMManifest{}, fmt.Errorf("evaluate %s: dump-package produced no JSON", filepath.Join(root, "Package.swift"))
	}
	var manifest swiftPMManifest
	if err := json.Unmarshal(out[idx:], &manifest); err != nil {
		return swiftPMManifest{}, fmt.Errorf("decode manifest of %s: %w", root, err)
	}
	if manifest.Name == "" {
		return swiftPMManifest{}, fmt.Errorf("manifest of %s has no package name", root)
	}
	return manifest, nil
}

func indexOfJSONObject(data []byte) int {
	for i, b := range data {
		if b == '{' {
			return i
		}
	}
	return -1
}

// swiftPMRootPackage is a package the project references directly, plus the
// product names the app target actually consumes from it.
type swiftPMRootPackage struct {
	Package *swiftPMPackage
	// Products is empty when the reference names no specific product, which
	// means every product the package vends is consumed.
	Products []string
}

// swiftPMGraphTarget is one compilable target with its resolved position in the
// dependency order.
type swiftPMGraphTarget struct {
	Package *swiftPMPackage
	Target  swiftPMTarget
	// Deps are graph keys of targets this one depends on.
	Deps []string
}

type swiftPMGraph struct {
	// Ordered targets, dependencies first.
	Ordered []swiftPMGraphTarget
	// Packages in load order.
	Packages []*swiftPMPackage
}

func swiftPMGraphKey(pkg *swiftPMPackage, targetName string) string {
	return pkg.Manifest.Name + "\x00" + targetName
}

// newSwiftPMGraph loads the dependency closure of the root packages and
// topologically orders every target reachable from the products the app
// actually consumes.
//
// Pruning to the reachable closure is not an optimization. A real package graph
// contains test-only, host-only and platform-specific targets that are never
// part of an app build (immich's graph has 128 targets, of which 45 are
// reachable), and compiling the unreachable ones would fail on dependencies the
// app never asked for.
func newSwiftPMGraph(roots []swiftPMRootPackage, resolver *swiftPMResolver) (*swiftPMGraph, error) {
	// Every loaded package, keyed by manifest name and by identity, so
	// .product(name:package:) resolves regardless of which spelling the
	// dependent manifest used. SwiftPM compares package identities
	// case-insensitively (sqlite-data says package: "GRDB.swift" while the
	// identity derived from the repository URL is "grdb.swift"), so every key is
	// also indexed lowercased.
	byName := make(map[string]*swiftPMPackage)
	lookupPackage := func(name string) *swiftPMPackage {
		if pkg := byName[name]; pkg != nil {
			return pkg
		}
		return byName[strings.ToLower(name)]
	}
	nodes := make(map[string]*swiftPMGraphTarget)
	var packages []*swiftPMPackage

	// index makes a loaded package's targets addressable. Package dependencies
	// are NOT followed here: they are resolved on demand while walking the
	// reachable closure. Eagerly resolving them would fetch packages no app
	// target uses, and those are routinely unpinned — GRDB depends on
	// swift-docc-plugin, which Xcode never resolves for an app build and which
	// therefore has no entry in Package.resolved.
	indexName := func(name string, pkg *swiftPMPackage) {
		if name == "" {
			return
		}
		if _, ok := byName[name]; !ok {
			byName[name] = pkg
		}
		lower := strings.ToLower(name)
		if _, ok := byName[lower]; !ok {
			byName[lower] = pkg
		}
	}
	index := func(pkg *swiftPMPackage) {
		if _, ok := byName[pkg.Manifest.Name]; ok {
			return
		}
		indexName(pkg.Manifest.Name, pkg)
		indexName(pkg.Identity, pkg)
		packages = append(packages, pkg)
		for _, target := range pkg.Manifest.Targets {
			nodes[swiftPMGraphKey(pkg, target.Name)] = &swiftPMGraphTarget{Package: pkg, Target: target}
		}
	}
	for _, root := range roots {
		index(root.Package)
	}

	// dependencies resolves and indexes a package's declared dependencies the
	// first time one of its reachable targets needs them.
	//
	// A failure to resolve one dependency is deferred rather than fatal: real
	// manifests declare dependencies that an app build never uses (GRDB pulls
	// swift-docc-plugin, sqlite-data pulls swift-tagged for its tests), and Xcode
	// does not resolve those either, so they have no pin. The error is only
	// surfaced if a reachable target turns out to need a product from that
	// dependency — in which case the missing pin is a genuine build failure.
	dependencies := func(pkg *swiftPMPackage) ([]*swiftPMPackage, []error, error) {
		if pkg.dependenciesLoaded {
			return pkg.Dependencies, pkg.dependencyErrors, nil
		}
		for _, dep := range pkg.Manifest.Dependencies {
			refs, err := dep.references()
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", pkg.Root, err)
			}
			for _, ref := range refs {
				child, err := resolver.load(ref)
				if err != nil {
					pkg.dependencyErrors = append(pkg.dependencyErrors, err)
					continue
				}
				pkg.Dependencies = append(pkg.Dependencies, child)
				index(child)
				indexName(ref.Name, child)
			}
		}
		pkg.dependenciesLoaded = true
		return pkg.Dependencies, pkg.dependencyErrors, nil
	}

	productTargets := func(pkg *swiftPMPackage, productName string) ([]string, error) {
		for _, product := range pkg.Manifest.Products {
			if product.Name != productName {
				continue
			}
			var keys []string
			for _, name := range product.Targets {
				key := swiftPMGraphKey(pkg, name)
				if _, ok := nodes[key]; !ok {
					return nil, fmt.Errorf("package %s product %s references unknown target %s", pkg.Manifest.Name, productName, name)
				}
				keys = append(keys, key)
			}
			return keys, nil
		}
		return nil, fmt.Errorf("package %s has no product %s", pkg.Manifest.Name, productName)
	}

	// resolveDeps computes the graph keys a target depends on. It runs only for
	// reachable targets, so an unreachable target with an unsatisfiable
	// dependency (common: test-only targets naming test-only products) never
	// fails the build.
	resolveDeps := func(node *swiftPMGraphTarget) ([]string, error) {
		pkg, target := node.Package, node.Target
		// A macro target is built for the host against the toolchain's prebuilt
		// SwiftSyntax, so its dependencies (SwiftCompilerPlugin/SwiftSyntaxMacros
		// from swift-syntax) are host-only and must not enter the iOS graph.
		// Following them would try to cross-compile all of swift-syntax for
		// arm64-apple-ios, which no app ever links.
		//
		// They must still be *loaded*: buildSwiftPMMacroPlugin compiles
		// SwiftCompilerPlugin out of the swift-syntax checkout, and it finds that
		// checkout by walking pkg.Dependencies. Returning here without resolving
		// them leaves that list empty unless some unrelated target in the same
		// package happens to force the load first, which is how a macro-only
		// package would fail with a spurious "needs the swift-syntax package but
		// it is not in the dependency graph".
		if target.Type == "macro" {
			if _, _, err := dependencies(pkg); err != nil {
				return nil, err
			}
			return nil, nil
		}
		var deps []string
		enabled := pkg.Manifest.enabledTraits()
		for _, dep := range target.ParsedDependencies {
			// A dependency guarded by `condition: .when(traits:)` does not exist
			// unless the trait is enabled. Following it anyway would resolve a
			// package Xcode never resolves, which therefore has no pin
			// (sqlite-data gates Tagged behind its default-off SQLiteDataTagged
			// trait). Likewise a non-iOS platform condition is not our build.
			if !dep.appliesToIOS(enabled) {
				continue
			}
			switch {
			case dep.TargetName != "":
				// byName names either a sibling target or a product of a package
				// dependency (SwiftPM's shorthand).
				if _, ok := nodes[swiftPMGraphKey(pkg, dep.TargetName)]; ok {
					deps = append(deps, swiftPMGraphKey(pkg, dep.TargetName))
					continue
				}
				resolved := false
				candidates, deferred, err := dependencies(pkg)
				if err != nil {
					return nil, err
				}
				for _, candidate := range candidates {
					if keys, err := productTargets(candidate, dep.TargetName); err == nil {
						deps = append(deps, keys...)
						resolved = true
						break
					}
				}
				if !resolved {
					// The name may belong to a dependency that failed to resolve;
					// report that cause rather than a misleading "unknown name".
					if len(deferred) > 0 {
						return nil, fmt.Errorf("package %s target %s depends on %q, which could not be resolved: %w", pkg.Manifest.Name, target.Name, dep.TargetName, errors.Join(deferred...))
					}
					return nil, fmt.Errorf("package %s target %s depends on %q which is neither a target in the package nor a product of its dependencies", pkg.Manifest.Name, target.Name, dep.TargetName)
				}
			case dep.ProductName != "":
				// FlutterFramework is a generated marker package whose real payload
				// is Flutter.xcframework. ripley already exposes that framework to every
				// SwiftPM compile via frameworkSearch, so package-only Flutter plugins
				// can be evaluated directly even when Flutter has not materialized the
				// sibling FlutterFramework package on this Linux checkout.
				if dep.ProductName == flutterFrameworkSwiftPackageName && strings.EqualFold(dep.PackageName, flutterFrameworkSwiftPackageName) {
					continue
				}
				// Resolving the package's dependencies also indexes them, which is
				// what makes the named package findable.
				_, deferred, err := dependencies(pkg)
				if err != nil {
					return nil, err
				}
				owner := lookupPackage(dep.PackageName)
				if owner == nil {
					if len(deferred) > 0 {
						return nil, fmt.Errorf("package %s target %s depends on product %s of package %s, which could not be resolved: %w", pkg.Manifest.Name, target.Name, dep.ProductName, dep.PackageName, errors.Join(deferred...))
					}
					return nil, fmt.Errorf("package %s target %s depends on product %s of unknown package %s", pkg.Manifest.Name, target.Name, dep.ProductName, dep.PackageName)
				}
				keys, err := productTargets(owner, dep.ProductName)
				if err != nil {
					return nil, err
				}
				deps = append(deps, keys...)
			}
		}
		// Build tool plugins used by this target must run before it.
		for _, usage := range target.ParsedPluginUsages {
			// A plugin may live in a package dependency, so resolve them first.
			candidates, deferred, err := dependencies(pkg)
			if err != nil {
				return nil, err
			}
			owner := pkg
			if usage.PackageName != "" {
				if candidate := lookupPackage(usage.PackageName); candidate != nil {
					owner = candidate
				}
			}
			key := swiftPMGraphKey(owner, usage.Name)
			if _, ok := nodes[key]; !ok {
				// A plugin is also reachable as a product of a dependency.
				found := false
				for _, candidate := range candidates {
					if keys, err := productTargets(candidate, usage.Name); err == nil {
						deps = append(deps, keys...)
						found = true
						break
					}
				}
				if !found {
					if len(deferred) > 0 {
						return nil, fmt.Errorf("package %s target %s uses plugin %s, which could not be resolved: %w", pkg.Manifest.Name, target.Name, usage.Name, errors.Join(deferred...))
					}
					return nil, fmt.Errorf("package %s target %s uses unknown plugin %s", pkg.Manifest.Name, target.Name, usage.Name)
				}
				continue
			}
			deps = append(deps, key)
		}
		return uniqueStrings(deps), nil
	}

	// Seed the closure with the targets behind every consumed product.
	var seeds []string
	for _, root := range roots {
		wanted := root.Products
		if len(wanted) == 0 {
			// No product named: the whole package is consumed. This is what
			// Flutter's generated plugin package implies.
			for _, product := range root.Package.Manifest.Products {
				wanted = append(wanted, product.Name)
			}
		}
		for _, name := range wanted {
			keys, err := productTargets(root.Package, name)
			if err != nil {
				return nil, err
			}
			seeds = append(seeds, keys...)
		}
	}
	sort.Strings(seeds)
	seeds = uniqueStrings(seeds)

	// Depth-first walk of the reachable closure, emitting dependencies first.
	state := make(map[string]int) // 0 unvisited, 1 in progress, 2 done
	var order []swiftPMGraphTarget
	used := make(map[*swiftPMPackage]bool)
	var visit func(key string, stack []string) error
	visit = func(key string, stack []string) error {
		switch state[key] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("Swift package target dependency cycle: %s", strings.Join(append(stack, key), " -> "))
		}
		state[key] = 1
		node := nodes[key]
		deps, err := resolveDeps(node)
		if err != nil {
			return err
		}
		node.Deps = deps
		sorted := append([]string(nil), deps...)
		sort.Strings(sorted)
		for _, dep := range sorted {
			if err := visit(dep, append(stack, key)); err != nil {
				return err
			}
		}
		state[key] = 2
		used[node.Package] = true
		order = append(order, *node)
		return nil
	}
	for _, key := range seeds {
		if err := visit(key, nil); err != nil {
			return nil, err
		}
	}

	graph := &swiftPMGraph{Ordered: order}
	for _, pkg := range packages {
		if used[pkg] {
			graph.Packages = append(graph.Packages, pkg)
		}
	}
	return graph, nil
}

// swiftPMCompiled tracks per-target compile outputs so dependents can import
// the produced module, include its public headers and load its macro plugins.
type swiftPMCompiled struct {
	// IncludeDirs are -I inputs this target publishes to dependents, including
	// everything it inherited: a .swiftmodule names the modules it imports, so a
	// dependent must be able to find that whole closure.
	IncludeDirs []string
	// FrameworkPaths are -F inputs published to dependents.
	FrameworkPaths []string
	// MacroPlugins are `<executable>#<ModuleName>` entries for
	// -load-plugin-executable, published by macro targets to their dependents.
	MacroPlugins []string
}

// compileSwiftPMGraph compiles every target of the graph in dependency order.
// Objects come out dependencies-first so the caller can link them in order.
func compileSwiftPMGraph(tc toolchain.Toolchain, graph *swiftPMGraph, work, minOS string) (SwiftPMBuild, error) {
	var build SwiftPMBuild
	compiled := make(map[string]*swiftPMCompiled)
	for _, pkg := range graph.Packages {
		build.Packages = append(build.Packages, pkg.Root)
	}

	for _, node := range graph.Ordered {
		pkg := node.Package
		target := node.Target
		key := swiftPMGraphKey(pkg, target.Name)

		// Inherit every include/framework path and macro plugin published by
		// dependencies. Macro plugins propagate transitively: a target importing
		// a module whose API expands a macro needs that plugin loaded too.
		var includeDirs, frameworkPaths, macroPlugins []string
		for _, dep := range node.Deps {
			result := compiled[dep]
			if result == nil {
				continue
			}
			includeDirs = append(includeDirs, result.IncludeDirs...)
			frameworkPaths = append(frameworkPaths, result.FrameworkPaths...)
			macroPlugins = append(macroPlugins, result.MacroPlugins...)
		}
		includeDirs = uniqueStrings(includeDirs)
		frameworkPaths = uniqueStrings(frameworkPaths)
		macroPlugins = uniqueStrings(macroPlugins)

		switch target.Type {
		case "plugin", "test":
			// Plugin targets are executed by the targets that use them, never
			// compiled for iOS. Test targets are not part of an app build.
			continue
		case "system":
			// A system-library target is just a module map over a preinstalled
			// library: nothing to compile, but its module map directory must be
			// on the include path and its `link "name"` honoured.
			include, libraries, err := swiftPMSystemLibraryInclude(pkg, target)
			if err != nil {
				return SwiftPMBuild{}, err
			}
			compiled[key] = &swiftPMCompiled{IncludeDirs: []string{include}}
			build.ModuleSearchPaths = append(build.ModuleSearchPaths, include)
			build.LinkerLibraries = append(build.LinkerLibraries, libraries...)
			continue
		case "macro":
			// A macro target is a compiler plugin: it runs on the *host* during
			// compilation of its dependents and contributes no iOS code. It must
			// be built for the host and passed via -load-plugin-executable.
			executable, err := buildSwiftPMMacroPlugin(tc, node, work)
			if err != nil {
				return SwiftPMBuild{}, err
			}
			entry := executable + "#" + swiftPMModuleName(target.Name)
			compiled[key] = &swiftPMCompiled{MacroPlugins: []string{entry}}
			// The app's own target compiles against these packages too (immich
			// applies @Table in ios/Runner/Schemas/Tables.swift), so the plugin
			// has to escape the package graph and reach the host compile.
			build.MacroPlugins = append(build.MacroPlugins, entry)
			continue
		case "binary":
			framework, objects, headers, err := resolveSwiftPMBinaryTarget(pkg, target, work)
			if err != nil {
				return SwiftPMBuild{}, err
			}
			// Headers/Modules go on the include path; the .framework itself is
			// found through its parent directory with -F.
			result := &swiftPMCompiled{IncludeDirs: uniqueStrings(headers)}
			if framework != "" {
				build.Frameworks = append(build.Frameworks, framework)
				result.FrameworkPaths = []string{filepath.Dir(framework)}
			}
			build.Objects = append(build.Objects, objects...)
			compiled[key] = result
			build.ModuleSearchPaths = append(build.ModuleSearchPaths, result.IncludeDirs...)
			build.ModuleSearchPaths = append(build.ModuleSearchPaths, result.FrameworkPaths...)
			continue
		case "regular", "executable":
		default:
			return SwiftPMBuild{}, fmt.Errorf("Swift package %s target %s has unsupported type %q", pkg.Manifest.Name, target.Name, target.Type)
		}

		generated, err := runSwiftPMPlugins(tc, node, work)
		if err != nil {
			return SwiftPMBuild{}, err
		}
		objects, resources, modulePaths, err := compileSwiftPMTarget(tc, node, generated, work, minOS, includeDirs, frameworkPaths, macroPlugins)
		if err != nil {
			return SwiftPMBuild{}, fmt.Errorf("Swift package %s target %s: %w", pkg.Manifest.Name, target.Name, err)
		}
		// Macro plugins and include paths keep flowing downstream. A
		// .swiftmodule records the modules it imports, so any dependent must be
		// able to find that whole closure: CombineSchedulers imports
		// IssueReporting, whose module in turn references
		// IssueReportingPackageSupport, and swiftc loads both.
		compiled[key] = &swiftPMCompiled{
			IncludeDirs:    uniqueStrings(append(append([]string(nil), includeDirs...), modulePaths...)),
			FrameworkPaths: frameworkPaths,
			MacroPlugins:   macroPlugins,
		}
		build.Objects = append(build.Objects, objects...)
		build.Resources = append(build.Resources, resources...)
		build.ModuleSearchPaths = append(build.ModuleSearchPaths, modulePaths...)

		settings, err := decodeSwiftPMSettings(pkg.Root, target)
		if err != nil {
			return SwiftPMBuild{}, err
		}
		build.LinkerLibraries = append(build.LinkerLibraries, settings.LinkedLibraries...)
		build.LinkerFrameworks = append(build.LinkerFrameworks, settings.LinkedFrameworks...)
		if len(objects) > 0 {
			fmt.Printf("  swiftpm %s/%s\n", pkg.Manifest.Name, target.Name)
		}
	}

	build.Frameworks = uniqueStrings(build.Frameworks)
	build.Resources = uniqueStrings(build.Resources)
	build.ModuleSearchPaths = uniqueStrings(build.ModuleSearchPaths)
	build.Packages = uniqueStrings(build.Packages)
	build.LinkerLibraries = uniqueStrings(build.LinkerLibraries)
	build.LinkerFrameworks = uniqueStrings(build.LinkerFrameworks)
	build.MacroPlugins = uniqueStrings(build.MacroPlugins)
	return build, nil
}

// swiftPMModuleName mirrors SwiftPM: a target's module name is its name with
// every character that is not alphanumeric replaced by an underscore.
func swiftPMModuleName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// swiftPMTargetRoot resolves a target's source directory, following SwiftPM's
// layout rules: an explicit `path`, else Sources/<name> (Sources/ alone when the
// package has a single target laid out flat).
func swiftPMTargetRoot(pkg *swiftPMPackage, target swiftPMTarget) (string, error) {
	if target.Path != nil && *target.Path != "" {
		root := filepath.Join(pkg.Root, *target.Path)
		if !dirExists(root) {
			return "", fmt.Errorf("declared path %s does not exist", *target.Path)
		}
		return root, nil
	}
	candidates := []string{
		filepath.Join(pkg.Root, "Sources", target.Name),
		filepath.Join(pkg.Root, "Source", target.Name),
		filepath.Join(pkg.Root, "src", target.Name),
		filepath.Join(pkg.Root, target.Name),
	}
	for _, candidate := range candidates {
		if dirExists(candidate) {
			return candidate, nil
		}
	}
	// Single-target package with sources directly under Sources/.
	if len(pkg.Manifest.Targets) == 1 {
		for _, flat := range []string{filepath.Join(pkg.Root, "Sources"), filepath.Join(pkg.Root, "src")} {
			if dirExists(flat) {
				return flat, nil
			}
		}
	}
	return "", fmt.Errorf("no source directory found (looked for Sources/%s)", target.Name)
}

// swiftPMSourceFiles collects the compilable sources of a target, honouring
// explicit `sources` and `exclude` lists and skipping resource paths.
func swiftPMSourceFiles(root string, target swiftPMTarget, resourcePaths map[string]bool) (swift []string, clang []string, err error) {
	excluded := make(map[string]bool, len(target.Exclude))
	for _, entry := range target.Exclude {
		excluded[filepath.Clean(filepath.Join(root, entry))] = true
	}
	isExcluded := func(path string) bool {
		for dir := filepath.Clean(path); ; dir = filepath.Dir(dir) {
			if excluded[dir] || resourcePaths[dir] {
				return true
			}
			if dir == root || dir == "/" || dir == "." {
				return false
			}
		}
	}
	classify := func(path string) {
		if isExcluded(path) {
			return
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".swift":
			swift = append(swift, path)
		case ".c", ".m", ".mm", ".cc", ".cpp", ".cxx", ".s":
			clang = append(clang, path)
		}
	}
	// SwiftPM follows directory symlinks inside a target (swift-structured-queries
	// ships a "Symbolic Links" directory linking shared sources into its macro
	// target), and filepath.WalkDir deliberately does not. Walk manually,
	// resolving directories and guarding against symlink cycles.
	visited := make(map[string]bool)
	var walk func(dir string) error
	walk = func(dir string) error {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return err
		}
		if visited[real] {
			return nil
		}
		visited[real] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if isExcluded(path) {
				continue
			}
			info, err := os.Stat(path)
			if err != nil {
				// A dangling symlink is not a source file.
				continue
			}
			if info.IsDir() {
				if err := walk(path); err != nil {
					return err
				}
				continue
			}
			classify(path)
		}
		return nil
	}
	if len(target.Sources) > 0 {
		for _, entry := range target.Sources {
			path := filepath.Join(root, entry)
			switch {
			case dirExists(path):
				if err := walk(path); err != nil {
					return nil, nil, err
				}
			case fileExists(path):
				classify(path)
			default:
				return nil, nil, fmt.Errorf("declared source %s does not exist", entry)
			}
		}
	} else if err := walk(root); err != nil {
		return nil, nil, err
	}
	sort.Strings(swift)
	sort.Strings(clang)
	return swift, clang, nil
}

// swiftPMTargetSettings is the decoded cSettings/swiftSettings/linkerSettings of
// one target.
type swiftPMTargetSettings struct {
	CDefines          []string
	CUnsafeFlags      []string
	HeaderSearchPaths []string
	SwiftDefines      []string
	SwiftUnsafeFlags  []string
	SwiftLanguageMode string
	LinkedLibraries   []string
	LinkedFrameworks  []string
	LinkerUnsafeFlags []string
}

func decodeSwiftPMSettings(root string, target swiftPMTarget) (swiftPMTargetSettings, error) {
	var out swiftPMTargetSettings
	for _, setting := range target.Settings {
		for kind, raw := range setting.Kind {
			decodeString := func() (string, error) {
				var wrapper struct {
					Value string `json:"_0"`
				}
				if err := json.Unmarshal(raw, &wrapper); err != nil {
					return "", err
				}
				return wrapper.Value, nil
			}
			decodeStrings := func() ([]string, error) {
				var wrapper struct {
					Values []string `json:"_0"`
				}
				if err := json.Unmarshal(raw, &wrapper); err != nil {
					return nil, err
				}
				return wrapper.Values, nil
			}
			switch kind {
			case "define":
				value, err := decodeString()
				if err != nil {
					return out, err
				}
				if setting.Tool == "swift" {
					out.SwiftDefines = append(out.SwiftDefines, value)
				} else {
					out.CDefines = append(out.CDefines, value)
				}
			case "headerSearchPath":
				value, err := decodeString()
				if err != nil {
					return out, err
				}
				out.HeaderSearchPaths = append(out.HeaderSearchPaths, filepath.Join(root, value))
			case "unsafeFlags":
				values, err := decodeStrings()
				if err != nil {
					return out, err
				}
				switch setting.Tool {
				case "swift":
					out.SwiftUnsafeFlags = append(out.SwiftUnsafeFlags, values...)
				case "linker":
					out.LinkerUnsafeFlags = append(out.LinkerUnsafeFlags, values...)
				default:
					out.CUnsafeFlags = append(out.CUnsafeFlags, values...)
				}
			case "linkedLibrary":
				value, err := decodeString()
				if err != nil {
					return out, err
				}
				out.LinkedLibraries = append(out.LinkedLibraries, value)
			case "linkedFramework":
				value, err := decodeString()
				if err != nil {
					return out, err
				}
				out.LinkedFrameworks = append(out.LinkedFrameworks, value)
			case "swiftLanguageMode", "swiftLanguageVersion":
				value, err := decodeString()
				if err != nil {
					return out, err
				}
				out.SwiftLanguageMode = value
			case "enableUpcomingFeature", "enableExperimentalFeature":
				value, err := decodeString()
				if err != nil {
					return out, err
				}
				flag := "-enable-upcoming-feature"
				if kind == "enableExperimentalFeature" {
					flag = "-enable-experimental-feature"
				}
				out.SwiftUnsafeFlags = append(out.SwiftUnsafeFlags, flag, value)
			case "interoperabilityMode":
				value, err := decodeString()
				if err != nil {
					return out, err
				}
				out.SwiftUnsafeFlags = append(out.SwiftUnsafeFlags, "-cxx-interoperability-mode="+value)
			case "treatAllWarnings", "treatWarning", "strictMemorySafety", "defaultIsolation":
				// Diagnostic-only settings: they never change emitted code.
			default:
				return out, fmt.Errorf("unsupported %s setting %q", setting.Tool, kind)
			}
		}
	}
	return out, nil
}

// runSwiftPMPlugins executes the build tool / prebuild command plugins a target
// declares and returns the generated source files.
//
// SwiftPM plugins are ordinary Swift programs compiled for the host, so they run
// on Linux. `swift build` is used as the plugin driver because it is the only
// supported way to evaluate createBuildCommands(context:target:); the actual
// iOS compile is still done by us. Because the host build of an iOS-only target
// will not link, only the plugin outputs are consumed, and a host compile
// failure after plugins have emitted their outputs is not treated as fatal.
func runSwiftPMPlugins(tc toolchain.Toolchain, node swiftPMGraphTarget, work string) ([]string, error) {
	pkg := node.Package
	var pluginNames []string
	for _, usage := range node.Target.ParsedPluginUsages {
		pluginNames = append(pluginNames, usage.Name)
	}
	if len(pluginNames) == 0 {
		return nil, nil
	}

	// Command plugins are not part of the build graph: Xcode never runs them
	// during a build, and running them would mutate the package source. Reject
	// them explicitly rather than silently ignoring a declared dependency.
	for _, name := range pluginNames {
		plugin, ok := swiftPMTargetByName(pkg, name)
		if !ok {
			return nil, fmt.Errorf("package %s target %s uses plugin %s which the manifest does not declare", pkg.Manifest.Name, node.Target.Name, name)
		}
		if _, isCommand := plugin.PluginCapability["command"]; isCommand {
			return nil, fmt.Errorf("package %s target %s uses command plugin %s: command plugins are user-invoked (swift package %s), never part of a build, so honouring it during `ripley build` would silently mutate package sources", pkg.Manifest.Name, node.Target.Name, name, swiftPMCommandVerb(plugin))
		}
		if _, isBuildTool := plugin.PluginCapability["buildTool"]; !isBuildTool {
			return nil, fmt.Errorf("package %s plugin target %s has an unsupported capability %v", pkg.Manifest.Name, name, swiftPMKeys(plugin.PluginCapability))
		}
	}

	scratch := filepath.Join(work, "plugin-scratch", pkg.Manifest.Name)
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return nil, err
	}
	// The host build may fail to compile iOS-only sources; the plugin phase runs
	// first and its outputs land in the scratch tree regardless.
	buildErr := runSwiftPMBuildDriver(tc, pkg.Root, scratch, node.Target.Name)

	outputs := filepath.Join(scratch, "plugins", "outputs")
	var generated []string
	if dirExists(outputs) {
		err := filepath.WalkDir(outputs, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".swift", ".c", ".m", ".mm", ".cc", ".cpp", ".cxx", ".h":
				generated = append(generated, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(generated) == 0 {
		reason := "the plugin produced no source outputs"
		if buildErr != nil {
			reason = fmt.Sprintf("the plugin driver failed: %v", buildErr)
		}
		return nil, fmt.Errorf("build tool plugin(s) %s of package %s target %s produced no sources: %s", strings.Join(pluginNames, ", "), pkg.Manifest.Name, node.Target.Name, reason)
	}
	sort.Strings(generated)
	return generated, nil
}

// swiftPMBuildDriver is a seam so tests can assert the plugin driver invocation
// without a Swift toolchain.
var swiftPMBuildDriver = func(tc toolchain.Toolchain, packageRoot, scratch, targetName string) error {
	swift, err := swiftPackageExecutable(tc)
	if err != nil {
		return err
	}
	args := []string{"build", "--package-path", packageRoot, "--scratch-path", scratch, "--target", targetName}
	return run(packageRoot, nil, swift, args...)
}

func runSwiftPMBuildDriver(tc toolchain.Toolchain, packageRoot, scratch, targetName string) error {
	return swiftPMBuildDriver(tc, packageRoot, scratch, targetName)
}

func swiftPMTargetByName(pkg *swiftPMPackage, name string) (swiftPMTarget, bool) {
	for _, target := range pkg.Manifest.Targets {
		if target.Name == name {
			return target, true
		}
	}
	return swiftPMTarget{}, false
}

func swiftPMCommandVerb(plugin swiftPMTarget) string {
	raw, ok := plugin.PluginCapability["command"]
	if !ok {
		return plugin.Name
	}
	var payload []json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil || len(payload) == 0 {
		return plugin.Name
	}
	var intent map[string]struct {
		Verb string `json:"verb"`
	}
	if err := json.Unmarshal(payload[0], &intent); err != nil {
		return plugin.Name
	}
	for _, value := range intent {
		if value.Verb != "" {
			return value.Verb
		}
	}
	return plugin.Name
}

func swiftPMKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// resolveSwiftPMBinaryTarget picks the iOS arm64 device slice of a binaryTarget's
// xcframework. Remote binary targets (url + checksum) must have been fetched
// into the artifact cache; SwiftPM's own download path is not reimplemented here
// because the checksum-verified artifact is what Xcode consumes too.
func resolveSwiftPMBinaryTarget(pkg *swiftPMPackage, target swiftPMTarget, work string) (framework string, objects []string, headers []string, err error) {
	var artifact string
	switch {
	case target.Path != nil && *target.Path != "":
		artifact = filepath.Join(pkg.Root, *target.Path)
	case target.URL != nil && *target.URL != "":
		checksum := ""
		if target.Checksum != nil {
			checksum = *target.Checksum
		}
		cached := filepath.Join(swiftPMCacheDir(), "artifacts", pkg.Identity, checksum, target.Name+".xcframework")
		if !dirExists(cached) {
			return "", nil, nil, fmt.Errorf("package %s binary target %s is a remote artifact (%s, checksum %s) that is not in the artifact cache at %s; download and unzip it there so the build stays reproducible and offline-capable", pkg.Manifest.Name, target.Name, *target.URL, checksum, filepath.Dir(cached))
		}
		artifact = cached
	default:
		return "", nil, nil, fmt.Errorf("package %s binary target %s has neither path nor url", pkg.Manifest.Name, target.Name)
	}
	if !dirExists(artifact) {
		return "", nil, nil, fmt.Errorf("package %s binary target %s: %s does not exist", pkg.Manifest.Name, target.Name, artifact)
	}

	switch strings.ToLower(filepath.Ext(artifact)) {
	case ".xcframework":
		selected, headersPath, err := selectXCFrameworkSlice(artifact)
		if err != nil {
			return "", nil, nil, err
		}
		if headersPath != "" {
			headers = append(headers, headersPath)
		}
		switch strings.ToLower(filepath.Ext(selected)) {
		case ".framework":
			if headersDir := filepath.Join(selected, "Headers"); dirExists(headersDir) {
				headers = append(headers, headersDir)
			}
			if modulesDir := filepath.Join(selected, "Modules"); dirExists(modulesDir) {
				headers = append(headers, modulesDir)
			}
			return selected, nil, headers, nil
		case ".a":
			return "", []string{selected}, headers, nil
		default:
			return "", nil, nil, fmt.Errorf("package %s binary target %s: unsupported xcframework slice %s", pkg.Manifest.Name, target.Name, selected)
		}
	case ".framework":
		return artifact, nil, headers, nil
	case ".a":
		return "", []string{artifact}, headers, nil
	}
	return "", nil, nil, fmt.Errorf("package %s binary target %s: unsupported artifact %s", pkg.Manifest.Name, target.Name, artifact)
}

// swiftPMCompileSpec is the fully resolved compile description of one target.
// It exists as a separate value so tests can assert flag composition without
// invoking swiftc.
type swiftPMCompileSpec struct {
	ModuleName    string
	SwiftSources  []string
	ClangSources  []string
	SwiftArgs     []string
	ClangArgs     [][]string
	SwiftObject   string
	SwiftModule   string
	SwiftHeader   string
	ClangObjects  []string
	IncludeDirs   []string
	ResourceCopy  []swiftPMResourceCopy
	PublicHeaders string
}

type swiftPMResourceCopy struct {
	Source string
	Dest   string
	// Process is true for `.process` (flattened into the bundle root), false for
	// `.copy` (directory structure preserved).
	Process bool
}

// compileSwiftPMTarget compiles one regular target and returns objects,
// resources and the module search paths it publishes.
func compileSwiftPMTarget(tc toolchain.Toolchain, node swiftPMGraphTarget, generated []string, work, minOS string, includeDirs, frameworkPaths, macroPlugins []string) (objects, resources, modulePaths []string, err error) {
	spec, err := swiftPMTargetCompileSpec(tc, node, generated, work, minOS, includeDirs, frameworkPaths, macroPlugins)
	if err != nil {
		return nil, nil, nil, err
	}
	if spec == nil {
		return nil, nil, nil, nil
	}
	if len(spec.SwiftSources) > 0 {
		swiftc, err := swiftCompiler(tc)
		if err != nil {
			return nil, nil, nil, err
		}
		if err := run("", nil, swiftc, spec.SwiftArgs...); err != nil {
			return nil, nil, nil, fmt.Errorf("compile Swift package target %s: %w", node.Target.Name, err)
		}
		objects = append(objects, spec.SwiftObject)
	}
	if len(spec.ClangSources) > 0 {
		clang, err := pluginClang(tc)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, args := range spec.ClangArgs {
			if err := run("", nil, clang, args...); err != nil {
				return nil, nil, nil, fmt.Errorf("compile Swift package target %s: %w", node.Target.Name, err)
			}
		}
		objects = append(objects, spec.ClangObjects...)
	}
	for _, copyItem := range spec.ResourceCopy {
		if dirExists(copyItem.Source) {
			if err := copyDir(copyItem.Source, copyItem.Dest); err != nil {
				return nil, nil, nil, err
			}
		} else if fileExists(copyItem.Source) {
			if err := copyFile(copyItem.Source, copyItem.Dest); err != nil {
				return nil, nil, nil, err
			}
		} else {
			return nil, nil, nil, fmt.Errorf("Swift package target %s declares resource %s which does not exist", node.Target.Name, copyItem.Source)
		}
	}
	if len(spec.ResourceCopy) > 0 {
		bundle, err := swiftPMResourceBundle(node, work, minOS)
		if err != nil {
			return nil, nil, nil, err
		}
		resources = append(resources, bundle)
	}
	modulePaths = spec.IncludeDirs
	return objects, resources, modulePaths, nil
}

// swiftPMTargetMinOS folds the package's declared iOS platform version into the
// app's deployment target, mirroring SwiftPM/Xcode: the higher of the two wins.
func swiftPMTargetMinOS(pkg *swiftPMPackage, appMinOS string) string {
	minOS := appMinOS
	for _, platform := range pkg.Manifest.Platforms {
		if strings.EqualFold(platform.PlatformName, "ios") && platform.Version != "" {
			minOS = maxVersion(minOS, platform.Version)
		}
	}
	return minOS
}

// swiftPMLanguageMode picks the Swift language mode for a target. SwiftPM keys
// this off the manifest's swift-tools-version unless the target overrides it.
func swiftPMLanguageMode(pkg *swiftPMPackage, settings swiftPMTargetSettings) string {
	// A per-target swiftSettings override wins.
	if settings.SwiftLanguageMode != "" {
		return swiftLanguageVersion(settings.SwiftLanguageMode)
	}
	// Then the package-level `swiftLanguageModes:` declaration. Honouring it is
	// not cosmetic: swift-perception declares swift-tools-version 6.0 with
	// `swiftLanguageModes: [.v5]`, and compiling its sources in Swift 6 mode
	// turns strict-concurrency diagnostics into hard errors.
	if len(pkg.Manifest.SwiftLanguageVersions) > 0 {
		lowest := pkg.Manifest.SwiftLanguageVersions[0]
		for _, value := range pkg.Manifest.SwiftLanguageVersions[1:] {
			if maxVersion(lowest, value) == lowest {
				lowest = value
			}
		}
		return swiftLanguageVersion(lowest)
	}
	// Otherwise SwiftPM derives the mode from the tools version.
	if pkg.Manifest.ToolsVersion.Version != "" && maxVersion(pkg.Manifest.ToolsVersion.Version, "6.0") == pkg.Manifest.ToolsVersion.Version {
		return "6"
	}
	return "5"
}

// swiftPMPublicHeadersDir resolves a target's public header directory. SwiftPM
// defaults to "include" for targets containing C-family sources.
func swiftPMPublicHeadersDir(root string, target swiftPMTarget) string {
	if target.PublicHeadersPath != nil {
		if *target.PublicHeadersPath == "" {
			return ""
		}
		return filepath.Join(root, *target.PublicHeadersPath)
	}
	if include := filepath.Join(root, "include"); dirExists(include) {
		return include
	}
	return ""
}

// swiftPMBundleName is SwiftPM's resource bundle name: <package>_<target>.bundle.
// Bundle.module / SWIFTPM_MODULE_BUNDLE resolve exactly this name at runtime, so
// it must match or resource lookup fails on device.
func swiftPMBundleName(pkg *swiftPMPackage, target swiftPMTarget) string {
	return pkg.Manifest.Name + "_" + target.Name
}

// swiftPMModuleMapDir synthesizes the module map that lets Swift targets
// `import` a C-family target, the same way SwiftPM does for a target with a
// publicHeadersPath. When the header directory already ships a module.modulemap
// (Flutter plugins often do) that one is authoritative and is used unchanged.
//
// The returned directory is passed with -I, which is how clang discovers a
// module.modulemap sitting in a header search path.
func swiftPMModuleMapDir(moduleName, publicHeaders, work string) (string, error) {
	if publicHeaders == "" {
		return "", nil
	}
	for _, name := range []string{"module.modulemap", "module.map"} {
		if fileExists(filepath.Join(publicHeaders, name)) {
			return publicHeaders, nil
		}
	}
	dir := filepath.Join(work, "modulemaps", moduleName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	text := "module " + moduleName + " {\n  umbrella \"" + publicHeaders + "\"\n  export *\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "module.modulemap"), []byte(text), 0o644); err != nil {
		return "", err
	}
	return dir, nil
}

const swiftPMResourceAccessorSwift = `// Generated by ripley to match SwiftPM's resource_bundle_accessor.swift.
import class Foundation.Bundle
import class Foundation.ProcessInfo
import struct Foundation.URL

private class BundleFinder {}

extension Foundation.Bundle {
    static let module: Bundle = {
        let bundleName = "%s"
        var candidates = [
            Bundle.main.resourceURL,
            Bundle(for: BundleFinder.self).resourceURL,
            Bundle.main.bundleURL,
        ]
        if let override = ProcessInfo.processInfo.environment["PACKAGE_RESOURCE_BUNDLE_PATH"] {
            candidates.append(URL(fileURLWithPath: override))
        }
        for candidate in candidates {
            let path = candidate?.appendingPathComponent(bundleName + ".bundle")
            if let bundle = path.flatMap(Bundle.init(url:)) {
                return bundle
            }
        }
        fatalError("unable to find bundle named %s")
    }()
}
`

const swiftPMResourceAccessorObjCHeader = `// Generated by ripley to match SwiftPM's resource_bundle_accessor.h.
#import <Foundation/Foundation.h>
NS_ASSUME_NONNULL_BEGIN
NSBundle* %s_SWIFTPM_MODULE_BUNDLER(void);
#define SWIFTPM_MODULE_BUNDLE %s_SWIFTPM_MODULE_BUNDLER()
NS_ASSUME_NONNULL_END
`

const swiftPMResourceAccessorObjCSource = `// Generated by ripley to match SwiftPM's resource_bundle_accessor.m.
#import <Foundation/Foundation.h>

NSBundle* %s_SWIFTPM_MODULE_BUNDLER(void) {
    NSString *bundleName = @"%s";
    NSMutableArray *candidates = [NSMutableArray array];
    if (NSBundle.mainBundle.resourceURL) { [candidates addObject:NSBundle.mainBundle.resourceURL]; }
    NSBundle *container = [NSBundle bundleForClass:NSClassFromString(@"NSObject")];
    if (container.resourceURL) { [candidates addObject:container.resourceURL]; }
    [candidates addObject:NSBundle.mainBundle.bundleURL];
    for (NSURL *candidate in candidates) {
        NSURL *url = [candidate URLByAppendingPathComponent:[bundleName stringByAppendingString:@".bundle"]];
        NSBundle *bundle = [NSBundle bundleWithURL:url];
        if (bundle != nil) { return bundle; }
    }
    [NSException raise:@"SwiftPMResourceBundleNotFound" format:@"unable to find bundle named %s"];
    return nil;
}
`

// swiftPMResourceAccessors writes the Bundle.module / SWIFTPM_MODULE_BUNDLE
// shims for a target that ships resources. Without them any source using
// Bundle.module fails to compile, which is how SwiftPM targets normally reach
// their resources.
func swiftPMResourceAccessors(pkg *swiftPMPackage, target swiftPMTarget, work string, hasSwift, hasClang bool) (swiftFiles, clangFiles []string, includeDir string, err error) {
	bundleName := swiftPMBundleName(pkg, target)
	dir := filepath.Join(work, "accessors", pkg.Manifest.Name, target.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, "", err
	}
	if hasSwift {
		path := filepath.Join(dir, "resource_bundle_accessor.swift")
		body := fmt.Sprintf(swiftPMResourceAccessorSwift, bundleName, bundleName)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return nil, nil, "", err
		}
		swiftFiles = append(swiftFiles, path)
	}
	if hasClang {
		module := swiftPMModuleName(target.Name)
		header := filepath.Join(dir, "resource_bundle_accessor.h")
		if err := os.WriteFile(header, []byte(fmt.Sprintf(swiftPMResourceAccessorObjCHeader, module, module)), 0o644); err != nil {
			return nil, nil, "", err
		}
		source := filepath.Join(dir, "resource_bundle_accessor.m")
		if err := os.WriteFile(source, []byte(fmt.Sprintf(swiftPMResourceAccessorObjCSource, module, bundleName, bundleName)), 0o644); err != nil {
			return nil, nil, "", err
		}
		clangFiles = append(clangFiles, source)
	}
	return swiftFiles, clangFiles, dir, nil
}

// swiftPMTargetCompileSpec resolves everything needed to compile one target into
// explicit command lines, without running anything. Keeping composition separate
// from execution is what makes the flag contract testable off-device.
func swiftPMTargetCompileSpec(tc toolchain.Toolchain, node swiftPMGraphTarget, generated []string, work, appMinOS string, includeDirs, frameworkPaths, macroPlugins []string) (*swiftPMCompileSpec, error) {
	pkg := node.Package
	target := node.Target
	root, err := swiftPMTargetRoot(pkg, target)
	if err != nil {
		return nil, err
	}
	settings, err := decodeSwiftPMSettings(root, target)
	if err != nil {
		return nil, err
	}

	resourcePaths := make(map[string]bool)
	var copies []swiftPMResourceCopy
	outDir := filepath.Join(work, "targets", pkg.Manifest.Name, target.Name)
	bundleDir := filepath.Join(outDir, swiftPMBundleName(pkg, target)+".bundle")
	for _, resource := range target.Resources {
		source := filepath.Clean(filepath.Join(root, resource.Path))
		kind := resource.kind()
		switch kind {
		case "process", "copy":
		case "embedInCode":
			return nil, fmt.Errorf("resource %s uses .embedInCode, which requires SwiftPM's code generator", resource.Path)
		default:
			return nil, fmt.Errorf("resource %s has unsupported rule %q", resource.Path, kind)
		}
		resourcePaths[source] = true
		dest := filepath.Join(bundleDir, filepath.Base(source))
		if kind == "copy" {
			dest = filepath.Join(bundleDir, resource.Path)
		}
		copies = append(copies, swiftPMResourceCopy{Source: source, Dest: dest, Process: kind == "process"})
	}

	swiftSources, clangSources, err := swiftPMSourceFiles(root, target, resourcePaths)
	if err != nil {
		return nil, err
	}
	for _, path := range generated {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".swift":
			swiftSources = append(swiftSources, path)
		case ".c", ".m", ".mm", ".cc", ".cpp", ".cxx":
			clangSources = append(clangSources, path)
		}
	}
	if len(swiftSources) == 0 && len(clangSources) == 0 {
		if len(copies) == 0 {
			return nil, fmt.Errorf("no sources found under %s", root)
		}
		// Resource-only target: nothing to compile, but the bundle still ships.
		return &swiftPMCompileSpec{ModuleName: swiftPMModuleName(target.Name), ResourceCopy: copies}, nil
	}

	spec := &swiftPMCompileSpec{ModuleName: swiftPMModuleName(target.Name), ResourceCopy: copies}
	if len(copies) > 0 {
		extraSwift, extraClang, accessorDir, err := swiftPMResourceAccessors(pkg, target, work, len(swiftSources) > 0, len(clangSources) > 0)
		if err != nil {
			return nil, err
		}
		swiftSources = append(swiftSources, extraSwift...)
		clangSources = append(clangSources, extraClang...)
		settings.HeaderSearchPaths = append(settings.HeaderSearchPaths, accessorDir)
	}
	sort.Strings(swiftSources)
	sort.Strings(clangSources)
	spec.SwiftSources = swiftSources
	spec.ClangSources = clangSources

	minOS := swiftPMTargetMinOS(pkg, appMinOS)
	objectsDir := filepath.Join(outDir, "objects")
	if err := os.MkdirAll(objectsDir, 0o755); err != nil {
		return nil, err
	}
	publicHeaders := swiftPMPublicHeadersDir(root, target)
	spec.PublicHeaders = publicHeaders

	// Header search: the target's own root and public headers, its declared
	// headerSearchPaths, then whatever its dependencies published.
	headerPaths := []string{root}
	if publicHeaders != "" {
		headerPaths = append(headerPaths, publicHeaders)
	}
	headerPaths = append(headerPaths, settings.HeaderSearchPaths...)
	headerPaths = append(headerPaths, includeDirs...)
	headerPaths = uniqueStrings(headerPaths)

	published := []string{}
	if publicHeaders != "" {
		moduleMapDir, err := swiftPMModuleMapDir(spec.ModuleName, publicHeaders, work)
		if err != nil {
			return nil, err
		}
		published = append(published, publicHeaders)
		if moduleMapDir != "" && moduleMapDir != publicHeaders {
			published = append(published, moduleMapDir)
			headerPaths = append(headerPaths, moduleMapDir)
		}
	}

	if len(swiftSources) > 0 {
		flags, err := commonSwiftFlags(tc, minOS)
		if err != nil {
			return nil, err
		}
		search, err := frameworkSearch(tc)
		if err != nil {
			return nil, err
		}
		moduleDir := filepath.Join(outDir, "module")
		if err := os.MkdirAll(moduleDir, 0o755); err != nil {
			return nil, err
		}
		spec.SwiftObject = filepath.Join(objectsDir, spec.ModuleName+"-swift.o")
		spec.SwiftModule = filepath.Join(moduleDir, spec.ModuleName+".swiftmodule")
		spec.SwiftHeader = filepath.Join(moduleDir, spec.ModuleName+"-Swift.h")

		args := append([]string(nil), flags...)
		args = append(args, search...)
		args = append(args, "-module-name", spec.ModuleName, "-swift-version", swiftPMLanguageMode(pkg, settings))
		// SwiftPM always defines SWIFT_PACKAGE when building a package target,
		// and real packages branch on it: GRDB picks `import GRDBSQLite` (its own
		// system-library module) under SWIFT_PACKAGE and `import SQLite3`
		// otherwise, so omitting it silently compiles the wrong sources.
		args = append(args, "-DSWIFT_PACKAGE", "-Xcc", "-DSWIFT_PACKAGE")
		for _, path := range headerPaths {
			args = append(args, "-Xcc", "-I", "-Xcc", path)
		}
		for _, define := range settings.CDefines {
			args = append(args, "-Xcc", "-D"+define)
		}
		for _, define := range settings.SwiftDefines {
			args = append(args, "-D"+define)
		}
		for _, dir := range includeDirs {
			args = append(args, "-I", dir)
		}
		args = append(args, "-I", moduleDir)
		for _, path := range frameworkPaths {
			args = append(args, "-F", path)
		}
		args = append(args, settings.SwiftUnsafeFlags...)
		for _, plugin := range macroPlugins {
			args = append(args, "-load-plugin-executable", plugin)
		}
		// `package` access needs the package name SwiftPM would have passed;
		// without it, any `package func` in the target fails to compile.
		if target.PackageAccess {
			args = append(args, "-package-name", pkg.Manifest.Name)
		}
		args = append(args, swiftSources...)
		args = append(args, "-emit-module", "-emit-module-path", spec.SwiftModule,
			"-emit-objc-header-path", spec.SwiftHeader, "-o", spec.SwiftObject)
		spec.SwiftArgs = args
		published = append(published, moduleDir)
	}

	if len(clangSources) > 0 {
		sdk, err := tc.IOSSDK()
		if err != nil {
			return nil, err
		}
		search, err := frameworkSearch(tc)
		if err != nil {
			return nil, err
		}
		for _, path := range frameworkPaths {
			search = append(search, "-F", path)
		}
		for _, source := range clangSources {
			obj := filepath.Join(objectsDir, podObjectName(source))
			// SWIFT_PACKAGE is defined for the C-family sources too: SwiftPM
			// passes it to every compiler it drives, and package headers branch
			// on it exactly like the Swift sources do.
			args := []string{"-target", "arm64-apple-ios" + minOS, "-isysroot", sdk, "-fmodules", "-O2", "-fobjc-arc", "-DSWIFT_PACKAGE"}
			for _, define := range settings.CDefines {
				args = append(args, "-D"+define)
			}
			for _, path := range headerPaths {
				args = append(args, "-I", path)
			}
			args = append(args, search...)
			if standard := pkg.Manifest.CLanguageStandard; standard != nil && *standard != "" {
				if ext := strings.ToLower(filepath.Ext(source)); ext == ".c" || ext == ".m" {
					args = append(args, "-std="+*standard)
				}
			}
			if standard := pkg.Manifest.CxxLanguageStandard; standard != nil && *standard != "" {
				if ext := strings.ToLower(filepath.Ext(source)); ext == ".cc" || ext == ".cpp" || ext == ".cxx" || ext == ".mm" {
					args = append(args, "-std="+*standard)
				}
			}
			args = append(args, settings.CUnsafeFlags...)
			args = append(args, "-c", source, "-o", obj)
			spec.ClangArgs = append(spec.ClangArgs, args)
			spec.ClangObjects = append(spec.ClangObjects, obj)
		}
	}

	spec.IncludeDirs = uniqueStrings(published)
	return spec, nil
}

// swiftPMResourceBundle finishes a target's resource bundle by writing the
// Info.plist device builds require, and returns the bundle directory.
func swiftPMResourceBundle(node swiftPMGraphTarget, work, appMinOS string) (string, error) {
	pkg := node.Package
	name := swiftPMBundleName(pkg, node.Target)
	bundle := filepath.Join(work, "targets", pkg.Manifest.Name, node.Target.Name, name+".bundle")
	if !dirExists(bundle) {
		return "", fmt.Errorf("resource bundle %s was not created", bundle)
	}
	info := map[string]any{
		"CFBundleIdentifier":         "org.swift.swiftpm." + strings.ReplaceAll(name, "_", "-"),
		"CFBundleName":               name,
		"CFBundlePackageType":        "BNDL",
		"CFBundleShortVersionString": "1.0",
		"CFBundleVersion":            "1",
		"MinimumOSVersion":           swiftPMTargetMinOS(pkg, appMinOS),
	}
	if err := writePlist(filepath.Join(bundle, "Info.plist"), info); err != nil {
		return "", err
	}
	return bundle, nil
}

// swiftPMSystemLibraryInclude resolves a `system` library target: a directory
// holding a module map over headers that are already installed on the machine
// (SQLite is the common case). Nothing is compiled — the module map is handed to
// dependents on the include path, exactly as SwiftPM does.
//
// The module map's `link "name"` directives are returned so the caller can pass
// the corresponding -l flags to the linker.
func swiftPMSystemLibraryInclude(pkg *swiftPMPackage, target swiftPMTarget) (include string, linkedLibraries []string, err error) {
	root, err := swiftPMTargetRoot(pkg, target)
	if err != nil {
		return "", nil, fmt.Errorf("package %s system library target %s: %w", pkg.Manifest.Name, target.Name, err)
	}
	var moduleMap string
	for _, name := range []string{"module.modulemap", "module.map"} {
		if candidate := filepath.Join(root, name); fileExists(candidate) {
			moduleMap = candidate
			break
		}
	}
	if moduleMap == "" {
		return "", nil, fmt.Errorf("package %s system library target %s has no module.modulemap in %s", pkg.Manifest.Name, target.Name, root)
	}
	data, err := os.ReadFile(moduleMap)
	if err != nil {
		return "", nil, err
	}
	for _, match := range swiftPMModuleMapLinkRe.FindAllStringSubmatch(string(data), -1) {
		linkedLibraries = append(linkedLibraries, match[1])
	}
	return root, uniqueStrings(linkedLibraries), nil
}

var swiftPMModuleMapLinkRe = regexp.MustCompile(`(?m)^\s*link\s+"([^"]+)"`)

// buildSwiftPMMacroPlugin compiles a `macro` target into a host executable that
// swiftc loads with -load-plugin-executable while compiling the iOS targets that
// use the macro.
//
// A macro plugin runs on the build machine, not the device, so it is built for
// the host triple. It is linked against the SwiftSyntax libraries that ship with
// the Swift toolchain rather than the swift-syntax package sources in the
// dependency graph: building swift-syntax from source needs far more memory than
// a small Linux builder has (it OOMs at -O), while the prebuilt host modules are
// the same version the compiler itself uses.
func buildSwiftPMMacroPlugin(tc toolchain.Toolchain, node swiftPMGraphTarget, work string) (string, error) {
	pkg := node.Package
	target := node.Target
	root, err := swiftPMTargetRoot(pkg, target)
	if err != nil {
		return "", fmt.Errorf("package %s macro target %s: %w", pkg.Manifest.Name, target.Name, err)
	}
	sources, _, err := swiftPMSourceFiles(root, target, nil)
	if err != nil {
		return "", err
	}
	if len(sources) == 0 {
		return "", fmt.Errorf("package %s macro target %s has no Swift sources under %s", pkg.Manifest.Name, target.Name, root)
	}
	swiftc, err := swiftCompiler(tc)
	if err != nil {
		return "", err
	}
	hostModules, err := swiftPMHostPluginModules(tc)
	if err != nil {
		return "", err
	}
	outDir := filepath.Join(work, "macros", pkg.Manifest.Name)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	executable := filepath.Join(outDir, target.Name)

	// Building a macro plugin costs a full optimized swiftc invocation over the
	// whole macro target, and every dependent target in the graph asks for it.
	// Reuse the executable while it is newer than every source that produced it,
	// matching how the SwiftCompilerPlugin support module below is cached.
	fresh, err := swiftPMExecutableIsFresh(executable, sources)
	if err != nil {
		return "", err
	}
	if fresh {
		return executable, nil
	}

	// SwiftCompilerPlugin is the only macro-facing module the toolchain does not
	// ship prebuilt (it is a thin @main entry point over the message handler), so
	// it is compiled from the swift-syntax checkout already in the graph.
	pluginSupport, err := swiftPMCompilerPluginModule(tc, node, hostModules, outDir)
	if err != nil {
		return "", err
	}

	settings, err := decodeSwiftPMSettings(root, target)
	if err != nil {
		return "", err
	}
	args := []string{
		"-swift-version", swiftPMLanguageMode(pkg, settings),
		"-I", hostModules, "-L", hostModules,
		"-I", pluginSupport, "-L", pluginSupport,
		"-lSwiftCompilerPlugin",
		"-Xlinker", "-rpath", "-Xlinker", hostModules,
		"-Xlinker", "-rpath", "-Xlinker", pluginSupport,
		"-parse-as-library", "-O",
	}
	if target.PackageAccess {
		args = append(args, "-package-name", pkg.Manifest.Name)
	}
	args = append(args, settings.SwiftUnsafeFlags...)
	args = append(args, sources...)
	args = append(args, "-o", executable)
	if err := run(root, nil, swiftc, args...); err != nil {
		return "", fmt.Errorf("build macro plugin %s of package %s: %w", target.Name, pkg.Manifest.Name, err)
	}
	return executable, nil
}

// swiftPMExecutableIsFresh reports whether executable exists and is at least as
// new as every one of sources.
func swiftPMExecutableIsFresh(executable string, sources []string) (bool, error) {
	info, err := os.Stat(executable)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	built := info.ModTime()
	for _, source := range sources {
		stat, err := os.Stat(source)
		if err != nil {
			return false, err
		}
		if stat.ModTime().After(built) {
			return false, nil
		}
	}
	return true, nil
}

// swiftPMHostPluginModules is the toolchain directory holding the prebuilt
// host SwiftSyntax modules a macro plugin links against.
func swiftPMHostPluginModules(tc toolchain.Toolchain) (string, error) {
	lib, err := tc.SwiftLib()
	if err != nil {
		return "", err
	}
	host := filepath.Join(lib, "host")
	if !dirExists(filepath.Join(host, "SwiftSyntaxMacros.swiftmodule")) {
		return "", fmt.Errorf("Swift toolchain at %s does not ship prebuilt host SwiftSyntax modules (looked for %s); SwiftPM macro targets cannot be built without them", lib, filepath.Join(host, "SwiftSyntaxMacros.swiftmodule"))
	}
	return host, nil
}

// swiftPMCompilerPluginModule builds the SwiftCompilerPlugin module from the
// swift-syntax package in the dependency graph, against the toolchain's prebuilt
// host modules. Result is cached per toolchain+revision under the work tree.
func swiftPMCompilerPluginModule(tc toolchain.Toolchain, node swiftPMGraphTarget, hostModules, outDir string) (string, error) {
	syntax := swiftPMFindPackage(node, "swift-syntax")
	if syntax == nil {
		return "", fmt.Errorf("package %s macro target %s needs the swift-syntax package (for SwiftCompilerPlugin) but it is not in the dependency graph", node.Package.Manifest.Name, node.Target.Name)
	}
	sourceDir := filepath.Join(syntax.Root, "Sources", "SwiftCompilerPlugin")
	if !dirExists(sourceDir) {
		return "", fmt.Errorf("swift-syntax checkout %s has no Sources/SwiftCompilerPlugin", syntax.Root)
	}
	dir := filepath.Join(outDir, ".compiler-plugin-support")
	built := filepath.Join(dir, "libSwiftCompilerPlugin.so")
	if fileExists(built) {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	sources, _, err := swiftPMSourceFiles(sourceDir, swiftPMTarget{Name: "SwiftCompilerPlugin"}, nil)
	if err != nil {
		return "", err
	}
	if len(sources) == 0 {
		return "", fmt.Errorf("no Swift sources in %s", sourceDir)
	}
	swiftc, err := swiftCompiler(tc)
	if err != nil {
		return "", err
	}
	args := []string{
		"-emit-library", "-emit-module", "-module-name", "SwiftCompilerPlugin",
		"-I", hostModules, "-L", hostModules,
		"-lSwiftSyntaxMacros", "-lSwiftCompilerPluginMessageHandling",
		"-Xlinker", "-rpath", "-Xlinker", hostModules,
		"-swift-version", "5", "-O",
		"-emit-module-path", filepath.Join(dir, "SwiftCompilerPlugin.swiftmodule"),
	}
	args = append(args, sources...)
	args = append(args, "-o", built)
	if err := run(dir, nil, swiftc, args...); err != nil {
		return "", fmt.Errorf("build SwiftCompilerPlugin from %s: %w", sourceDir, err)
	}
	return dir, nil
}

// swiftPMFindPackage locates a package by identity or manifest name among the
// dependencies reachable from a node's package.
func swiftPMFindPackage(node swiftPMGraphTarget, identity string) *swiftPMPackage {
	seen := make(map[*swiftPMPackage]bool)
	var walk func(pkg *swiftPMPackage) *swiftPMPackage
	walk = func(pkg *swiftPMPackage) *swiftPMPackage {
		if pkg == nil || seen[pkg] {
			return nil
		}
		seen[pkg] = true
		if pkg.Identity == identity || strings.EqualFold(pkg.Manifest.Name, identity) {
			return pkg
		}
		for _, dep := range pkg.Dependencies {
			if found := walk(dep); found != nil {
				return found
			}
		}
		return nil
	}
	return walk(node.Package)
}
