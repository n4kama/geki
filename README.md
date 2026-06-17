# Geki

Discord bot that replaces `:keyword:` tags in chat with inline emotes (animated gif / static png).

- Scans every message for `:keyword:` tags (case-insensitive, letters/numbers only), anywhere in the text.
- Resolves each keyword via a [7TV](https://7tv.app) search (most-popular exact match).
- Uploads each emote once as an **application emoji** and reuses it across servers/restarts (this is also the cache — no separate emote file).
- Reposts the message through a webhook (sender's name + avatar) with the tags swapped for real inline emojis; surrounding text is kept. Unknown tags are left untouched.

## Per-server overrides

Default emotes (the most-popular 7TV exact match) are **global**. A server can override what a keyword resolves to **for that server only**:

```
!geki set <name> <https-png-or-gif-url>
```

e.g. `!geki set madge https://cdn.7tv.app/emote/01F6ASPNM00009TPCEMWQTT4XX/4x.png`. The override is uploaded as a separate application emoji and recorded under that guild in `servers.json`; other servers keep the default. URLs must be **https** and resolve to a **public** IP (internal/loopback/private addresses are blocked at dial time, redirects included).

## Whitelist (per server)

`!geki set` is restricted to usernames whitelisted **on that server** plus anyone with the **Administrator** permission there. Server admins grant access with:

```
!geki allow <username>
```

Whitelists and overrides live per-guild in `servers.json`. Use the account's Discord username (handle), not its display name.

## Run (local dev)

```sh
go run .            # needs DISCORD_TOKEN in the environment
```

## Deploy (systemd)

Uses [`just`](https://github.com/casey/just). The token is the only manual input; everything else lives in `justfile`, `geki.service`, and `deploy/remote-install.sh` (version-controlled). The bot runs as an unprivileged transient user (`DynamicUser`); `servers.json` persists in `/var/lib/geki`. Install/deploy abort if the target isn't running systemd.

**On the server directly** (needs `just` + Go on that host):

```sh
DISCORD_TOKEN=your-token just install   # build, write /etc/geki.env (0600), install unit, enable + start
just deploy        # rebuild + restart after a code change (token/unit untouched)
```

**From your workstation, over SSH** (cross-builds here, installs on the remote):

```sh
DISCORD_TOKEN=your-token just deploy-remote        # first time: prompts for a host from ~/.ssh/config
just deploy-remote my-server                        # or name the host directly
```

`deploy-remote` detects the remote's CPU arch, cross-compiles, and installs it. The token is sent only on the first deploy (when `/etc/geki.env` doesn't exist yet) and never stored in the repo. Needs SSH access with `sudo` on the remote.

Other recipes (operate on this host; for a remote, `ssh` in and run them there):

```sh
just set-token     # rotate the token from $DISCORD_TOKEN
just logs          # follow journald output
just status        # service status
just uninstall     # remove service + binary (keeps token file and state)
```

## Discord setup

In the [Developer Portal](https://discord.com/developers/applications) for your bot:

- **Bot → Privileged Gateway Intents → Message Content Intent**: ON (required to read messages).
- Invite with **Manage Messages** (delete the user's original) and **Manage Webhooks** (repost under the user's name + avatar).
- Without Manage Webhooks it posts an error to the channel (and logs it); without Manage Messages the original stays.
