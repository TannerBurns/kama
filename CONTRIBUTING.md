# Contributing to Kama

Thank you for helping improve Kama. Contributions of code, tests, documentation, and
design feedback are welcome. Participation in this project is governed by the
[Code of Conduct](CODE_OF_CONDUCT.md).

## Before contributing

- Search existing issues and pull requests before opening a duplicate.
- Use an issue to discuss substantial features or behavior changes before investing
  in an implementation.
- Report vulnerabilities through the private process in [SECURITY.md](SECURITY.md),
  not through a public issue.
- Record changes to accepted architecture or public contracts in an ADR under
  `docs/adr/` and update the affected project-plan documents and tests.

## Development workflow

Kama's supported tool versions are pinned by the repository. Install the local tools
and run the standard verification suite with:

```console
make bootstrap
make verify
```

Run `make test-kind` for changes that affect installation, Kubernetes compatibility,
KEDA integration, or uninstall behavior. Run `make help` for the complete developer
command contract. Generated code and manifests must be produced through the Make
targets; do not edit generated output by hand.

Keep each pull request focused. Include:

- a clear problem statement and explanation of the chosen behavior;
- tests for new behavior and regressions;
- documentation for user-facing or operational changes; and
- the commands used to validate the change.

All required GitHub Actions checks must pass before merge. Maintainers may request
changes when a pull request lacks compatibility evidence, introduces an unsupported
dependency, or changes a public contract without an ADR.

## Go source headers

New Go files use this Apache-2.0 header:

```go
// Copyright 2026 Kama Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
```

By submitting a contribution, you agree that it is licensed under the repository's
[Apache License 2.0](LICENSE).
