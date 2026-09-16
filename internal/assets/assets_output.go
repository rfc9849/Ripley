package assets

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"ripley/internal/toolchain"
)

type constFinderLocation struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

type constFinderInstance struct {
	FontPackage *string  `json:"fontPackage"`
	FontFamily  *string  `json:"fontFamily"`
	CodePoint   *float64 `json:"codePoint"`
}

type constFinderResult struct {
	NonConstLocations []constFinderLocation `json:"nonConstLocations"`
	ConstantInstances []constFinderInstance `json:"constantInstances"`
}

type iconTreeShaker struct {
	Toolchain    toolchain.Toolchain
	fontManifest []byte
	Enabled      bool
	loaded       bool
	iconData     map[string][]int
}

func newIconTreeShaker(tc toolchain.Toolchain, fontManifest []byte, enabled bool) *iconTreeShaker {
	return &iconTreeShaker{Toolchain: tc, fontManifest: fontManifest, Enabled: enabled && fontManifest != nil}
}

func (s *iconTreeShaker) loadIconData(appDill string) error {
	stdout, stderr, err := runCapture("", nil, "", s.Toolchain.Dart(),
		s.Toolchain.ConstFinder(),
		"--kernel-file", appDill,
		"--class-library-uri", "package:flutter/src/widgets/icon_data.dart",
		"--class-name", "IconData",
		"--annotation-class-name", "_StaticIconProvider",
		"--annotation-class-library-uri", "package:flutter/src/widgets/icon_data.dart",
	)
	if err != nil {
		return fmt.Errorf("ConstFinder failure: %s", stderr)
	}
	var result constFinderResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		return fmt.Errorf("decode ConstFinder output: %w", err)
	}
	if len(result.NonConstLocations) > 0 {
		for _, loc := range result.NonConstLocations {
			fmt.Fprintf(os.Stderr, "  - %s:%d:%d\n", loc.File, loc.Line, loc.Column)
		}
		return errors.New("this application cannot tree shake icon fonts (non-constant IconData); rebuild with --no-tree-shake-icons")
	}
	icons := make(map[string][]int)
	for _, instance := range result.ConstantInstances {
		if instance.FontFamily == nil || instance.CodePoint == nil {
			continue
		}
		key := *instance.FontFamily
		if instance.FontPackage != nil && *instance.FontPackage != "" {
			key = "packages/" + *instance.FontPackage + "/" + key
		}
		icons[key] = append(icons[key], int(*instance.CodePoint+0.5))
	}
	var fonts []font
	if err := json.Unmarshal(s.fontManifest, &fonts); err != nil {
		return fmt.Errorf("decode FontManifest.json: %w", err)
	}
	fontFiles := make(map[string]string)
	for _, font := range fonts {
		if _, needed := icons[font.Family]; !needed {
			continue
		}
		if len(font.Fonts) != 1 {
			return errors.New("cannot tree-shake icon fonts with multiple fonts per family")
		}
		fontFiles[font.Family] = font.Fonts[0].Asset
	}
	s.iconData = make(map[string][]int)
	for family, codepoints := range icons {
		if file, ok := fontFiles[family]; ok {
			s.iconData[file] = codepoints
		}
	}
	s.loaded = true
	return nil
}

func (s *iconTreeShaker) subsetFont(inputPath, outputPath, relativePath, appDill string) (bool, error) {
	if !s.Enabled {
		return false, nil
	}
	st, err := os.Stat(inputPath)
	if err != nil {
		return false, err
	}
	if st.Size() < 12 {
		return false, nil
	}
	ext := strings.ToLower(filepath.Ext(inputPath))
	if ext != ".ttf" && ext != ".otf" {
		return false, nil
	}
	if !s.loaded {
		if err := s.loadIconData(appDill); err != nil {
			return false, err
		}
	}
	codepoints, ok := s.iconData[relativePath]
	if !ok {
		return false, nil
	}
	parts := make([]string, len(codepoints))
	for i, cp := range codepoints {
		parts[i] = strconv.Itoa(cp)
	}
	_, stderr, err := runCapture("", nil, strings.Join(parts, " "), s.Toolchain.FontSubset(), outputPath, inputPath)
	if err != nil {
		return false, fmt.Errorf("font subsetting failed: %s", stderr)
	}
	outInfo, err := os.Stat(outputPath)
	if err != nil {
		return false, err
	}
	reduction := float64(st.Size()-outInfo.Size()) / float64(st.Size()) * 100
	fmt.Printf("font asset %q was tree-shaken, reducing it from %d to %d bytes (%.1f%% reduction).\n", filepath.Base(inputPath), st.Size(), outInfo.Size(), reduction)
	return true, nil
}

func compileShader(tc toolchain.Toolchain, inputPath, outputPath string, transformers []transformer, projectDir, buildMode string) error {
	source := inputPath
	transformed := outputPath + ".transformed"
	if len(transformers) > 0 {
		if err := applyTransformers(tc, inputPath, transformed, transformers, projectDir, buildMode); err != nil {
			return err
		}
		source = transformed
		defer os.Remove(transformed)
	}
	shaderLib := filepath.Join(filepath.Dir(tc.ImpellerC()), "shader_lib")
	args := []string{
		"--runtime-stage-metal", "--iplr",
		"--sl=" + outputPath,
		"--spirv=" + outputPath + ".spirv",
		"--input=" + source,
		"--input-type=frag",
		"--include=" + filepath.Dir(source),
		"--include=" + shaderLib,
	}
	_, stderr, err := runCapture("", nil, "", tc.ImpellerC(), args...)
	if err != nil {
		return fmt.Errorf("impellerc failed on %s:\n%s", inputPath, stderr)
	}
	_ = os.Remove(outputPath + ".spirv")
	return nil
}

func applyTransformers(tc toolchain.Toolchain, asset, output string, transformers []transformer, workingDir, buildMode string) error {
	tmp, err := os.MkdirTemp("", "ripley-asset-transform-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	current := filepath.Join(tmp, filepath.Base(asset)+".in0")
	if err := copyFile(asset, current); err != nil {
		return err
	}
	env := append(os.Environ(), "FLUTTER_BUILD_MODE="+buildMode)
	for i, transformer := range transformers {
		next := filepath.Join(tmp, fmt.Sprintf("%s.out%d", filepath.Base(asset), i))
		args := []string{"run", transformer.Package, "--input=" + current, "--output=" + next}
		args = append(args, transformer.Args...)
		stdout, stderr, err := runCapture(workingDir, env, "", tc.Dart(), args...)
		if err != nil {
			return fmt.Errorf("asset transformer %q failed:\n%s\n%s", transformer.Package, stdout, stderr)
		}
		if !fileExists(next) {
			return fmt.Errorf("asset transformer %q produced no output:\nstdout:\n%s\nstderr:\n%s", transformer.Package, stdout, stderr)
		}
		current = next
	}
	return copyFile(current, output)
}

func CopyAssets(bundle *AssetBundle, outDir string, tc toolchain.Toolchain, appDill string, treeShakeIcons bool, buildMode string) error {
	if err := os.RemoveAll(outDir); err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	fontManifest := bundle.Entries["FontManifest.json"]
	var fontManifestData []byte
	if fontManifest.Generated {
		fontManifestData = fontManifest.Data
	}
	shaker := newIconTreeShaker(tc, fontManifestData, treeShakeIcons)

	keys := make([]string, 0, len(bundle.Entries))
	for key := range bundle.Entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := bundle.Entries[key]
		decoded, err := urlPathUnescape(key)
		if err != nil {
			return err
		}
		dest := filepath.Join(outDir, filepath.FromSlash(decoded))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if entry.Generated {
			if err := os.WriteFile(dest, entry.Data, 0o644); err != nil {
				return err
			}
			continue
		}
		switch entry.Kind {
		case fontAssetKind:
			subset, err := shaker.subsetFont(entry.Source, dest, key, appDill)
			if err != nil {
				return err
			}
			if subset {
				continue
			}
		case shaderAsset:
			if err := compileShader(tc, entry.Source, dest, entry.Transformers, bundle.ProjectRoot, buildMode); err != nil {
				return err
			}
			continue
		default:
			if len(entry.Transformers) > 0 {
				if err := applyTransformers(tc, entry.Source, dest, entry.Transformers, bundle.ProjectRoot, buildMode); err != nil {
					return err
				}
				continue
			}
		}
		if err := copyFile(entry.Source, dest); err != nil {
			return err
		}
	}
	return nil
}

func urlPathUnescape(value string) (string, error) {
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return "", fmt.Errorf("decode asset path %q: %w", value, err)
	}
	return decoded, nil
}
