# Geki — build & deploy.
#   local / server-direct:  just install        (build + systemd on THIS host)
#   remote over ssh:        just deploy-remote   (build here, install on a remote host)
set shell := ["bash", "-euo", "pipefail", "-c"]

bin     := "/usr/local/bin/geki"
unit    := "/etc/systemd/system/geki.service"
envfile := "/etc/geki.env"

# list available recipes
default:
    @just --list

# run tests
test:
    go test ./...

# build a static, reproducible binary for THIS host
build:
    CGO_ENABLED=0 go build -trimpath -o geki .

# --- local / server-direct (run on the machine that will run the bot) ---

# first-time install on this host: binary + env file + unit, then enable & start
install: _check-systemd build _envfile
    sudo install -m 0755 geki {{bin}}
    sudo install -m 0644 geki.service {{unit}}
    sudo systemctl daemon-reload
    sudo systemctl enable --now geki
    @echo "geki installed and running — 'just logs' to watch"

# routine update on this host: rebuild and restart
deploy: _check-systemd build
    sudo install -m 0755 geki {{bin}}
    sudo systemctl restart geki
    @echo "geki redeployed"

# --- remote (run from your workstation; cross-builds and ships over ssh) ---

# build for the remote's arch and install it there. Picks a host from ~/.ssh/config
# (or type one). First deploy to a host needs DISCORD_TOKEN set locally.
deploy-remote host="":
    #!/usr/bin/env bash
    set -euo pipefail
    target="{{host}}"
    if [[ -z "$target" ]]; then
        mapfile -t hosts < <(awk '$1=="Host"{for(i=2;i<=NF;i++) if($i!~/[*?]/) print $i}' ~/.ssh/config 2>/dev/null | sort -u)
        if ((${#hosts[@]})); then
            echo "Deploy to which host?"
            select t in "${hosts[@]}" "(enter manually)"; do
                if [[ -z "${t:-}" || "$t" == "(enter manually)" ]]; then read -rp "ssh host: " target; else target="$t"; fi
                [[ -n "${target:-}" ]] && break
            done
        else
            read -rp "ssh host: " target
        fi
    fi
    echo "→ target: $target"
    ssh "$target" 'test -d /run/systemd/system' \
        || { echo "ERROR: $target is unreachable or not running systemd." >&2; exit 1; }
    case "$(ssh "$target" uname -m)" in
        x86_64)        arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        armv7l)        arch=arm ;;
        *)             arch=amd64 ;;
    esac
    echo "→ building linux/$arch"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o geki .
    scp -q geki geki.service deploy/remote-install.sh "$target":/tmp/
    if ! ssh "$target" 'test -f /etc/geki.env'; then
        [[ -n "${DISCORD_TOKEN:-}" ]] || { echo "first deploy to $target needs DISCORD_TOKEN set locally" >&2; exit 1; }
        tmp=$(mktemp); printf 'DISCORD_TOKEN=%s\n' "$DISCORD_TOKEN" >"$tmp"
        scp -qp "$tmp" "$target":/tmp/geki.env; rm -f "$tmp"
    fi
    ssh -t "$target" 'sudo bash /tmp/remote-install.sh && rm -f /tmp/remote-install.sh'
    echo "✓ deployed to $target"

# --- ops (operate on THIS host; for a remote, ssh in and run them there) ---

# (re)write /etc/geki.env from $DISCORD_TOKEN and restart
set-token:
    #!/usr/bin/env bash
    set -euo pipefail
    [[ -n "${DISCORD_TOKEN:-}" ]] || { echo "export DISCORD_TOKEN first" >&2; exit 1; }
    sudo install -m 0600 /dev/null {{envfile}}
    printf 'DISCORD_TOKEN=%s\n' "$DISCORD_TOKEN" | sudo tee {{envfile}} >/dev/null
    sudo systemctl restart geki 2>/dev/null || true
    echo "wrote {{envfile}} (0600)"

# follow logs
logs:
    journalctl -u geki -f

# service status
status:
    systemctl status geki --no-pager

# stop and remove the service + binary (keeps {{envfile}} and /var/lib/geki)
uninstall:
    -sudo systemctl disable --now geki
    sudo rm -f {{bin}} {{unit}}
    sudo systemctl daemon-reload
    @echo "removed geki (kept {{envfile}} and /var/lib/geki)"

# fail unless THIS host runs systemd (used by install/deploy)
_check-systemd:
    #!/usr/bin/env bash
    if [[ ! -d /run/systemd/system ]]; then
        echo "ERROR: this host is not running systemd — can't install the service." >&2
        echo "Deploy to a systemd host with 'just deploy-remote', or run 'go run .' directly." >&2
        exit 1
    fi

# create the token env file only if it's missing (used by `install`)
_envfile:
    #!/usr/bin/env bash
    set -euo pipefail
    if sudo test -f {{envfile}}; then
        echo "{{envfile}} exists — leaving it (use 'just set-token' to change)"
        exit 0
    fi
    if [[ -z "${DISCORD_TOKEN:-}" ]]; then
        echo "ERROR: {{envfile}} missing and DISCORD_TOKEN unset." >&2
        echo "Run: DISCORD_TOKEN=xxxx just install" >&2
        exit 1
    fi
    sudo install -m 0600 /dev/null {{envfile}}
    printf 'DISCORD_TOKEN=%s\n' "$DISCORD_TOKEN" | sudo tee {{envfile}} >/dev/null
    echo "wrote {{envfile}} (0600)"
