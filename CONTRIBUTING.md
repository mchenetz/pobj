# Contributing to entity

Thanks for contributing to `entity`.

## Scope

Contributions are welcome for:
- bug fixes
- tests and reliability improvements
- documentation
- performance and scalability improvements
- COSI and Kubernetes interoperability

## Before You Start

1. Open an issue for significant behavior changes.
2. Confirm design direction with maintainers before large refactors.
3. Keep PRs small and focused.

## Development Setup

```bash
go build ./...
go test ./...
```

For full integration:

```bash
DOCKER_CONTEXT=desktop-linux make e2e-kind
```

## Coding Guidelines

- Keep changes minimal and explicit.
- Add tests for new behavior and bug fixes.
- Prefer backward-compatible changes where possible.
- Update docs when behavior, flags, or CRD fields change.

## Commit and PR Expectations

- Use clear commit messages.
- Include problem statement, change summary, and validation steps in PR description.
- If API/CRD behavior changes, include migration notes.

## Sign-off

By submitting contributions, you agree your contribution is licensed under AGPL-3.0-only.
