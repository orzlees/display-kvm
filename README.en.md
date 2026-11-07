[简体中文](README.md) | English

# displayKvm

Switch monitor inputs from a local background process, command-line client, and desktop application. Each computer controls its own monitor connections; no network remote-control service is required.

## Components

- `displaykvmd`: the background process that loads settings and handles local requests.
- `displaykvm-cli`: the packaged command-line client. In source commands, its entry point is `cmd/displaykvm`.
- `displaykvm`: the packaged desktop application.

The desktop application connects to the background process. Start the background process separately; closing the desktop application does not stop it.

## Download a candidate

- [Current prerelease: v0.0.1-rc.20261107.12](https://github.com/orzlees/display-kvm/releases/tag/v0.0.1-rc.20261107.12)
- [Successful candidate build and release workflow](https://github.com/orzlees/display-kvm/actions/runs/42381983666)

Choose an archive for your operating system and CPU architecture, and check its matching SHA-256 file. The release contains nine archives and nine checksum files. These are prerelease candidates without a trusted distribution signature; the macOS app uses an ad-hoc signature and is not notarized. CI and archive checks have passed. The packaged applications and real monitor, USB, login, and sleep/wake behavior still require testing on real systems.

## Quick start

Create a complete settings file, configure real display identities and targets, and validate it before starting the background process. An empty generated document has no device mappings and cannot switch a monitor. Sample paths, display IDs, UUIDs, USB IDs, and input values must be replaced with your own values.

From a source checkout:

```sh
go run ./cmd/displaykvm config new --file '/absolute/settings.json'
go run ./cmd/displaykvm config check --file '/absolute/settings.json'
go run ./cmd/displaykvmd -settings '/absolute/settings.json'
```

In a second terminal:

```sh
go run ./cmd/displaykvm list
go run ./cmd/displaykvm status
go run ./cmd/displaykvm --json status
```

For packaged builds, use `displaykvm-cli` for CLI commands and `displaykvmd` for the background process. The [Windows candidate guide](docs/usage/windows-candidate.md) (Chinese) explains its three executable files.

You can also create a settings document while the background process is offline: open Settings, choose New configuration, use Save as to select a file, and save the startup profile. Creating or saving a profile does not start the background process or enable login startup. See the [full user guide](docs/usage/README.md) (Chinese).

For a macOS source build:

```sh
sh desktop/build-macos.sh
desktop/bin/DisplayKvm.app/Contents/MacOS/displaykvmd -settings '/absolute/settings.json'
```

Then open the desktop application in another terminal:

```sh
desktop/bin/DisplayKvm.app/Contents/MacOS/displaykvm
```

Keep the application bundle and its bundled tools together. See the [configuration contract](docs/contracts.md) (Chinese) for the complete settings schema.

## Five-second input preview

Save a target before testing it. The background process first reads the monitor's current input, submits the test switch, waits for the fixed five-second preview period, and attempts to restore the original input. Success requires a matching readback; a submitted command alone is not confirmation that the monitor changed inputs.

If the original input cannot be read reliably, the preview does not switch that display. It skips a display already using the requested input. During restoration, a third input value is treated as a conflict and is left unchanged. A failed, unknown, or conflicting restoration stops previews for subsequent displays. Restoration succeeds only after the original input is read back.

A disconnected control path, a killed background process, or a power failure can prevent restoration. Be ready to select an input with the monitor's own menu. The Windows AMD/LG special-protocol path has no reliable readback and does not offer this preview. Read the [preview behavior and limits](docs/usage/input-preview.md) (Chinese) before using it.

## Platform status

| Platform | Current scope |
| --- | --- |
| macOS Apple Silicon | arm64 app candidate with bundled `m1ddc` v1.3.1. Real-system and hardware testing remains outstanding. |
| macOS Intel | The display-control backend is unavailable. |
| Windows amd64 / arm64 | Native standard VCP input control, desktop candidates, and current-user login startup are implemented. The AMD/LG special-protocol path is amd64 only. Real-system and hardware testing remains outstanding. |
| Linux amd64 / arm64 | rc12 bundles `ddcutil`, `busctl`, and `udevadm`; native payload generation and strict dependency, source, and license release gates passed. The build baseline is Debian 12 / glibc 2.36. Real-system testing and distribution compatibility remain unverified. |

Login startup runs after the current user signs in; it is not a pre-login system service. Monitor DDC/CI support, cabling, drivers, permissions, and control availability while another video input is active determine whether a switch can work. See [compatibility and evidence boundaries](docs/compatibility.md) (Chinese).

## Build and package

```sh
sh build/build-core.sh --target all --version 0.0.1-dev --run-id local-core
sh packaging/package.sh core --run-id local-core
sh desktop/build-macos.sh
sh packaging/package.sh desktop --app desktop/bin/DisplayKvm.app --run-id local-desktop
```

Linux builds require matching, verified Linux tool payloads; the commands above do not generate those payloads on macOS or Windows. Follow the [build guide](build/README.md) (Chinese) and [packaging guide](packaging/README.md) (Chinese) for prerequisites and additional arguments.

## Documentation and licenses

- [Documentation index](docs/README.md) (Chinese)
- [User guide](docs/usage/README.md) (Chinese)
- [Compatibility](docs/compatibility.md) (Chinese)
- [Packaging](packaging/README.md) (Chinese)
- [Contributing](CONTRIBUTING.md) (Chinese)
- [Project license](LICENSE)

Keep the third-party notices, licenses, source materials, manifests, and checksums that accompany a distribution. Bundled third-party tools retain their own licenses.
