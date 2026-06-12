# Contributing to airlock

Thanks for your interest in contributing!

## Prerequisites

- macOS 13 or later
- Xcode 15+ / Swift 5.9+

## Build

```bash
swift build
```

## Test

```bash
swift test
```

## Local integration smoke

CI runs an end-to-end smoke against a release build of the daemon. To run it locally, build with `swift build -c release`, make sure nothing else is bound to port 27183 (stop any running airlock instance first), then run the commands from the **Integration smoke** step in [`.github/workflows/ci.yml`](.github/workflows/ci.yml) against `.build/release/airlock`. Remember to kill the backgrounded daemon afterward (`kill %1` or `pkill -f '/airlock$'`), and note that the smoke writes to your real UserDefaults unless you launch the daemon with `AIRLOCK_DEFAULTS_SUITE` set to a throwaway suite name.

## Branch convention

- Work on feature branches (e.g. `feat/...`, `fix/...`, `chore/...`).
- Direct commits to `main` are blocked by a local git hook — open a branch and merge.

## Pull request expectations

- `swift build` and `swift test` pass.
- New behavior is covered by tests.
- Add an entry to `CHANGELOG.md` describing the change.
