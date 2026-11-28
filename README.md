# DevPilot hook daemon

The **hook daemon** of the DevPilot project, in [`devpilot-app/hook`](https://github.com/devpilot-app/hook): a long-running Go process on your dev machine that feeds coding-agent hook events (Claude Code / Codex / OpenCode / Gemini CLI / Qwen Code / Grok Build / Cursor CLI / GitHub Copilot CLI) (including approval requests) into the cloud push pipeline and routes approval decisions back to the agent — the loop stays closed even when the app has been killed or the phone is locked.

> **License:** source-available under [FSL-1.1-ALv2](LICENSE.md) — free to install, audit and modify; shipping it (or a substantially similar daemon) as a competing product needs a commercial license. Details under [License](#license).

## How the three projects fit together

Two Go repos and one iOS app, one chain:

| | DevPilot (iOS app) | **[devpilot-app/hook](https://github.com/devpilot-app/hook) (this repo)** | [devpilot-app/apns](https://github.com/devpilot-app/apns) |
|---|---|---|---|
| What it is | iOS app (Swift/SwiftUI) | **Go daemon on the dev machine** | Go relay on a cloud VPS |
| Responsibility | Terminal/SSH, inbox UI, approval panel, Live Activity, APNs token registration | Hook adapters (8 agents), local gateway on 127.0.0.1:24543, event aggregation & rate limiting, offline buffering | WSS ingest, pairing routing table, APNs push, Live Activity remote-update forwarding |
| Role in the chain | WSS client (app side): receives pushes, sends approval decisions upstream | WSS client (daemon side): sends events upstream, receives approval decisions | Hub: auth, routing, APNs |
| Code boundary | App-side integration (RelayChannel model, etc.) | The daemon itself; `pkg/wire` imported from `github.com/devpilot-app/apns` via go.mod | The whole relay + the **`pkg/wire` protocol type library (single source of truth for the wire protocol across all three ends)** |

```
Claude Code / Codex / OpenCode events
  → hook daemon (this repo, local gateway 127.0.0.1:24543)
  → APNs relay (devpilot-app/apns, VPS: auth → routing table → channel selection)
  → Apple APNs (HTTP/2) ── or ── direct push over the app's WSS connection
  → iPhone (notification / Live Activity / inbox)
Approval flows back: iPhone Allow/Deny → relay → daemon (this repo) → hook unblocks, agent continues
```

Design rules: a timeout anywhere in the approval chain falls back to the terminal's native prompt (worst case you are asked twice; it never approves on its own); the cloud channel only carries summary fields you could already see in the notification; benchmarked against moshi-hook.

Question cards are the one exception: Claude Code's `AskUserQuestion` is not a permission prompt, so the daemon **notifies without blocking** — the hook returns immediately, the native question shows up in the terminal, and the phone card is only a heads-up with an "Answer at Terminal" jump into that agent session. The card closes itself once the question is answered (`PostToolUse` → `approval_closed(cancelled)`); no decision ever travels back for questions.

## Install

macOS and Linux only (same scope as moshi-hook; no Windows).

```bash
# macOS (Homebrew — the same tap + install two-step as moshi)
brew tap devpilot-app/devpilot
brew install --cask devpilot-hook

# Or in one command (Homebrew clones the tap automatically)
brew install --cask devpilot-app/devpilot/devpilot-hook

# macOS / Linux (script: verifies SHA-256, installs to ~/.local/bin)
curl -fsSL https://raw.githubusercontent.com/devpilot-app/hook/main/install.sh | sh
```

> `devpilot-app/devpilot` is a third-party tap backed by the GitHub repo
> `devpilot-app/homebrew-devpilot`; it is not part of the official Homebrew
> repositories. moshi's `rjyo/moshi` works the same way against
> `rjyo/homebrew-moshi`. The tap only needs to be added once; afterwards
> `brew upgrade --cask devpilot-hook` is enough.
>
> On recent Homebrew versions a third-party tap must be trusted once:
> `brew trust devpilot-app/devpilot`.

On Linux you can also grab `.deb` / `.rpm` / `.tar.gz` from Releases directly.

```bash
devpilot-hook install            # writes Claude Code / Codex hooks (add --agent qwen,gemini,grok,cursor,copilot,opencode for the rest)
devpilot-hook service install    # launchd / systemd user auto-start
devpilot-hook pair               # defaults to the official hosted relay relay.devpilot.app; prints a QR code + pairing code in the terminal
devpilot-hook pair --relay https://relay.example.com   # explicit relay for self-hosting (dual track)
devpilot-hook doctor             # checks config, hooks, gateway, relay and queue
devpilot-hook serve              # runs in the foreground; the service manager runs it too
```

The `pair` address fallback chain: `--relay` > `relay_url` in config.json > official hosted relay (`config.DefaultRelayURL`). Scan the terminal QR code with the DevPilot iOS app to pair (payload is `devpilot://pair?v=1&relay=…&code=…`), or type the pairing code manually; `--no-qr` disables the QR code. `unpair` best-effort calls the relay's revoke endpoint to cascade-delete cloud data; failure (offline / older relay) does not affect local unbinding.

`uninstall` removes only its own hook entries and auto-start config; `--purge` wipes the state directory.

### Supported agents

| Agent | CLI | Hooks config | Phone approve | Phone deny | Notes |
|---|---|---|---|---|---|
| Claude Code | `claude` | `~/.claude/settings.json` | ✅ `PermissionRequest` answered remotely | ✅ | AskUserQuestion = notify card (Answer at Terminal) |
| Codex CLI | `codex` | `~/.codex/hooks.json` | ✅ | ✅ | |
| Qwen Code | `qwen` | `~/.qwen/settings.json` | ✅ `PermissionRequest` | ✅ | PreToolUse gating is a no-op upstream; approvals only via `PermissionRequest` |
| GitHub Copilot CLI | `copilot` | `~/.copilot/hooks/devpilot.json` | ✅ `permissionRequest` short-circuits the native prompt | ✅ | |
| Cursor CLI | `agent` / `cursor-agent` | `~/.cursor/hooks.json` (merged with your entries) | gate mode only (`preToolUse`) | ✅ | No native-prompt hook: default mode = notifications only |
| Gemini CLI | `gemini` | `~/.gemini/settings.json` | gate mode only (`BeforeTool`) | ✅ | See gate mode below |
| Grok Build | `grok` | `~/.grok/hooks/devpilot.json` | gate mode only (`PreToolUse`) | ✅ | Grok also reads `~/.claude/settings.json`; those entries are suppressed at runtime |
| OpenCode | `opencode` | global plugin (see below) | ✅ via plugin | ✅ | |

### Gate mode (Gemini / Grok / Cursor with `--approval-mode pre-tool-use`)

Gemini CLI and Grok Build cannot answer their native permission dialogs from a
hook (verified in source: their `allow` decisions are no-ops). To approve from
your phone anyway, install with `--approval-mode pre-tool-use` and launch the
agent with its no-confirm flag — DevPilot's hook then becomes the **only
gate**:

```bash
gemini --approval-mode yolo     # DevPilot gate mode
grok --always-approve           # DevPilot gate mode
```

In gate mode every mutating tool call waits for your phone: approve = the tool
runs, deny = the tool is blocked, **timeout / daemon unreachable = blocked
(fail-closed)** — otherwise an unattended agent would run tools ungated.
Read-only tools never prompt. `devpilot-hook doctor` reminds you of the
required launch flags.

### OpenCode

OpenCode has no `settings.json`-style hook file, so the integration is a global
plugin. `devpilot-hook install --agent claude,codex,opencode` detects the
installed `opencode` version and writes the matching plugin (it never edits
your own plugin files). If you are adding OpenCode to an existing install,
list every agent you use — `--agent` replaces the recorded agent set used by
`serve`'s self-heal:

| OpenCode | Plugin path | API |
|---|---|---|
| 0.x | `~/.config/opencode/plugin/devpilot-hook.ts` | named export + `permission.ask` |
| 2.x | `~/.config/opencode/plugins/devpilot-hook.ts` | default export + `permission.hook("evaluate")` |

OpenCode 1.x still uses the 0.x hook API and is treated as 0.x. Only `.ts`/`.js`
files are discovered (OpenCode ignores `.mjs`). The plugin shells out to
`devpilot-hook hook opencode` for every event, so changing the gateway port or
token never requires reinstalling it. Upgrading OpenCode across a major version
is self-healed by `serve`; `doctor` warns when the plugin generation does not
match the installed CLI.

## Development & testing

```bash
cp go.work.example go.work   # adjust the sibling path if the apns checkout is not ../DevPilot-APNS
go test -race ./...
go test -race -tags=e2e ./test/e2e -v   # needs the sibling apns checkout (DEVPILOT_APNS_DIR overrides the default); builds both binaries on the fly
make build                    # ./bin/devpilot-hook
```

E2E coverage: a real `hook` subprocess → daemon → relay (APNs mock) → fake app over WSS; verifies allow/deny flowing back, first-wins on duplicate decisions, timeout producing no output (never times out into an approval) and the WAL drained.

## Release

This repo depends on `github.com/devpilot-app/apns` (`pkg/wire`); tag that repo first:

```bash
# 1. devpilot-app/apns: publish a tag (relay + pkg/wire)
# 2. This repo: refresh the dependency checksums (a workspace replace does not participate)
GOWORK=off go mod tidy
# 3. Tag to trigger .github/workflows/release.yml (GoReleaser)
git tag vX.Y.Z && git push origin vX.Y.Z
```

GoReleaser needs one extra secret: `HOMEBREW_TAP_TOKEN` (pushes the cask to
`devpilot-app/homebrew-devpilot`). Grant that token `Contents: Read and write`
on the tap repository.

Releases are non-draft: the cask manifest references release assets, and
external downloads would 404 while a release is a draft.

## License

Source-available under the [Functional Source License 1.1, ALv2 Future License](LICENSE.md) (FSL-1.1-ALv2). This is not an OSI-approved open source license: the code stays publicly readable and auditable, but there is one restriction.

**Permitted** — any purpose other than a Competing Use, which includes:

- reading, auditing and modifying the code;
- installing and running the daemon on your own machines;
- internal use in your own products and services;
- non-commercial education and research.

**Not permitted** — a Competing Use: offering this daemon, or anything with substantially similar functionality, to others as a commercial product or hosted service.

Every release becomes available under the [Apache License 2.0](https://www.apache.org/licenses/LICENSE-2.0) on the second anniversary of its release. For a commercial license covering a Competing Use, contact [support@devpilot.app](mailto:support@devpilot.app).
