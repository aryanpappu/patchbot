# M25 report: per-repository container image

Date: 2026-11-14. Branch `m25-repo-image`.

## What was done

- **Config**: optional `image` and `path` under `[repos."owner/name"]` override `[executor] image`/`path` for that
  repository only (`config.Config.RepoExecutor`). Validation matches the global image (`execx.ValidateImage`:
  digest-pinned `name[:tag]@sha256:<64 hex>`, `localhost/...@sha256:` allowed; `execx.ValidateContainerPath`).
  In container mode a repository with no image and no `[executor] image` is a config error naming the repo
  (`repos."owner/name".image: required ...`). With no repositories listed the old `executor.image: required`
  error remains. Local mode ignores the fields, like every other container setting.
- **Runner**: `Options.Executor` is now `func(dir string, config.RepoExecutor)`. `Runner.runExecutor` resolves the
  repo's image/path the first time a run needs an executor and records it as the run's `executor` artifact
  (JSON: image, path, image_source); every later clone, resume or revision of the run reads it back, so the image
  is frozen on the run and auditable. No migration. Local executor runs record nothing and are unchanged.
  The command policy for a run uses the frozen PATH.
- **Config API**: `CommandPolicyOptions(repo, ...)`, `ContainerConfig(repo, ...)`, `ContainerConfigFor(RepoExecutor, ...)`.
  `ContainerConfig("")` serves image-independent work (startup sweep, runtime checks) and uses a placeholder
  image when `[executor] image` is empty.
- **Doctor**: runtime and rootless checks stay global; for each repo it reports `container image <repo>`
  (digest-pinned, with the source key), `container image present <repo>` (`image inspect`), and its commands as
  `<program> (resolved in the container image) from <image>`.
- **`config show`**: per repo, `# effective container image: <image> (from repos."o/n".image | executor.image)`
  in container mode; set repo `image`/`path` print with their usual source comment, unset ones are omitted.
- **Docs**: `patchbot.example.toml`, `docs/ARCHITECTURE.md`, `docs/DECISIONS.md`.

## Tests

- `config.TestRepoImageValidation`, `TestRepoImageShownWithSource`
- `runner.TestPerRepoImageChangesContainerArgv` (two repos, different images/PATH in the runtime argv),
  `TestRepoImageOverridesGlobalAndFallsBack`, `TestRunExecutorFreezesAndRecordsImage`,
  `TestLocalExecutorRecordsNothing`
- `cli.TestDoctorContainerPerRepoImages`

## Open issues

- The run's frozen image is not shown in the web UI/`runs show`; it is in the `executor` artifact only.
- A repository's image is looked up once per run; a revision after a config change keeps the old image by design.
- Doctor runs `image inspect` once per repo even when repos share an image.
- A repo image or PATH that redaction would alter (credential-shaped) is refused at freeze time (fail closed), since the stored artifact is redacted and a resume would read it back corrupted.
