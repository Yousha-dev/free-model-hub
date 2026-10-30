# Free Model Hub

A single native CLIProxyAPI plugin that puts free and free-tier model providers behind the proxy. It loads into the official CLIProxyAPI as a native plugin (`free-model-hub.dll` / `.so` / `.dylib`), so the official proxy and UI stay unmodified and updatable.

It merges five community plugins after a source review (see [NOTICE.md](NOTICE.md)), keeping their working parts and dropping the risky ones.

## What it connects

| Group | Providers | Account needed | Model IDs |
| --- | --- | --- | --- |
| Keyless | **Kilo free pool** | None (added automatically) | `kilo/<model>` |
| Keyless | **OpenCode Zen free models** | None (added automatically) | `big-pickle`, `*-free` |
| OpenCode | **OpenCode Go** subscription | Go API key | `opencode-go/<model>` |
| OpenCode | **OpenCode Zen** with your key | Zen API key | free models only |
| API keys | OpenRouter (`:free` models), GitHub Models, NVIDIA NIM, Groq, Cerebras, Mistral, Codestral, SambaNova, LLM7, Cohere, Ollama Cloud, Vercel AI Gateway (monthly free credit), Z.ai (free `-Flash` models only) | Your own free-tier key | `<provider>/<model>` |
| Logins | Cline, Qoder (CN/Global), Trae (CN/Global), WorkBuddy (CN/Global), CodeArts, MiniMax Code (CN/Global), ZCode, AutoClaw (CN/Global), Raccoon | Sign-in from the console | provider's own names |

All upstream traffic goes through the host's HTTP bridge, so CLIProxyAPI's proxy settings, request logs and usage statistics apply.

Pay-only providers (Hugging Face, Chutes, Together, Fireworks, DeepInfra, SiliconFlow, Nebius, Hyperbolic, Scaleway, DeepSeek, Moonshot) are not included: they have no free models or recurring free tier.

## Install

Install **Free Model Hub** from the Plugin Store in the CLIProxyAPI management UI. To install manually, download the zip for your platform from the [latest release](https://github.com/Yousha-dev/free-model-hub/releases/latest), extract the library into the proxy's plugin directory, and enable it:

```yaml
plugins:
  enabled: true
  configs:
    free-model-hub:
      enabled: true
```

## Use it

1. Open the console: `http://127.0.0.1:8317/v0/resource/plugins/free-model-hub/console` (or **Plugins → Free Model Hub** in the management UI) and enter the management key.
2. Keyless providers work immediately. For the others, use **Add account** and pick the provider, then paste a key or complete the sign-in.
3. Call any model through the proxy with a normal client key, for example `kilo/kilo-auto/free`, `big-pickle`, `openrouter/meta-llama/llama-3.3-70b-instruct:free` or `opencode-go/<model>`.

The console, logs, error messages and settings are in English.

## Settings

Set under `plugins.configs.free-model-hub` in the proxy config, or in the console:

| Key | Default | Meaning |
| --- | --- | --- |
| `keyless_accounts` | `true` | Create the Kilo and OpenCode Zen keyless accounts once. Deleting one keeps it deleted. |
| `openrouter_paid_models` | `false` | Also list OpenRouter paid models (spends your credits). |
| `cline_pass_models` | `false` | Also list Cline Pass (subscription) models. Leave off unless you subscribe; they fail with "not subscribed" on free accounts. |
| `openrouter_app_url`, `openrouter_app_title` | `https://cline.bot`, `Cline` | App identity sent to OpenRouter. Some `:free` models (e.g. `thinkingmachines/inkling:free`) are only served to coding agents OpenRouter recognises. |
| `opencodezen_base_url`, `opencodego_base_url` | official | Upstream overrides. |

## Behavior notes

- **OpenCode Zen free tier** is reached the way the official OpenCode CLI reaches it: the public credential, the CLI's client headers and user agent, and canonical `ses_…` session IDs. Zen enforces a per-network free usage limit; once reached, every client (including the official CLI) gets `FreeUsageLimitError` until it resets.
- **OpenRouter** requests carry the Cline coding agent's app identity (`HTTP-Referer`/`X-Title`), which unlocks agent-only free models. Change it with `openrouter_app_url` / `openrouter_app_title`.
- **Free pools may log prompts.** Kilo and OpenCode Zen state that free-tier prompts can be used to improve their services. Do not send secrets through them.
- The login providers (Cline, Qoder, Trae, and the rest) emulate their official IDE clients. Using them may break those services' terms, and accounts can be suspended.
- The only background job is a daily token keepalive (default 22:00; change the hours in the console settings) so login accounts stay signed in. Daily check-ins and reward tasks from the base plugin were removed.

## Build

```sh
go test ./...                                   # needs CGO_ENABLED=1 and a C compiler
go build -buildmode=c-shared -o free-model-hub.so .
```

Release builds for Windows, Linux and macOS are produced by `.github/workflows/release.yml` when a `v*` tag is pushed.

Plugin state (machine salt, model cache, keyless markers) lives in `~/.free-model-hub` (`%USERPROFILE%\.free-model-hub` on Windows), or `FREE_MODEL_HUB_HOME` if set.

## Layout

| Path | Purpose |
| --- | --- |
| `internal/vendors/openaicompat` | Generic OpenAI-compatible providers, including the keyless Kilo pool |
| `internal/vendors/opencodezen` | OpenCode Zen (keyless free lane plus optional key) |
| `internal/vendors/opencodego` | OpenCode Go vendor glue |
| `internal/ocgo` | OpenCode Go catalog and protocol translators (chat ↔ messages ↔ responses) |
| `internal/vendors/*` | Login providers from the base plugin |
| `internal/admin` | Console page and management API |
| `tests` | Architecture guards, integration and end-to-end tests |
