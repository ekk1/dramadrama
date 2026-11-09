# dramadrama

A workbench for producing short dramas through a simple, linear pipeline.
Every step is triggered by a person, every output can be edited before the
next step reads it, and every generated image lands in one shared asset
library.

Written in Go using only the standard library (no third-party modules).

## Pipeline

| # | Step | Backend | Output |
|---|------|---------|--------|
| 1 | Input | — | core idea, requirements, aspect ratio, style reference images |
| 2 | Idea expansion | LLM (streamed) | story concept (editable text) |
| 3a | Visual prompts | LLM (can see the reference images) | global style + characters / scenes / props with English image prompts |
| 3b | Concept images | fal.ai | one or more images per visual item |
| 4 | Storyboard overview | LLM | shot list: scene, characters, action, dialogue, camera, duration |
| 5 | Shot-level prompts | LLM, per shot or all | storyboard description, keyframe prompt, motion prompt (for future image-to-video) |
| 6 | Keyframes | fal.ai, per shot or all | keyframe images, conditioned on the matching concept images |

Notes:

- Re-running a step replaces its output in the project. Generated images stay in the library.
- If an LLM returns JSON that can't be parsed, the raw text is kept so you can fix it by hand and apply it.
- In step 6, each shot automatically uses the concept image currently in use for each character, scene, or prop it mentions. Attaching references to a shot explicitly overrides this.

## Running

```bash
go run ./cmd/dramadrama
```

Then open <http://127.0.0.1:8080>.

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
internal/pipeline  stage implementations and prompt templates (pipeline/prompts/*.tmpl)
internal/server    HTTP handlers (html/template, SSE for streaming runs)
web/               templates, CSS and JS (embedded into the binary)
```

### Adding a provider

- **LLM:** implement `llm.Client` (`Chat`, `Stream`) in `internal/llm` and register its type in `llm.New` and `llm.Types`.
- **Image:** implement `imagegen.Generator` and register it in `imagegen.New` and `imagegen.Types`.

Providers then become selectable in Settings.

### Video

Shots already carry a `motion_prompt` and a reserved `clip_id`, and the asset
library has a `video` kind. An image-to-video step would:

1. Read each shot's keyframe that is in use, plus its motion prompt.
2. Call a video backend through a new interface next to `imagegen`.
3. Store the clip as a `video` asset.

## Tests

```bash
go test ./...
```

The LLM and fal clients are tested against local fake HTTP servers. The
pipeline and web server are tested end to end with fake providers.
