# hs-dashboard

A small, fast home server dashboard. One Go binary (standard library only), a page with no build step, runs on amd64, arm64 and riscv64.

## Files — what to edit

| File | What it is |
|---|---|
| `config.jsonc` | Your services (name, link, group). Saved changes appear within ~20 seconds, no restart. |
| `web/style.css` | Colours (Catppuccin Macchiato/Latte) at the top. Change `--accent` to re-tint everything. |
| `web/index.html` | Page layout. |
| `web/app.js` | Page behaviour. |
| `stats.go` | What gets read from `/host/proc` and `/host/sys`. |
| `services.go` | Config loading and service checks. |

## Run it

1. Copy `config.jsonc` to `/srv/docker/cont/hs-dashboard/config.jsonc` and edit it.
2. Add the service from `compose.yaml` and run `docker compose up -d`.
3. Open `http://<host>:8080`. Press `/` to filter services, Enter opens the first match.

Without a mounted config it starts with the built-in example, so you can always see it working first.

## Notes

- Disks: the host root is mounted at `/host/root`. Several mounts on one device (Btrfs subvolumes) show once.
- Temperature: reads `thermal_zone*` (ARM/RISC-V boards) and `hwmon` (x86). Shows "No sensor found" if neither exists.
- Without Docker: `HOST_ROOT=/ PORT=8080 CONFIG=./config.jsonc go run .`
