# Ripley

Named after [_The Talented Mr. Ripley_](https://en.wikipedia.org/wiki/The_Talented_Mr._Ripley) by Patricia Highsmith.

Build and package Flutter iOS apps on Linux without macOS or Xcode.

Ripley compiles Dart AOT, C/C++/Objective-C/Swift code, CocoaPods, and SwiftPM packages, links Mach-O binaries, packages resources, signs IPAs, and installs them directly onto physical iPhones over USB.

## Features

- **No macOS required:** build and package iOS apps entirely on Linux (`x86_64` and `arm64`).
- **Full plugin support:** CocoaPods (static & dynamic), Swift Package Manager, Dart Native Assets, and Cargokit/Rust (`flutter_rust_bridge`).
- **App extensions:** compiles and bundles Share, Widget, and Notification extensions.
- **Native asset handling:** pure-Go asset catalog (`Assets.car`) and storyboard compilation.
- **Flexible signing:** `.p12` + mobileprovision profiles, ad-hoc signing, or `--no-sign`.
- **On-device workflow:** install, launch, stream logs, and uninstall over USB.

Tested on real-world Flutter apps including [LocalSend](https://github.com/localsend/localsend), [Immich](https://github.com/immich-app/immich), [Saber](https://github.com/saber-notes/saber), [Smooth App](https://github.com/openfoodfacts/smooth-app), and own apps with complex CocoaPods graphs (Firebase, Google Maps, ML Kit).

## Prerequisites

- **Linux** (`x86_64` or `arm64`)
- **Flutter SDK**
- **Go 1.24+** (only if building Ripley from source)
- **Clang & LLD** (`sudo apt install clang lld`)
- **Swift for Linux** matching your iPhoneOS SDK
- **iPhoneOS SDK** and Swift iOS runtime libraries (from Xcode)
- **CocoaPods** (if your project uses pods)
- **usbmuxd** (for USB device deployment)

> **Where to get the iPhoneOS SDK:**
> Ripley does not distribute Apple proprietary files. You can obtain `iPhoneOS.sdk` and the Swift iOS runtime from Xcode:
> - **Download Xcode (`Xcode.xip`):** Go to [developer.apple.com/download/all/?q=Xcode](https://developer.apple.com/download/all/?q=Xcode) in your browser (requires logging in with your Apple ID and accepting terms; do not use `curl`).
> - **Files needed from Xcode:**
>   - SDK: `Xcode.app/Contents/Developer/Platforms/iPhoneOS.platform/Developer/SDKs/iPhoneOS.sdk`
>   - Swift iOS libs: `Xcode.app/Contents/Developer/Toolchains/XcodeDefault.xctoolchain/usr/lib/swift/iphoneos`
> - **Sync from a Mac:** If you have access to a Mac with Xcode, you can sync the SDK directly:
>   `ripley setup --sdk user@mac:/Applications/Xcode.app/Contents/Developer/Platforms/iPhoneOS.platform/Developer/SDKs/iPhoneOS.sdk`

## Installation

```bash
go build -o ~/.local/bin/ripley ./cmd/ripley
ripley --version
```

Cross-compiling for Linux from another machine:

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o ripley ./cmd/ripley
```

## Quick Start

### 1. Toolchain setup

Point Ripley to your Flutter SDK:

```bash
export FLUTTER_ROOT=/path/to/flutter
```

Fetch the prebuilt cross-compilation `gen_snapshot`:

```bash
ripley toolchain fetch
# Or build from Dart source: ripley toolchain build
```

*(If using a source build of Ripley, set `RIPLEY_GEN_SNAPSHOT_REPO=owner/ripley` before running `fetch`).*

Configure your iPhoneOS SDK and Swift toolchain:

```bash
ripley setup \
  --sdk /path/to/iPhoneOS.sdk \
  --swift /path/to/swift-toolchain
```

Tell Ripley where your Swift toolchain and iOS runtime libraries live:

```bash
export RIPLEY_SWIFT=/path/to/swift-toolchain
# If Swift iOS libraries are not in ~/.ripley/sdks/iphoneos:
export RIPLEY_SWIFT_IOS_LIBS=/path/to/XcodeDefault.xctoolchain/usr/lib/swift/iphoneos
```

### 2. Build your app

In your Flutter project directory:

```bash
flutter pub get

# Build a signed release IPA
ripley build ios --ipa

# Build without signing
ripley build ios --ipa --no-sign
```

Useful build options:

```bash
# Profile mode
ripley build ios --ipa --mode profile

# Custom entrypoint and flavor
ripley build ios --ipa -t lib/main_prod.dart --flavor production

# Override bundle ID
ripley build ios --ipa --bundle-id com.example.myapp

# Obfuscate Dart code and save symbol maps
ripley build ios --ipa --split-debug-info=build/symbols --obfuscate

# Generate dSYM debug bundle
ripley build ios --ipa --save-debugging-info
```

Outputs are saved in `build/ripley_ios/` (`<App>.app`, `<App>.ipa`, and optional `<App>.dSYM`).

### 3. Deploy to a physical device

Connect your iPhone via USB (ensure `usbmuxd` is running):

```bash
# List devices
ripley device list

# Install and launch the app
ripley device run build/ripley_ios/MyApp.ipa

# Stream system logs
ripley device logs

# Launch an already installed app
ripley device launch com.example.myapp

# Uninstall
ripley device uninstall com.example.myapp
```

## Code Signing

Drop your certificate and profile in `~/.ripley-signing/`:

```text
~/.ripley-signing/
  developer.p12
  profile.mobileprovision
  password                  # optional, or use export RIPLEY_SIGN_PASSWORD='...'
```

Or pass them explicitly:

```bash
ripley build ios --ipa \
  --key developer.p12 \
  --prov profile.mobileprovision \
  --password "secret"
```

If no signing material is configured and `--no-sign` is not passed, Ripley applies ad-hoc signing (`zsign -a`).

## Key Environment Variables

| Variable | Description |
| --- | --- |
| `FLUTTER_ROOT` | Path to Flutter SDK (auto-detected if `flutter` is in `PATH`) |
| `RIPLEY_SWIFT` | Path to Linux Swift toolchain root (`usr/bin/swiftc`) |
| `RIPLEY_SWIFT_IOS_LIBS` | Path to Swift iOS runtime libraries (`XcodeDefault.xctoolchain/usr/lib/swift/iphoneos`) |
| `RIPLEY_IOS_SDK` | Path to `iPhoneOS.sdk` (alternative to `ripley setup --sdk`) |
| `RIPLEY_SIGN_PASSWORD` | Password for `.p12` signing key |
| `RIPLEY_CARGO` | Path to `cargo` binary for Rust / Cargokit projects |
| `RIPLEY_HOME` | Cache and toolchain storage directory (default: `~/.ripley`) |

## Repository Structure

```text
cmd/ripley/                 CLI entry point
internal/app/               Build orchestration, CLI commands, device management
internal/assets/            Flutter assets and shader compilation
internal/toolchain/         Toolchain provisioning and cross gen_snapshot
internal/ios/               Clang/Swift compilation, linking, CocoaPods, SwiftPM, signing
internal/ios/assetcatalog/  Pure-Go asset catalog compiler (Assets.car)
internal/ios/storyboard/    Pure-Go storyboard compiler
internal/ios/idevice/       USB communication (usbmux, lockdown, AFC, DDI mounter)
```

## License

Ripley is licensed under the Apache License 2.0. See [LICENSE](LICENSE) and [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES).
