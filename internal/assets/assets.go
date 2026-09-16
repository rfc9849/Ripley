package assets

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var variantDirRE = regexp.MustCompile(`^(\d+(\.\d*)?)x$`)

const licenseSeparator = "\n--------------------------------------------------------------------------------\n"

type transformer struct {
	Package string   `yaml:"package"`
	Args    []string `yaml:"args"`
}

type assetEntry struct {
	Path         string
	Flavors      []string
	Platforms    []string
	Transformers []transformer
}

func (e *assetEntry) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		if node.Tag == "!!null" || node.Value == "" {
			return nil
		}
		e.Path = node.Value
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("asset manifest entry must be a string or map")
	}
	var raw struct {
		Path         string        `yaml:"path"`
		Flavors      []string      `yaml:"flavors"`
		Platforms    []string      `yaml:"platforms"`
		Transformers []transformer `yaml:"transformers"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	if raw.Path == "" {
		return errors.New("asset manifest map is missing path")
	}
	for _, transformer := range raw.Transformers {
		if transformer.Package == "" {
			return errors.New("asset transformer is missing package")
		}
	}
	e.Path = raw.Path
	e.Flavors = raw.Flavors
	e.Platforms = raw.Platforms
	e.Transformers = raw.Transformers
	return nil
}

type fontAsset struct {
	Weight *int   `yaml:"weight,omitempty" json:"weight,omitempty"`
	Style  string `yaml:"style,omitempty" json:"style,omitempty"`
	Asset  string `yaml:"asset" json:"asset"`
}

type font struct {
	Family string      `yaml:"family" json:"family"`
	Fonts  []fontAsset `yaml:"fonts" json:"fonts"`
}

type pubspec struct {
	Name            string `yaml:"name"`
	FlutterNonEmpty bool   `yaml:"-"`
	Flutter         struct {
		UsesMaterial bool         `yaml:"uses-material-design"`
		Assets       []assetEntry `yaml:"assets"`
		Shaders      []assetEntry `yaml:"shaders"`
		Fonts        []font       `yaml:"fonts"`
		Licenses     []string     `yaml:"licenses"`
	} `yaml:"flutter"`
}

func loadPubspec(path string) (pubspec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return pubspec{}, err
	}
	var spec pubspec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return pubspec{}, fmt.Errorf("parse %s: %w", path, err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return pubspec{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(document.Content) > 0 && document.Content[0].Kind == yaml.MappingNode {
		root := document.Content[0]
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == "flutter" {
				value := root.Content[i+1]
				spec.FlutterNonEmpty = value.Kind == yaml.MappingNode && len(value.Content) > 0
				break
			}
		}
	}
	spec.Flutter.Assets = nonEmptyAssetEntries(spec.Flutter.Assets)
	spec.Flutter.Shaders = nonEmptyAssetEntries(spec.Flutter.Shaders)
	fonts := spec.Flutter.Fonts[:0]
	for _, font := range spec.Flutter.Fonts {
		if font.Family != "" && len(font.Fonts) > 0 {
			fonts = append(fonts, font)
		}
	}
	spec.Flutter.Fonts = fonts
	return spec, nil
}

func nonEmptyAssetEntries(entries []assetEntry) []assetEntry {
	out := entries[:0]
	for _, entry := range entries {
		if entry.Path != "" {
			out = append(out, entry)
		}
	}
	return out
}

type packageLocation struct {
	Root        string
	PackageRoot string
}

type packageConfig struct {
	Packages  map[string]packageLocation
	Order     []string
	GraphPath string
}

func loadPackageConfig(path string) (packageConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return packageConfig{}, err
	}
	var raw struct {
		Packages []struct {
			Name       string `json:"name"`
			RootURI    string `json:"rootUri"`
			PackageURI string `json:"packageUri"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return packageConfig{}, fmt.Errorf("decode %s: %w", path, err)
	}
	config := packageConfig{Packages: make(map[string]packageLocation), GraphPath: filepath.Join(filepath.Dir(path), "package_graph.json")}
	for _, pkg := range raw.Packages {
		u, err := url.Parse(pkg.RootURI)
		if err != nil || u.Scheme != "file" {
			continue
		}
		root, err := url.PathUnescape(u.Path)
		if err != nil {
			return packageConfig{}, fmt.Errorf("decode rootUri for package %s: %w", pkg.Name, err)
		}
		packageURI := pkg.PackageURI
		if packageURI == "" {
			packageURI = "lib/"
		}
		config.Packages[pkg.Name] = packageLocation{
			Root:        root,
			PackageRoot: filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(packageURI, "/"))),
		}
		config.Order = append(config.Order, pkg.Name)
	}
	return config, nil
}

type packageGraph struct {
	Dependencies    map[string][]string
	DevDependencies map[string][]string
}

func loadPackageGraph(path string) (packageGraph, error) {
	graph := packageGraph{Dependencies: make(map[string][]string), DevDependencies: make(map[string][]string)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return graph, nil
	}
	if err != nil {
		return packageGraph{}, err
	}
	var raw struct {
		Packages []struct {
			Name            string   `json:"name"`
			Dependencies    []string `json:"dependencies"`
			DevDependencies []string `json:"devDependencies"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return packageGraph{}, fmt.Errorf("decode %s: %w", path, err)
	}
	for _, pkg := range raw.Packages {
		graph.Dependencies[pkg.Name] = pkg.Dependencies
		graph.DevDependencies[pkg.Name] = pkg.DevDependencies
	}
	return graph, nil
}

type packageDependency struct {
	Name         string
	ExclusiveDev bool
}

func transitiveDependencies(appName string, graph packageGraph, config packageConfig) []packageDependency {
	deps, hasGraph := graph.Dependencies[appName]
	if !hasGraph {
		out := make([]packageDependency, 0, len(config.Order))
		for _, name := range config.Order {
			if name == appName {
				continue
			}
			out = append(out, packageDependency{Name: name})
		}
		return out
	}

	dev := map[string]bool{appName: true}
	order := []string{appName}
	toVisit := append(append([]string{}, deps...), graph.DevDependencies[appName]...)
	for len(toVisit) > 0 {
		last := len(toVisit) - 1
		current := toVisit[last]
		toVisit = toVisit[:last]
		if _, seen := dev[current]; seen {
			continue
		}
		dev[current] = true
		order = append(order, current)
		toVisit = append(toVisit, graph.Dependencies[current]...)
	}
	visited := make(map[string]bool)
	toVisit = []string{appName}
	for len(toVisit) > 0 {
		last := len(toVisit) - 1
		current := toVisit[last]
		toVisit = toVisit[:last]
		if visited[current] {
			continue
		}
		visited[current] = true
		if _, exists := dev[current]; exists {
			dev[current] = false
		}
		toVisit = append(toVisit, graph.Dependencies[current]...)
	}
	out := make([]packageDependency, 0, len(order))
	for _, name := range order {
		out = append(out, packageDependency{Name: name, ExclusiveDev: dev[name]})
	}
	return out
}

type assetKind string

const (
	regularAsset  assetKind = "regular"
	fontAssetKind assetKind = "font"
	shaderAsset   assetKind = "shader"
)

type asset struct {
	BaseDir      string
	RelativeURI  string
	EntryURI     string
	Package      string
	Kind         assetKind
	OriginURI    string
	Flavors      []string
	Platforms    []string
	Transformers []transformer
}

func (a asset) file() string {
	rel, err := url.PathUnescape(a.RelativeURI)
	if err != nil {
		rel = a.RelativeURI
	}
	return filepath.Join(a.BaseDir, filepath.FromSlash(rel))
}

func (a asset) symbolicPrefix() string {
	if a.EntryURI == a.RelativeURI {
		return ""
	}
	if idx := strings.Index(a.EntryURI, a.RelativeURI); idx >= 0 {
		return a.EntryURI[:idx]
	}
	return ""
}

func (a asset) matches(flavor, platform string) bool {
	if len(a.Flavors) > 0 && (flavor == "" || !containsString(a.Flavors, flavor)) {
		return false
	}
	return len(a.Platforms) == 0 || containsString(a.Platforms, platform)
}

func (a asset) identity() string {
	flavors := append([]string{}, a.Flavors...)
	platforms := append([]string{}, a.Platforms...)
	sort.Strings(flavors)
	sort.Strings(platforms)
	return strings.Join([]string{a.BaseDir, a.RelativeURI, a.EntryURI, string(a.Kind), strings.Join(flavors, ","), strings.Join(platforms, ",")}, "\x00")
}

type assetVariants struct {
	Main     asset
	Variants []asset
}

type dirCache struct {
	variantsByDir map[string][]string
	cache         map[string][]string
}

func newDirCache() *dirCache {
	return &dirCache{variantsByDir: make(map[string][]string), cache: make(map[string][]string)}
}

func (c *dirCache) variantsFor(assetPath string) ([]string, error) {
	if cached, ok := c.cache[assetPath]; ok {
		return cached, nil
	}
	directory := filepath.Dir(assetPath)
	if !dirExists(directory) {
		return nil, nil
	}
	potential, ok := c.variantsByDir[directory]
	if !ok {
		entries, err := os.ReadDir(directory)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() || !variantDirRE.MatchString(entry.Name()) {
				continue
			}
			children, err := os.ReadDir(filepath.Join(directory, entry.Name()))
			if err != nil {
				return nil, err
			}
			for _, child := range children {
				if !child.IsDir() {
					potential = append(potential, filepath.Join(directory, entry.Name(), child.Name()))
				}
			}
		}
		c.variantsByDir[directory] = potential
	}
	var out []string
	if fileExists(assetPath) {
		out = append(out, assetPath)
	}
	for _, path := range potential {
		if filepath.Base(path) == filepath.Base(assetPath) {
			out = append(out, path)
		}
	}
	c.cache[assetPath] = out
	return out, nil
}

type bundleEntry struct {
	Data         []byte
	Source       string
	Generated    bool
	Kind         assetKind
	Transformers []transformer
}

type AssetBundle struct {
	ProjectRoot    string
	FlutterRoot    string
	TargetPlatform string
	Flavor         string
	Entries        map[string]bundleEntry
	InputFiles     []string
	// PackageConfigPath is the package config that governs this project. Under
	// pub workspace resolution it lives at the workspace root, not below the
	// member, so the caller resolves it and passes it in.
	PackageConfigPath string
	packageConfig     packageConfig
}

func NewAssetBundle(projectRoot, flutterRoot, targetPlatform, flavor, packageConfigPath string) (*AssetBundle, error) {
	projectRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, err
	}
	if packageConfigPath == "" {
		packageConfigPath = filepath.Join(projectRoot, ".dart_tool", "package_config.json")
	}
	return &AssetBundle{
		ProjectRoot:       projectRoot,
		FlutterRoot:       flutterRoot,
		TargetPlatform:    targetPlatform,
		Flavor:            flavor,
		PackageConfigPath: packageConfigPath,
		Entries:           make(map[string]bundleEntry),
	}, nil
}

func (b *AssetBundle) resolveAsset(assetBase, uri, packageName, attributedPackage string, kind assetKind, originURI string, flavors, platforms []string, transformers []transformer) (asset, bool) {
	parts := strings.Split(strings.Trim(uri, "/"), "/")
	decoded, _ := url.PathUnescape(uri)
	if len(parts) > 0 && parts[0] == "packages" && !fileExists(filepath.Join(assetBase, filepath.FromSlash(decoded))) {
		if len(parts) > 2 {
			if pkg, ok := b.packageConfig.Packages[parts[1]]; ok {
				rel := strings.Join(parts[2:], "/")
				return asset{BaseDir: pkg.PackageRoot, RelativeURI: rel, EntryURI: uri, Package: attributedPackage, Kind: kind, OriginURI: firstNonEmpty(originURI, uri), Flavors: flavors, Platforms: platforms, Transformers: transformers}, true
			}
		}
		fmt.Fprintf(os.Stderr, "Error: could not resolve package for asset %s\n", uri)
		return asset{}, false
	}
	entryURI := uri
	if packageName != "" {
		entryURI = "packages/" + packageName + "/" + uri
	}
	return asset{BaseDir: assetBase, RelativeURI: uri, EntryURI: entryURI, Package: attributedPackage, Kind: kind, OriginURI: firstNonEmpty(originURI, entryURI), Flavors: flavors, Platforms: platforms, Transformers: transformers}, true
}

func (b *AssetBundle) parseFile(cache *dirCache, result *[]assetVariants, assetBase, uri, packageName, attributedPackage string, kind assetKind, originURI string, flavors, platforms []string, transformers []transformer) error {
	mainAsset, ok := b.resolveAsset(assetBase, uri, packageName, attributedPackage, kind, originURI, flavors, platforms, transformers)
	if !ok {
		return nil
	}
	for _, existing := range *result {
		if existing.Main.EntryURI == mainAsset.EntryURI && !sameStrings(existing.Main.Flavors, mainAsset.Flavors) {
			return fmt.Errorf("multiple asset entries include %q but specify different flavors", mainAsset.EntryURI)
		}
	}
	paths, err := cache.variantsFor(mainAsset.file())
	if err != nil {
		return err
	}
	prefix := mainAsset.symbolicPrefix()
	variants := make([]asset, 0, len(paths))
	for _, path := range paths {
		rel, err := filepath.Rel(mainAsset.BaseDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		entry := rel
		if prefix != "" {
			entry = prefix + rel
		}
		variant := mainAsset
		variant.RelativeURI = rel
		variant.EntryURI = entry
		variants = append(variants, variant)
	}
	*result = append(*result, assetVariants{Main: mainAsset, Variants: variants})
	return nil
}

func (b *AssetBundle) parseFolder(cache *dirCache, result *[]assetVariants, assetBase string, entry assetEntry, packageName, attributedPackage string) error {
	decoded, err := url.PathUnescape(strings.TrimSuffix(entry.Path, "/"))
	if err != nil {
		return err
	}
	dir := filepath.Join(assetBase, filepath.FromSlash(decoded))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("unable to find directory entry in pubspec.yaml %s: %w", dir, err)
	}
	for _, child := range entries {
		if child.IsDir() {
			continue
		}
		path := filepath.Join(dir, child.Name())
		rel, err := filepath.Rel(assetBase, path)
		if err != nil {
			return err
		}
		if err := b.parseFile(cache, result, assetBase, filepath.ToSlash(rel), packageName, attributedPackage, regularAsset, entry.Path, entry.Flavors, entry.Platforms, entry.Transformers); err != nil {
			return err
		}
	}
	return nil
}

func (b *AssetBundle) parseAssets(spec pubspec, assetBase, packageName, attributedPackage string) ([]assetVariants, error) {
	var result []assetVariants
	cache := newDirCache()
	for _, entry := range spec.Flutter.Assets {
		if strings.HasSuffix(entry.Path, "/") {
			if err := b.parseFolder(cache, &result, assetBase, entry, packageName, attributedPackage); err != nil {
				return nil, err
			}
		} else if err := b.parseFile(cache, &result, assetBase, entry.Path, packageName, attributedPackage, regularAsset, "", entry.Flavors, entry.Platforms, entry.Transformers); err != nil {
			return nil, err
		}
	}
	for _, shader := range spec.Flutter.Shaders {
		for _, asset := range spec.Flutter.Assets {
			if asset.Path == shader.Path || (strings.HasSuffix(asset.Path, "/") && strings.HasPrefix(shader.Path, asset.Path)) {
				return nil, fmt.Errorf("shader %q is also defined as an asset", shader.Path)
			}
		}
		if err := b.parseFile(cache, &result, assetBase, shader.Path, packageName, attributedPackage, shaderAsset, "", shader.Flavors, shader.Platforms, shader.Transformers); err != nil {
			return nil, err
		}
	}
	filtered := result[:0]
	for _, record := range result {
		if record.Main.matches(b.Flavor, b.TargetPlatform) {
			filtered = append(filtered, record)
		}
	}
	result = filtered
	for _, font := range spec.Flutter.Fonts {
		for _, fontAsset := range font.Fonts {
			asset, ok := b.resolveAsset(assetBase, fontAsset.Asset, packageName, attributedPackage, fontAssetKind, "", nil, nil, nil)
			if !ok {
				return nil, fmt.Errorf("unable to resolve font %s", fontAsset.Asset)
			}
			if !fileExists(asset.file()) {
				return nil, fmt.Errorf("unable to locate asset entry in pubspec.yaml %q", fontAsset.Asset)
			}
			if !hasMainAsset(result, asset) {
				result = append(result, assetVariants{Main: asset})
			}
		}
	}
	return result, nil
}

func hasMainAsset(records []assetVariants, asset asset) bool {
	id := asset.identity()
	for _, record := range records {
		if record.Main.identity() == id {
			return true
		}
	}
	return false
}

func packageFonts(fonts []font, packageName string) []font {
	out := make([]font, 0, len(fonts))
	for _, family := range fonts {
		copyFamily := font{Family: "packages/" + packageName + "/" + family.Family}
		for _, file := range family.Fonts {
			copyFile := file
			if !strings.HasPrefix(file.Asset, "packages/") {
				copyFile.Asset = "packages/" + packageName + "/" + file.Asset
			}
			copyFamily.Fonts = append(copyFamily.Fonts, copyFile)
		}
		out = append(out, copyFamily)
	}
	return out
}

func (b *AssetBundle) Build() error {
	manifestPath := filepath.Join(b.ProjectRoot, "pubspec.yaml")
	spec, err := loadPubspec(manifestPath)
	if err != nil {
		return err
	}
	b.packageConfig, err = loadPackageConfig(b.PackageConfigPath)
	if err != nil {
		return err
	}
	if !spec.FlutterNonEmpty {
		b.Entries["AssetManifest.bin"] = bundleEntry{Data: encodeEmptySMCMap(), Generated: true, Kind: regularAsset}
		return nil
	}

	variants, err := b.parseAssets(spec, filepath.Dir(manifestPath), "", "")
	if err != nil {
		return err
	}
	var fonts []font
	if spec.Flutter.UsesMaterial {
		fonts = append(fonts, font{Family: "MaterialIcons", Fonts: []fontAsset{{Asset: "fonts/MaterialIcons-Regular.otf"}}})
	}
	fonts = append(fonts, spec.Flutter.Fonts...)

	graph, err := loadPackageGraph(b.packageConfig.GraphPath)
	if err != nil {
		return err
	}
	var additionalLicenses []string
	for _, dep := range transitiveDependencies(spec.Name, graph, b.packageConfig) {
		if dep.ExclusiveDev || dep.Name == spec.Name {
			continue
		}
		pkg, ok := b.packageConfig.Packages[dep.Name]
		if !ok {
			return fmt.Errorf("could not locate package:%s; try running `flutter pub get`", dep.Name)
		}
		pkgManifestPath := filepath.Join(filepath.Dir(pkg.PackageRoot), "pubspec.yaml")
		if !fileExists(pkgManifestPath) {
			continue
		}
		pkgSpec, err := loadPubspec(pkgManifestPath)
		if err != nil {
			return err
		}
		for _, license := range pkgSpec.Flutter.Licenses {
			decoded, err := url.PathUnescape(license)
			if err != nil {
				return err
			}
			additionalLicenses = append(additionalLicenses, filepath.Join(filepath.Dir(pkg.PackageRoot), filepath.FromSlash(decoded)))
		}
		if pkgSpec.Name == spec.Name {
			continue
		}
		pkgAssets, err := b.parseAssets(pkgSpec, filepath.Dir(pkgManifestPath), dep.Name, dep.Name)
		if err != nil {
			return fmt.Errorf("parse assets of package %s: %w", dep.Name, err)
		}
		variants = mergeAssetVariants(variants, pkgAssets)
		fonts = append(fonts, packageFonts(pkgSpec.Flutter.Fonts, dep.Name)...)
	}

	if spec.Flutter.UsesMaterial {
		material := filepath.Join(b.FlutterRoot, "bin", "cache", "artifacts", "material_fonts", "MaterialIcons-Regular.otf")
		b.Entries["fonts/MaterialIcons-Regular.otf"] = bundleEntry{Source: material, Kind: fontAssetKind}
	}
	frameworkShaders := []struct{ Source, Entry string }{
		{filepath.Join(b.FlutterRoot, "packages", "flutter", "lib", "src", "material", "shaders", "ink_sparkle.frag"), "shaders/ink_sparkle.frag"},
		{filepath.Join(b.FlutterRoot, "packages", "flutter", "lib", "src", "widgets", "shaders", "stretch_effect.frag"), "shaders/stretch_effect.frag"},
	}
	for _, shader := range frameworkShaders {
		if fileExists(shader.Source) {
			if _, exists := b.Entries[shader.Entry]; !exists {
				b.Entries[shader.Entry] = bundleEntry{Source: shader.Source, Kind: shaderAsset}
			}
		}
	}

	for i := range variants {
		record := &variants[i]
		if !fileExists(record.Main.file()) && len(record.Variants) == 0 {
			return fmt.Errorf("no file or variants found for %s", record.Main.EntryURI)
		}
		if fileExists(record.Main.file()) && !containsAsset(record.Variants, record.Main) {
			record.Variants = append([]asset{record.Main}, record.Variants...)
		}
		for _, variant := range record.Variants {
			b.InputFiles = append(b.InputFiles, variant.file())
			b.Entries[variant.EntryURI] = bundleEntry{Source: variant.file(), Kind: variant.Kind, Transformers: variant.Transformers}
		}
	}

	manifest, keys := createAssetManifest(variants)
	b.Entries["AssetManifest.bin"] = bundleEntry{Data: encodeAssetManifest(manifest, keys), Generated: true, Kind: regularAsset}
	fontJSON, err := json.Marshal(fonts)
	if err != nil {
		return err
	}
	b.Entries["FontManifest.json"] = bundleEntry{Data: fontJSON, Generated: true, Kind: regularAsset}
	notices, err := b.collectLicenses(additionalLicenses)
	if err != nil {
		return err
	}
	b.Entries["NOTICES.Z"] = bundleEntry{Data: notices, Generated: true, Kind: regularAsset}
	b.Entries["NativeAssetsManifest.json"] = bundleEntry{Data: []byte(`{"format-version":[1,0,0],"native-assets":{}}`), Generated: true, Kind: regularAsset}
	return nil
}

func mergeAssetVariants(left, right []assetVariants) []assetVariants {
	index := make(map[string]int, len(left))
	for i, record := range left {
		index[record.Main.identity()] = i
	}
	for _, record := range right {
		if i, ok := index[record.Main.identity()]; ok {
			left[i] = record
		} else {
			index[record.Main.identity()] = len(left)
			left = append(left, record)
		}
	}
	return left
}

func containsAsset(assets []asset, target asset) bool {
	id := target.identity()
	for _, asset := range assets {
		if asset.identity() == id {
			return true
		}
	}
	return false
}

func createAssetManifest(records []assetVariants) (map[string][]manifestVariant, []string) {
	type item struct {
		Entry    string
		Variants []asset
	}
	items := make([]item, 0, len(records))
	for _, record := range records {
		items = append(items, item{Entry: record.Main.EntryURI, Variants: record.Variants})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Entry < items[j].Entry })
	manifest := make(map[string][]manifestVariant, len(items))
	keys := make([]string, 0, len(items))
	for _, item := range items {
		key, _ := url.PathUnescape(item.Entry)
		keys = append(keys, key)
		for _, variant := range item.Variants {
			entry, _ := url.PathUnescape(variant.EntryURI)
			mv := manifestVariant{asset: entry}
			parent := filepath.Base(filepath.Dir(filepath.FromSlash(entry)))
			if match := variantDirRE.FindStringSubmatch(parent); match != nil {
				if dpr, err := strconv.ParseFloat(match[1], 64); err == nil {
					mv.DPR = dpr
					mv.HasDPR = true
				}
			}
			manifest[key] = append(manifest[key], mv)
		}
	}
	return manifest, keys
}

func (b *AssetBundle) collectLicenses(additional []string) ([]byte, error) {
	packageLicenses := make(map[string]map[string]bool)
	for name, pkg := range b.packageConfig.Packages {
		root := filepath.Dir(pkg.PackageRoot)
		file := filepath.Join(root, "NOTICES")
		if !fileExists(file) {
			file = filepath.Join(root, "LICENSE")
		}
		if !fileExists(file) {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		parts := strings.Split(string(data), licenseSeparator)
		for _, raw := range parts {
			names := []string{name}
			text := raw
			if len(parts) > 1 {
				if idx := strings.Index(raw, "\n\n"); idx >= 0 {
					names = strings.Split(raw[:idx], "\n")
					text = raw[idx+2:]
				}
			}
			if packageLicenses[text] == nil {
				packageLicenses[text] = make(map[string]bool)
			}
			for _, packageName := range names {
				packageLicenses[text][packageName] = true
			}
		}
	}
	combined := make([]string, 0, len(packageLicenses)+len(additional))
	for text, packageNames := range packageLicenses {
		names := make([]string, 0, len(packageNames))
		for name := range packageNames {
			names = append(names, name)
		}
		sort.Strings(names)
		combined = append(combined, strings.Join(names, "\n")+"\n\n"+text)
	}
	sort.Strings(combined)
	for _, file := range additional {
		if !fileExists(file) {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		combined = append(combined, string(data))
	}
	blob := []byte(strings.Join(combined, licenseSeparator))
	var deflated bytes.Buffer
	writer, err := flate.NewWriter(&deflated, 9)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(blob); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	out := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x13}
	out = append(out, deflated.Bytes()...)
	var footer [8]byte
	binary.LittleEndian.PutUint32(footer[0:4], crc32.ChecksumIEEE(blob))
	binary.LittleEndian.PutUint32(footer[4:8], uint32(len(blob)))
	return append(out, footer[:]...), nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string{}, a...)
	bc := append([]string{}, b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
