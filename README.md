# patchbot

Turn a GitHub issue into an approved spec and a tested pull request, using
Claude, Gemini or any OpenAI-compatible model.

Status: **early development, Step 1 in progress**. Currently, only CLI help
and version output are implemented; the other commands are placeholders.

## How it works (planned)

1. Refine the issue into a spec.
2. Wait for your approval.
3. Code the approved spec.
4. Test the changes.
5. Open a PR.

## Build and test

Requires Go 1.27 or later. Linting requires golangci-lint v2.

```sh
make build
./bin/patchbot version
./bin/patchbot help
make test
make race
make ci
```

See [the Step 1 plan](docs/PLAN.md) for the design and milestones.

Licensed under the [MIT License](LICENSE).
