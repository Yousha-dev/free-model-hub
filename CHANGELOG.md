# Changelog

## 1.0.1

- Attribute credential files to an explicit vendor first, then their leading vendor prefix, before checking legacy embedded markers. Account labels containing another provider's name no longer select that provider by accident.
- Add regression coverage using the real Cline parser and retain regional vendor precedence.
- Require vet and race-enabled tests before release builds, and inject the release version into each platform's plugin metadata.
