package ios

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ripley/internal/toolchain"
)

// runLD64 invokes ld64.lld with a linker-compatible view of the iPhoneOS SDK.
// Compile steps keep using the original SDK. This matters for Xcode 27 SDKs,
// whose TAPI stubs contain arm64e.x1-ios, a target spelling LLVM 20's ld64.lld
// does not understand yet.
func runLD64(tc toolchain.Toolchain, args ...string) error {
	return runLD64Path(tc, filepath.Join(tc.ToolsetBin(), "ld64.lld"), args...)
}

func runLD64Path(tc toolchain.Toolchain, lld string, args ...string) error {
	sdk, err := tc.IOSSDK()
	if err != nil {
		return err
	}
	linkerSDK, err := tc.LinkerIOSSDK()
	if err != nil {
		return err
	}
	rewritten := rewriteSDKArgs(args, sdk, linkerSDK)
	return run("", nil, lld, rewritten...)
}

func rewriteSDKArgs(args []string, sdk, linkerSDK string) []string {
	if sdk == "" || linkerSDK == "" || filepath.Clean(sdk) == filepath.Clean(linkerSDK) {
		return append([]string(nil), args...)
	}
	out := make([]string, len(args))
	for i, arg := range args {
		out[i] = strings.ReplaceAll(arg, sdk, linkerSDK)
	}
	return out
}

// writeLD64SDKShim writes a tiny linker proxy for toolchains that invoke LLD
// indirectly through clang/rustc/build hooks. Compile steps keep their original
// SDK; only linker arguments that point inside that SDK are redirected.
func writeLD64SDKShim(path, realLLD, sdk, linkerSDK string) error {
	script := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
real=%q
sdk=%q
linker_sdk=%q
args=()
for arg in "$@"; do
  if [[ "$sdk" != "$linker_sdk" ]]; then
    arg="${arg//$sdk/$linker_sdk}"
  fi
  args+=("$arg")
done
exec "$real" "${args[@]}"
`, realLLD, sdk, linkerSDK)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(script), 0o755)
}
