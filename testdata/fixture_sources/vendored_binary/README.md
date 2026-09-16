# Synthetic vendored-binary fixture sources

These tiny C functions are the complete source for the `DynamicThing` framework/XCFramework binary and `libStaticThing.a` archives checked into the E2E fixtures.

They exist only to exercise CocoaPods vendored-framework and vendored-library handling. They are original Ripley test material and are licensed under Apache-2.0 with the rest of Ripley's original source code.

Maintainers with Xcode installed can regenerate every checked-in copy with:

```bash
testdata/fixture_sources/vendored_binary/build.sh
```

The script targets iOS arm64 with a minimum deployment target of iOS 15.0. Binary bytes can vary across Apple linker/SDK versions; the fixture contract is the exported functions and their values (`DynamicThingValue() == 41`, `StaticThingValue() == 1`), not a fixed hash.
