# Kamui Cloud — Release Notes

## 1. Purpose

This document records significant changes to Kamui Cloud, including new functionality, improvements, bug fixes, known limitations, and release information.

Only verified changes should be recorded as completed.

## 2. Release Notes Format

Use this template for each release.

# Kamui Cloud vX.Y.Z

**Release Date:** YYYY-MM-DD  
**Status:** Planned / In Development / Released

### Added
- List newly implemented and verified features.

### Changed
- Describe changes to existing functionality or behavior.

### Fixed
- List verified bug fixes.

### Known Limitations
- Describe unresolved issues and unsupported functionality.

### Compatibility and Requirements
- Document relevant operating-system, runtime, network, or configuration requirements.

### Contributors
- Mention contributors and link relevant pull requests when available.

---

## 3. Initial Development Status

### Version 0.1 — Development Baseline

**Status:** Under development

This section establishes a documentation baseline. It does not claim that a release has shipped.

#### Project Direction

Kamui Cloud is intended to provide an intranet-first platform for managing computing resources and cloud-style services across authorized machines.

#### Planned Service Areas

- Node registration and monitoring
- API and coordination
- Storage
- Workload execution
- Command-line management
- Machine-learning training
- Model inference
- Web console
- Private-network file sharing

These are project areas, not a declaration that each service is implemented.

#### Known Limitations

The actual state of each service must be determined by inspecting its implementation, configuration, and tests.

Do not assume that distributed scheduling, replication, fault tolerance, GPU execution, or production security guarantees are available without verification.

---

## 4. Upcoming Changes Template

For a future release, use the following structure:

### Added
- [Feature name] — implementation and verification reference.

### Changed
- [Change description] — affected component and behavior.

### Fixed
- [Bug description] — verified correction.

### Known Limitations
- [Outstanding issue] — current impact and any workaround.

### Validation
- Tests executed:
- Test results:
- Documentation updated:
- Related pull requests:

Replace placeholders only when the corresponding information is available.

## 5. Release Documentation Workflow

1. Review the changes included in the release.
2. Confirm the relevant pull requests and commits.
3. Verify the implementation and tests.
4. Update the release-note section.
5. Record known limitations honestly.
6. Check the version and date with the project head.
7. Submit the documentation change through the repository's approved workflow.

## 6. Release Accuracy Rules

- Never invent release dates or version numbers.
- Never mark a planned feature as released.
- Never report a test as passed unless it was executed successfully.
- Record breaking changes and compatibility requirements.
- Keep release notes concise, factual, and traceable to actual changes.