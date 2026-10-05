# hs-dashboard

A small, fast home server dashboard. One Go binary (standard library only), a page with no build step, runs on amd64, arm64 and riscv64.

## Files — what to edit

| File | What it is |
|---|---|
| `config.jsonc` | Services, colours, clock settings. Saved changes appear within ~20 seconds, no restart. A typo shows a red banner with the line number. |
| `logos/` (next to `config.jsonc`) | Optional png/svg/webp/jpg logos. Name a file after the service (`jellyfin.svg`) and it is picked up automatically. |
| `web/style.css` | Colours (Catppuccin Macchiato/Latte) at the top. Change `--accent` to re-tint everything. |
| `web/index.html` | Page layout. |
| `web/app.js` | Page behaviour. |
| `stats.go` | What gets read from `/host/proc` and `/host/sys`. |
| `services.go` | Config loading and service checks. |

## Run it

1. Copy `config.jsonc` to `/srv/docker/cont/hs-dashboard/config.jsonc` and edit it. Optionally make `/srv/docker/cont/hs-dashboard/logos/` and drop logo files in it.
2. Add the service from `compose.yaml` and run `docker compose up -d`.
3. Open `http://<host>:8080`. Keys: `/` filters services (Enter opens the first match), `f` toggles full screen. Click the green/red chip to show only services that are down.

Without a mounted config it starts with the built-in example, so you can always see it working first.

## Notes

- Disks: the host root is mounted at `/host/root`. Several mounts on one device (Btrfs subvolumes) show once.
- Temperature: reads `thermal_zone*` (ARM/RISC-V boards) and `hwmon` (x86). Shows "No sensor found" if neither exists.
- Without Docker: `HOST_ROOT=/ PORT=8080 CONFIG=./config.jsonc go run .`
- The page always fits one screen: it picks the biggest row size at which every service and the monitor are visible with no scrolling. Phones get one column and scroll.
- The tab title shows how many services are down, for example `(2 down) hs`.
