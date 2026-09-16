#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")/../../.." && pwd)
src="$root/testdata/fixture_sources/vendored_binary"
sdk=$(xcrun --sdk iphoneos --show-sdk-path)
clang=$(xcrun --sdk iphoneos --find clang)
ar=$(xcrun --sdk iphoneos --find ar)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

"$clang" -target arm64-apple-ios15.0 -isysroot "$sdk" \
  -dynamiclib \
  -install_name @rpath/DynamicThing.framework/DynamicThing \
  "$src/DynamicThing.c" -o "$work/DynamicThing"

"$clang" -target arm64-apple-ios15.0 -isysroot "$sdk" \
  -c "$src/StaticThing.c" -o "$work/static.o"
"$ar" rcs "$work/libStaticThing.a" "$work/static.o"

framework_bins=(
  "$root/testdata/e2e/framework/packages/vendored_binary_plugin/ios/Vendor/DynamicThing.framework/DynamicThing"
  "$root/testdata/e2e/vendored/packages/vendored_binary_plugin/ios/Vendor/DynamicThing.xcframework/ios-arm64/DynamicThing.framework/DynamicThing"
  "$root/testdata/e2e/script_phase/packages/vendored_binary_plugin/ios/Vendor/DynamicThing.xcframework/ios-arm64/DynamicThing.framework/DynamicThing"
)
static_libs=(
  "$root/testdata/e2e/framework/packages/vendored_binary_plugin/ios/Vendor/libStaticThing.a"
  "$root/testdata/e2e/vendored/packages/vendored_binary_plugin/ios/Vendor/libStaticThing.a"
  "$root/testdata/e2e/script_phase/packages/vendored_binary_plugin/ios/Vendor/libStaticThing.a"
)

for dst in "${framework_bins[@]}"; do
  install -m755 "$work/DynamicThing" "$dst"
done
for dst in "${static_libs[@]}"; do
  install -m644 "$work/libStaticThing.a" "$dst"
done

printf 'Regenerated synthetic vendored fixtures with iPhoneOS SDK %s\n' \
  "$(basename "$sdk")"
