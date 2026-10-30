# Sources, licenses and review

Free Model Hub combines code and findings from five CLIProxyAPI community plugins, reviewed on 2026-10-29 at the commits below.

| Repository | Commit | License | Used for |
| --- | --- | --- | --- |
| [jiandanc/CLIProxyAPI-freetier2api-plugin](https://github.com/jiandanc/CLIProxyAPI-freetier2api-plugin) | `e90609d0138c0cfd7f1e00c0f8c827f8e5c22d11` | MIT (`LICENSE`) | Base: plugin ABI layer, console, login providers, OpenCode Zen lane |
| [xspeed1989/opencode-go-cliproxyapi](https://github.com/xspeed1989/opencode-go-cliproxyapi) | `ede6a197fd69b24103dc15887ce0f517def8769a` | MIT (`internal/ocgo/LICENSE-opencode-go-cliproxyapi`) | OpenCode Go catalog and protocol translators (`internal/ocgo`) |
| [dingguotu/oc2cpa](https://github.com/dingguotu/oc2cpa) | `1876f22864e484bccd5c1dd683fef10942b60d52` | MIT | Reference for the keyless OpenCode Zen account; no code copied |
| [tsaQB/cpa-freetier-plugin](https://github.com/tsaQB/cpa-freetier-plugin) | `1a51670c77238e7f2cc91d7edb6ea4ed73fdeca0` | README claims MIT; no LICENSE file | Reference for the keyless Kilo free pool; no code copied |
| [sy0u1ti/cpa-our-free-model](https://github.com/sy0u1ti/cpa-our-free-model) | `a673eb52f9db94b6776863900a3861e48286b513` | MIT | Provider endpoint list (first-party providers only); no code copied |

## Removed or not adopted

| Item | Source | Reason |
| --- | --- | --- |
| ~35 MB Windows engine binary split into `.part` files | sy0u1ti | Opaque executable that cannot be audited |
| Node.js child process and Node auto-download | sy0u1ti | Large unaudited runtime surface |
| mihomo proxy spawning, `gh-proxy.com` update and feed fetches | sy0u1ti | Runs third-party binaries, routes updates through a mirror |
| EAC lane (GitHub login plus repository star, author's gateway) | sy0u1ti | Prompts and tokens pass through a third-party gateway |
| `fleetrelay_t1` (`vpn2.asigo.top`) and other unknown relays/resellers | sy0u1ti | Anonymous relays would see every prompt and key |
| OmniRush gateway | tsaQB | Unknown reseller; prompts would leave for an unvetted service |
| OpenCode system prompt and fake tools injected into every request | tsaQB | Silently alters every client's prompts |
| Local "zen-proxy" sidecar for TLS fingerprint evasion | tsaQB | Third-party bypass component |
| Daily check-ins, growth/reward tasks (travel, lottery, "blackcat", school, daily bonus, scripted fake chats), check-in history and the "quota reset = check-in" hook | jiandanc | Reward farming on upstream accounts: no use for normal model access and a ban risk |
| Store manifest, packaging scripts, CI workflow | jiandanc | Not published to a store from here |

## Behavior changes from the base plugin

- Plugin ID `free-model-hub`, provider key `freehub`, state directory `~/.free-model-hub` (`FREE_MODEL_HUB_HOME`).
- Check-ins and reward tasks are removed; the scheduler only runs token keepalive. Quota reset reports "not supported".
- Console, logs, errors and comments translated to English. Strings matched against upstream responses or sent upstream stay in their original language, since vendors check them.
- OpenCode Zen works without a key through a keyless account; paid Zen models are refused on that account before any upstream call.
- New vendors: generic OpenAI-compatible providers (with the keyless Kilo pool) and OpenCode Go.
- Tests no longer write plugin state into the real home directory.
