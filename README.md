# dramadrama

A workbench for producing short dramas through a simple, linear pipeline.
Every step is triggered by a person, every output can be edited before the
next step reads it, and every generated image and video clip lands in one shared
asset library.

Written in Go using only the standard library (no third-party modules).

## Pipeline

| # | Step | Backend | Output |
|---|------|---------|--------|
| 1 | Input | — | core idea, requirements, aspect ratio, style reference images |
| 2 | Idea expansion | LLM (streamed) | story concept (editable text) |
| 3a | Visual prompts | LLM (can see the reference images) | global style + characters / scenes / props with English image prompts |
| 3b | Concept images | fal.ai | one or more images per visual item |
| 4 | Storyboard overview | LLM | shot list: scene, characters, action, dialogue, camera, duration |
| 5 | Shot-level prompts | LLM, per shot or all | storyboard description, keyframe prompt, motion prompt (the video prompt) |
| 6 | Keyframes | fal.ai, per shot or all | keyframe images, conditioned on the matching concept images |
| 7 | Video clips | fal.ai, per shot or all | one clip per shot, opening on its keyframe |

Notes:

- Re-running a step replaces its output in the project. Generated images stay in the library.
- If an LLM returns JSON that can't be parsed, the raw text is kept so you can fix it by hand and apply it.
- In step 6, each shot automatically uses the concept image currently in use for each character, scene, or prop it mentions. Attaching references to a shot explicitly overrides this.
- In step 7, a clip's first frame is the shot's keyframe in use. Each shot can also have a last frame (any image, or one click for the next shot's keyframe), reference videos and reference audio. The same reference images as in step 6 are sent too.
- Image and video requests go through fal's queue and are polled automatically. The request ID is saved in the project while a request is in flight, so if a run is stopped or the server restarts, running the step again collects the finished result instead of generating (and paying) again. If the prompt or inputs changed meanwhile, the old request is cancelled and a new one is sent.

## Running

```bash
go run ./cmd/dramadrama
```

Then open <http://127.0.0.1:8080>.

The interface is available in English and Simplified Chinese:

- Switch with the `中文 | EN` toggle in the top bar; the choice is kept in a cookie.
- Without a cookie, the browser's language decides.
- Translations live in `internal/server/i18n.go`, keyed by the English text.
- A test fails if a template string has no Chinese translation.

Flags:

| Flag | Default | |
|------|---------|-|
| `-addr` | `127.0.0.1:8080` | listen address |
| `-data` | `data` | data directory |
| `-config` | `<data>/config.json` | config file |
| `-dev` | | serve templates/static from a directory (e.g. `./web`) and re-parse on each request |

On first start, a config file is created with these providers:

| Provider | Type | Key from |
|----------|------|----------|
| anthropic | Messages API | `ANTHROPIC_API_KEY` |
| openai | Responses API | `OPENAI_API_KEY` |
| openai-compatible | Chat Completions | `OPENAI_API_KEY` |
| gemini | Gemini API | `GEMINI_API_KEY` |
| fal | fal.ai | `FAL_KEY` |

Export the variables you need, or store keys on the **Settings** page.

Settings has four tabs: **Providers** (one card per provider; click to edit,
fetch its model list, or delete), **Language models** (default provider and
model per step), **Image generation** and **Video generation**. Every model field has a "Browse"
button that opens the provider's model list with search; for fal the list can
be filtered by category (text-to-image, image-to-image, image-to-video, …).
Typing a model ID by hand still works.

The workbench has no login. It listens on localhost by default and rejects
cross-origin form posts. Don't expose it to a network without putting
authentication in front of it.

## Data layout

```
data/
  config.json                 providers, keys, per-step models (mode 0600)
  projects/<id>/project.json  project state; references assets by ID
  assets/index.json           asset metadata (kind, provenance, tags, …)
  assets/files/<sha256>.<ext> asset content, de-duplicated
  prompts/<name>.tmpl         prompt template overrides (edit on the Prompts page)
  models/<provider>.json      model list fetched from each provider (Settings → provider
                              → "Fetch model list", or from any model picker)
  models/<provider>.schemas/  input lists of the video models that have been used
  logs/<day>/<time>_….json    request log: one file per LLM call or media generation,
                              with the full prompt and output (view on the Logs page)
```

## Code layout

```
cmd/dramadrama     entry point
internal/llm       provider-neutral LLM client: Chat + Stream (iter.Seq2)
                   openai_chat, openai_responses, anthropic, gemini; shared SSE reader
internal/imagegen  image generation interface; fal.ai queue API client
internal/assets    asset library
internal/project   project model and JSON store
internal/config    configuration
internal/catalog   cached provider model lists
internal/reqlog    request log (one JSON file per model request)
internal/pipeline  stage implementations and prompt templates (pipeline/prompts/*.tmpl)
internal/server    HTTP handlers (html/template, SSE for streaming runs)
web/               templates, CSS and JS (embedded into the binary)
```

### Adding a provider

- **LLM:** implement `llm.Client` (`Chat`, `Stream`) in `internal/llm` and register its type in `llm.New` and `llm.Types`.
- **Image:** implement `imagegen.Generator` and register it in `imagegen.New` and `imagegen.Types`.

Providers then become selectable in Settings.

### Video models

Video models name their inputs differently (`image_url`, `first_frame_image_url`,
`tail_image_url`, a duration that is a number or a string enum, …). The clip
request describes its inputs by role: prompt, first frame, last frame,
reference images, reference videos, reference audio, duration, aspect ratio.
The roles are matched to the model's fields using the input list fal publishes
for each model, which is fetched on first use and cached under `data/models/`.

**Settings → Video generation** shows how the chosen model's inputs are matched,
lists its other inputs (set them through "Extra parameters"), and takes an
optional override when the automatic match is wrong. Inputs the model lacks are
skipped, and the run log says so. The duration is rounded to the nearest value
the model accepts.

Files are sent to fal inline as data URIs. That is fine for frames and short
clips; large reference videos make the request heavy.

## Tests

```bash
go test ./...
```

The LLM and fal clients are tested against local fake HTTP servers. The
pipeline and web server are tested end to end with fake providers.
