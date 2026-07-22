# Third-Party Notices

## scrcpy-server 3.3.4

The plugin bundles the Android server artifact from Genymobile scrcpy 3.3.4.

- Project: https://github.com/Genymobile/scrcpy
- License: Apache License 2.0
- Bundled license: `licenses/scrcpy-APACHE-2.0.txt`
- SHA-256: `8588238c9a5a00aa542906b6ec7e6d5541d9ffb9b5d0f6e1bc0e365e2303079e`

The plugin communicates with the server through scrcpy's documented socket and
control protocols. It does not bundle the scrcpy desktop client.

## Android SDK Platform-Tools adb 37.0.0

The macOS plugin bundles the universal `adb` binary from Google Android SDK
Platform-Tools 37.0.0 (`37.0.0-14910828`).

- Project: https://developer.android.com/tools/releases/platform-tools
- Distribution: `platform-tools-latest-darwin.zip`
- Upstream notices: `licenses/android-platform-tools-NOTICE.txt`
- Upstream package metadata: `licenses/android-platform-tools-source.properties`
- Archive SHA-256: `094a1395683c509fd4d48667da0d8b5ef4d42b2abfcd29f2e8149e2f989357c7`
- Bundled `adb` SHA-256: `9fdf861259dc807937b13afdd5f053c7fda9f3b7726933fe0e0f45130ecb8dc7`

## Go runtime dependencies

The standalone `phone-agent` binaries include the following Go modules. The
listed license files are copied verbatim from the exact module versions in
`go.sum`.

### github.com/modelcontextprotocol/go-sdk v1.6.1

- Project: https://github.com/modelcontextprotocol/go-sdk
- License: Apache-2.0 and residual MIT during the project's licensing
  transition; the upstream file also contains CC-BY-4.0 terms for
  documentation
- Bundled license: `licenses/go-modelcontextprotocol-go-sdk-LICENSE.txt`

### github.com/google/jsonschema-go v0.4.3

- Project: https://github.com/google/jsonschema-go
- License: MIT
- Bundled license: `licenses/go-google-jsonschema-go-LICENSE.txt`

### github.com/segmentio/asm v1.1.3

- Project: https://github.com/segmentio/asm
- License: MIT
- Bundled license: `licenses/go-segmentio-asm-LICENSE.txt`

### github.com/segmentio/encoding v0.5.4

- Project: https://github.com/segmentio/encoding
- License: MIT
- Bundled license: `licenses/go-segmentio-encoding-LICENSE.txt`

### github.com/yosida95/uritemplate/v3 v3.0.2

- Project: https://github.com/yosida95/uritemplate
- License: BSD-3-Clause
- Bundled license: `licenses/go-yosida95-uritemplate-LICENSE.txt`

### golang.org/x/oauth2 v0.35.0

- Project: https://go.googlesource.com/oauth2
- License: BSD-3-Clause
- Bundled license: `licenses/go-x-oauth2-LICENSE.txt`

### golang.org/x/sys v0.41.0

- Project: https://go.googlesource.com/sys
- License: BSD-3-Clause
- Bundled license: `licenses/go-x-sys-LICENSE.txt`
