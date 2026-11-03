# ADR 0005: Generated protobuf code is committed

**Status:** accepted · **Issue:** #2

## Context

The Go code generated from `proto/` can be committed or generated in CI and on every developer's machine.

## Decision

The generated code in `gen/go/` is **committed**. CI runs `buf generate` and fails if the result differs from what is in the repository, and `buf breaking` fails on incompatible changes against `main`.

## Consequences

- `go build` and `go test` work from a plain checkout, without buf or any protoc plugin.
- API changes are visible in review, in the generated code as well as in the `.proto` files.
- After changing a `.proto` file, run `task proto` and commit the result. CI says so if that was forgotten.
