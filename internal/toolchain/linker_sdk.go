package toolchain

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	linkerSDKCompatVersion = "tapi-arm64e-x1-v1"
	unsupportedTAPITarget  = "arm64e.x1-ios"
)

var (
	linkerSDKMu       sync.Mutex
	linkerSDKSafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	linkerSDKResolved = map[string]string{}
)

// LinkerIOSSDK returns an SDK view suitable for the bundled ld64.lld.
//
// Xcode 27 introduced the TAPI target spelling "arm64e.x1-ios". LLVM 20's
// ld64.lld rejects the whole .tbd document when that target is present, even
// though the same document also contains the compatible arm64e-ios target.
// Clang and Swift must continue to see the original SDK unchanged, so ripley builds
// a sparse linker-only view containing the SDK's linkable stubs/archives. The
// only textual transformation is removal of arm64e.x1-ios from multi-target
// TAPI lists. The source SDK is never modified.
func (t Toolchain) LinkerIOSSDK() (string, error) {
	sdk, err := t.IOSSDK()
	if err != nil {
		return "", err
	}
	cacheKey := filepath.Clean(sdk) + "\n" + filepath.Clean(t.Root)
	linkerSDKMu.Lock()
	defer linkerSDKMu.Unlock()
	if resolved, ok := linkerSDKResolved[cacheKey]; ok {
		return resolved, nil
	}

	needsCompat, err := sdkNeedsTAPICompat(sdk)
	if err != nil {
		return "", err
	}
	if !needsCompat {
		linkerSDKResolved[cacheKey] = sdk
		return sdk, nil
	}

	version, err := t.IOSSDKVersion()
	if err != nil {
		return "", err
	}
	name := linkerSDKSafeName.ReplaceAllString(version, "_")
	if name == "" {
		name = "unknown"
	}
	dst := filepath.Join(t.Root, "linker-sdk", "iPhoneOS"+name+"-"+linkerSDKCompatVersion)
	marker := filepath.Join(dst, ".ripley-linker-sdk")
	fingerprint := filepath.Clean(sdk) + "\n" + version + "\n" + linkerSDKCompatVersion + "\n"
	if data, err := os.ReadFile(marker); err == nil && string(data) == fingerprint {
		linkerSDKResolved[cacheKey] = dst
		return dst, nil
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dst), ".linker-sdk-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := buildLinkerSDKView(sdk, tmp); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, ".ripley-linker-sdk"), []byte(fingerprint), 0o644); err != nil {
		return "", err
	}
	if err := os.RemoveAll(dst); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	linkerSDKResolved[cacheKey] = dst
	return dst, nil
}

func sdkNeedsTAPICompat(sdk string) (bool, error) {
	for _, root := range []string{filepath.Join(sdk, "System"), filepath.Join(sdk, "usr", "lib")} {
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return false, err
		}
		found := false
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if found || d.IsDir() || d.Type()&os.ModeSymlink != 0 || filepath.Ext(path) != ".tbd" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			found = strings.Contains(string(data), unsupportedTAPITarget)
			return nil
		})
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

func buildLinkerSDKView(sdk, dst string) error {
	for _, relRoot := range []string{"System", filepath.Join("usr", "lib")} {
		root := filepath.Join(sdk, relRoot)
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == root || d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(sdk, path)
			if err != nil {
				return err
			}
			target := filepath.Join(dst, rel)
			if d.Type()&os.ModeSymlink != 0 {
				link, err := os.Readlink(path)
				if err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					return err
				}
				if err := os.Symlink(link, target); err != nil && !os.IsExist(err) {
					return err
				}
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".tbd" && ext != ".a" && ext != ".dylib" && ext != ".o" {
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if ext == ".tbd" {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				clean, changed, err := sanitizeTAPIForLLD(data)
				if err != nil {
					return fmt.Errorf("sanitize %s: %w", path, err)
				}
				if changed {
					return os.WriteFile(target, clean, 0o644)
				}
			}
			return os.Symlink(path, target)
		}); err != nil {
			return err
		}
	}
	return nil
}

func sanitizeTAPIForLLD(data []byte) ([]byte, bool, error) {
	text := string(data)
	if !strings.Contains(text, unsupportedTAPITarget) {
		return data, false, nil
	}
	clean := strings.ReplaceAll(text, ", "+unsupportedTAPITarget, "")
	clean = strings.ReplaceAll(clean, unsupportedTAPITarget+", ", "")
	if strings.Contains(clean, unsupportedTAPITarget) {
		return nil, false, fmt.Errorf("unsupported TAPI target %s appears without a compatible sibling target", unsupportedTAPITarget)
	}
	return []byte(clean), true, nil
}
