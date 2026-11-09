# CLAUDE.md

Context for AI sessions working on this repository. User-facing docs are in
`README.md`; this file covers how to work here and what to keep consistent.

## Project

dramadrama is a workbench for producing short dramas through a linear,
human-triggered pipeline (no node graphs, nothing runs automatically):

1. Input: idea, requirements, aspect ratio, and style reference images
2. Idea expansion (LLM)
3. Visual development: (a) visual prompts (LLM), (b) concept images (fal.ai)
4. Storyboard overview (LLM)
5. Shot-level prompts (LLM)
6. Keyframes (fal.ai)

**Status:** stage 1 is complete (merged PRs #1 and #2). The full pipeline,
the asset library, the web UI and a zh/en interface all exist. Nothing has
been run against real provider APIs yet; all tests use fakes.

## Working agreements with the owner

- **Ask before acting** when requirements are unclear or there is a real
  design choice. Use clarifying questions and propose a plan first for
  anything non-trivial.
- **Dependencies:** standard library only. Do not add a third-party module
  without asking first, and only when writing it ourselves would be
  unreasonable. `go.mod` currently has no requirements.
- **Frontend:** server-rendered `html/template`, a separate CSS file, and a
  small vanilla JS file. No frontend framework or build step.
- **Git:** this repo is maintained only by AI. For each job:
  1. Branch from the latest `main`.
  2. Open a PR.
  3. Check that it's mergeable and tested.
  4. Merge it yourself; the owner does not need to approve.

  Never stack new commits on an already-merged branch.
- The owner reads Chinese and English. The UI must keep working in both.

## Commands

```bash
go run ./cmd/dramadrama -data data -addr 127.0.0.1:8080
```

Add `-dev ./web` to reload templates and static files on each request.

```bash
go vet ./...
```

```bash
go test -race ./...
```

```bash
gofmt -l .
```

Runtime data goes to `data/` (gitignored): config with API keys, projects,
assets, and prompt overrides. Keys come from `ANTHROPIC_API_KEY`,
`OPENAI_API_KEY`, `GEMINI_API_KEY` or `FAL_KEY`, or are stored via Settings.

## Architecture

```
cmd/dramadrama     wiring + flags
internal/llm       Client{Chat, Stream(iter.Seq2[Chunk,error])}; openai_chat,
                   openai_responses, anthropic, gemini; shared SSE reader (http.go)
internal/imagegen  Generator interface; fal.go (queue API: submit → poll → fetch)
internal/assets    central asset library: index.json + files/<sha256><ext>
internal/project   Project model + JSON store; Store.Update(id, fn) is the
                   only safe way to mutate (holds a lock across load/fn/save)
internal/config    providers, per-stage models, image settings (file mode 0600)
internal/pipeline  stage implementations, Service.Run dispatch, prompt templates
                   (prompts/*.tmpl, overridable from data/prompts), JSON parsing
internal/server    handlers, i18n, SSE run endpoint, settings, prompt editor
web/               embedded templates (layout, partials, pages) + static/app.{css,js}
```

## Invariants and conventions

- **Projects reference assets by ID.** Generated files always go through
  `assets.Store.Put`, with provenance filled in: kind, project, stage,
  shot, prompt, model, parents.
  - Re-running a stage replaces project data but never deletes assets.
  - Deleting an asset must also call `Project.RemoveAssetRefs` on every
    project.
- **The "in use" image is the last element of a list** (`AssetIDs`,
  `KeyframeIDs`). The UI's "use" action moves an image to the end, and step
  6 auto-references the last concept image of each matching visual.
- **Stage runs:**
  - Each run goes through `pipeline.Service.Run`, which:
    - takes a per-(project, stage:target) busy lock and makes the run
      cancellable;
    - records `Project.Runs[key]`.
  - The run handler streams `key`, `delta`, `status`, `error` and `done`
    SSE events.
  - Runs are detached from the HTTP request (`context.WithoutCancel`), so
    closing the page doesn't stop them; the page has a stop button instead.
- **Parsing LLM JSON:**
  - Always use `extractJSON`, which tolerates code fences and surrounding
    prose.
  - For list-producing stages, keep the raw text on parse failure
    (`VisualsRaw`, `StoryboardRaw`) so the user can fix it and apply it.
- **Language handling:**
  - Prompts ask the model to write story and storyboard text in the
    language of the user's idea.
  - Image, keyframe and motion prompts are always English.
  - Prompt templates live in `internal/pipeline/prompts/`; template data is
    `promptData`.
- **Provider quirks:**
  - **Anthropic:** sends `anthropic-version: 2026-10-21` and must never
    send `temperature`/`top_p`/`top_k`, which return a 400 on that
    version. Thinking deltas and blocks are ignored. The default model is
    `claude-opus-5-5`.
  - **fal:** sends both `image_size` and `aspect_ratio`, and both
    `image_url` and `image_urls`, because endpoints differ.
    `ImageConfig.Extra` is merged last so it can override anything.
  - **Missing API keys:** `keyCheck` fails early on a missing key, except
    for `openai_chat`, because local servers often need none.
- **i18n (required for every UI change):**
  - Templates use `{{t "English text"}}`, which is bound per request in
    `Server.render`.
  - Add every new key to the `zh` map in `internal/server/i18n.go`.
  - Messages that embed values need a regex in `zhPatterns`.
  - JS strings go in `jsKeys` and are read in JS with `T("…")`.
  - New fixed `errors.New` messages in `pipeline.go` also need a `zh`
    entry.
  - `TestTemplateKeysTranslated` and `TestPipelineErrorsTranslated`
    enforce all of this.
- **Security:**
  - There is no authentication, so the server binds to localhost.
  - `Server.ServeHTTP` rejects cross-origin state-changing requests.
  - API keys are never rendered back to the page.
  - `back` redirect targets must pass `back()` validation (local paths
    only).
- **Video readiness:** shots already have `MotionPrompt` and `ClipID`, and
  the asset library has `KindVideo`. Image-to-video should be a new
  interface next to `imagegen`, plus a new stage `clips` after
  `keyframes`.

## Adding things

- **New LLM provider:**
  1. Implement `llm.Client` in `internal/llm`.
  2. Add its type constant, `Types` entry and `New` case.
  3. Add a fake-server test in `llm_test.go`.
- **New image provider:** same pattern in `internal/imagegen`.
- **New stage:**
  1. Add a constant in `project`, a case in `Service.Run`, and a `run…`
     method.
  2. Add prompt templates if it's an LLM stage, plus `Prompts.Names()` and
     `config.LLMStages` entries.
  3. Add a UI section in `project.html` using the `opts`, `runbtn`,
     `status` and `log` partials.
  4. Add translations for any new strings.
  5. Add pipeline and server tests.

## Testing approach

- Provider clients are tested against `httptest` fake servers that check
  the request shape and replay recorded-style responses, both streamed and
  non-streamed.
- Pipeline and server tests swap `Service.NewLLM` and `Service.NewImage`
  for fakes and drive every stage end to end, including over HTTP and SSE.
- For UI changes, also check in a browser. Playwright and Chromium are
  available in cloud sessions: start the binary, seed data through the
  HTTP endpoints, and screenshot.

## Known gaps / next candidates

- Do a first run with real `ANTHROPIC_API_KEY` and `FAL_KEY`, and fix
  anything that shows up.
- There is no CI. A GitHub Actions workflow running vet and race tests on
  PRs would help.
- `fal-ai/flux-pro/kontext` takes a single image, so with several
  references only the first is used. Consider a multi-reference default.
- Image-to-video stage (see Video readiness above).
- There is no auth. It's needed before exposing the server beyond
  localhost.
