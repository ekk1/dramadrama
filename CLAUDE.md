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
7. Video clips (fal.ai): per shot, from the keyframe (first frame), an
   optional last frame, reference images/videos/audio and the motion prompt

**Status:** the full pipeline including video, the asset library, the web
UI (zh/en), a request log and per-provider model lists exist. Nothing has
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
assets, prompt overrides, cached model lists and input schemas, and the
request log. Keys come from `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`,
`GEMINI_API_KEY` or `FAL_KEY`, or are stored via Settings.

## Architecture

```
cmd/dramadrama     wiring + flags
internal/llm       Client{Chat, Stream(iter.Seq2[Chunk,error])}; openai_chat,
                   openai_responses, anthropic, gemini; shared SSE reader (http.go)
internal/imagegen  media generation (images and video, despite the name): Generator
                   and Queued (Submit/Wait/Cancel) interfaces; fal.go (queue API);
                   schema.go (model input lists, role → field matching)
internal/assets    central asset library: index.json + files/<sha256><ext>
internal/project   Project model + JSON store; Store.Update(id, fn) is the
                   only safe way to mutate (holds a lock across load/fn/save)
internal/config    providers, per-stage models, image and video settings
                   (file mode 0600)
internal/catalog   model lists and video-model input schemas fetched from
                   providers, cached under data/models/; Fetch and FetchSchema
                   dispatch to llm/imagegen
internal/reqlog    request log: one JSON file per model request under
                   data/logs/<day>/; no index, the directory tree is the database
internal/pipeline  stage implementations, Service.Run dispatch, prompt templates
                   (prompts/*.tmpl, overridable from data/prompts), JSON parsing;
                   media.go: resumable provider jobs and the clips stage
internal/server    handlers, i18n, SSE run endpoint, settings, prompt editor,
                   request log pages
web/               embedded templates (layout, partials, pages) + static/app.{css,js}
```

## Invariants and conventions

- **Projects reference assets by ID.** Generated files always go through
  `assets.Store.Put`, with provenance filled in: kind, project, stage,
  shot, prompt, model, parents.
  - Re-running a stage replaces project data but never deletes assets.
  - Deleting an asset must also call `Project.RemoveAssetRefs` on every
    project.
- **The "in use" item is the last element of a list** (`AssetIDs`,
  `KeyframeIDs`, `EndFrameIDs`, `ClipIDs`). The UI's "use" action moves an
  item to the end, and step 6 auto-references the last concept image of
  each matching visual. `VideoRefIDs` and `AudioRefIDs` have no "in use"
  item: all of them are sent.
- **Reference lists are typed by media.** `refList` maps a target such as
  `keyframe:<shot>` or `vref:<shot>` to its list and `targetMedia` says
  whether it holds images, video or audio; adding and uploading check it.
  A new list needs both, plus `Project.RemoveAssetRefs`.
- **Stage runs:**
  - Each run goes through `pipeline.Service.Run`, which:
    - takes a per-(project, stage:target) busy lock and makes the run
      cancellable;
    - records `Project.Runs[key]`.
  - The run handler streams `key`, `delta`, `status`, `error` and `done`
    SSE events.
  - Runs are detached from the HTTP request: `handleRun` starts the run
    in a goroutine that publishes its events to a `runFeed` (feed.go) and
    then follows that feed like any other reader. Closing or leaving the
    page doesn't stop a run; the page has a stop button instead.
  - A page follows the runs it finds in progress when it loads
    (`resumeRuns` in `app.js`, `GET /projects/{id}/events?key=`), from
    their first event. The `status` partial marks a running step with
    `data-running` and the `runbtn` partial carries the same key in
    `data-key`. A run request for a step that is already running follows
    that run instead of failing.
- **Media jobs are resumable** (`Service.runJob`, used for images and
  video):
  - The provider's job handle is saved in `Project.Jobs[stage:item]` as
    soon as the request is accepted and removed when the result is
    collected or the provider gives a final error.
  - `imagegen.Retryable` decides what is not final: cancellation,
    network errors, 5xx and 429. Those keep the job.
  - A later run for the same item collects the saved job if its
    fingerprint (model + redacted payload) is unchanged; otherwise it
    cancels that job and submits a new one. Nothing resumes on its own.
  - The page shows a note with a "discard" action for a saved job.
- **Request log:**
  - Every model request is logged through `Service.writeLog`: LLM calls
    in `complete`, media generation in `generateImage` and
    `generateClip`. A new kind of request must be logged the same way,
    saying which project, stage and item it is for.
  - Entries hold the full prompt and output. They must never contain API
    keys, and binary inputs are recorded as asset IDs. Generators expose
    the body they send through `imagegen.PayloadDescriber`, with data
    URIs redacted.
  - The owner wants the filesystem to be the database: one file per
    request, no index file. The server only lists, reads and deletes.
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
  - New fixed `errors.New` messages in `pipeline.go` and `media.go` also
    need a `zh` entry.
  - `TestTemplateKeysTranslated` and `TestPipelineErrorsTranslated`
    enforce all of this.
- **Security:**
  - There is no authentication, so the server binds to localhost.
  - `Server.ServeHTTP` rejects cross-origin state-changing requests.
  - API keys are never rendered back to the page.
  - `back` redirect targets must pass `back()` validation (local paths
    only).
- **Settings and model lists:**
  - The settings page has tabs (`?tab=`). Each tab saves through its own
    endpoint with `config.Manager.Update`, which applies a function to a
    copy and validates before writing. Providers are edited one at a
    time in a dialog; renaming or deleting one goes through
    `renameProvider`, which must cover every setting that names a
    provider.
  - Model lists are fetched only when the user asks (provider dialog or
    the model picker) and cached by `catalog.Store`. Nothing is fetched
    automatically.
  - fal's catalogue is on `api.fal.ai`, a different host from the queue.
    `Provider.CatalogURL` overrides it; it is set only in the config
    file. The response shape was taken from fal's docs and has not been
    exercised against the live API.
  - Every model ID input uses the `modelfield` partial inside a
    `[data-model-scope]` that contains the `select[data-provider]`.
  - Dialogs are `<dialog class="modal">`, opened with `data-dialog="id"`
    and closed with `data-close`.
- **Frontend conventions:**
  - Colours, radii and shadows are CSS custom properties at the top of
    `app.css`, with a dark set under `prefers-color-scheme: dark`. Use the
    tokens; do not hard-code colours.
  - Plain "edit and save" forms carry `data-save`. `app.js` marks them
    dirty on input and posts dirty forms in the background before a run
    or another form post reloads the page, so edits are never lost. Do
    not put `data-save` on forms whose post has side effects (add, delete,
    apply JSON).
  - A project has one page per step, `/projects/{id}/step/{n}`, all
    rendered from `project.html` (`projectView.Step` picks the section).
    `/projects/{id}` redirects to the furthest step that has content.
    Forms send the page they are on as `back`; the handlers' default
    redirect is the step the form belongs to.
  - The step pages are "live" (`data-live` on their content): `app.js`
    sends form posts with fetch and merges the page the server answers
    with into the current one (`apply`/`syncChildren`), and does the same
    when a run ends or reports progress. Handlers stay plain
    post-and-redirect, and without JS the page still works. The merge
    pairs elements by `id`, then by position, so give list items (cards,
    shots) an `id`. It never touches forms with unsaved edits, run logs
    or a running run button (`keep`). Other pages still reload.
  - In JS read a form's URL with `form.getAttribute("action")`: shot
    forms have a field named `action` that shadows `form.action`.
  - The step navigation comes from `projectSteps` (server.go); update it
    when adding a stage.
  - Uploads must go through `saveUploads`, which caps the request body
    before any form value is read.
- **Video requests are described by role, not by field name.**
  - `imagegen.Request` carries `FirstFrame`, `LastFrame`, `References`,
    `RefVideos`, `RefAudios` and `Duration`. `ResolveRoles` matches them
    to the model's fields from its `InputSchema`, which is parsed from the
    OpenAPI document fal publishes per model and cached by
    `catalog.Store` on first use.
  - Without a schema only `prompt` and `image_url` are assumed.
    `VideoConfig.Mapping` overrides the match, for the default model only.
  - An input the model cannot take is never dropped silently: the
    generator's `Explain` notes are emitted as status lines.
  - The owner cares most about first/last frame plus reference video and
    audio, with `minimax/h3-max/reference-to-video` (the default model)
    as the reference case. Its real field names have not been verified:
    the fal hosts were unreachable from the sessions that wrote this.
  - Files go to fal inline as data URIs.

## Adding things

- **New LLM provider:**
  1. Implement `llm.Client` in `internal/llm`, and `listModels` in
     `models.go`.
  2. Add its type constant, `Types` entry and `New` case.
  3. Add fake-server tests in `llm_test.go`.
- **New image or video provider:** same pattern in `internal/imagegen`.
  Also implement `Queued` if requests can be collected later (that is
  what makes them resumable), `PayloadDescriber` for the request log,
  `Explainer` for video, and model listing in `models.go`.
- **New stage:**
  1. Add a constant in `project`, a case in `Service.Run`, and a `run…`
     method.
  2. Add prompt templates if it's an LLM stage, plus `Prompts.Names()` and
     `config.LLMStages` entries.
  3. Add a UI section in `project.html` (inside `{{if eq $step N}}`)
     using the `opts`, `runbtn`, `status` and `log` partials, a step in
     `projectSteps`, and a display name in `stageNames` (logs.go).
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
- Verify against the live fal API: the model list, the per-model OpenAPI
  document (`expand=openapi-3.0`, read from an `openapi` key), the queue
  cancel URL, and the role matching for the default video model.
- Large reference videos are sent inline; fal's storage upload would be
  better but its API was not verified.
- There is no step that joins the clips into one video (ffmpeg would be
  an external dependency; ask first).
- Last frames are picked by hand or taken from the next shot's keyframe;
  there is no prompt or generation step for a dedicated end frame.
- `fal-ai/flux-pro/kontext` takes a single image, so with several
  references only the first is used. Consider a multi-reference default.
- There is no auth. It's needed before exposing the server beyond
  localhost.
- The `Host` header is not validated, so a DNS-rebinding page could drive
  the localhost server. Rejecting unexpected hosts needs a decision on
  how LAN use (`-addr 0.0.0.0`) should be allowed.
- A provider stream that closes early without an error is only reported
  as a warning in the run log; the truncated text is still saved.
