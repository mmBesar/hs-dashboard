# hs-dashboard

A fast, modern home server dashboard. It shows your server's health and all your services on **one screen, with no scrolling**.

- **System monitor in one row:** processor and per-core use, memory, load, temperature, network and disks, with live graphs.
- **All your services at once:** grouped in boxes, with a live up/down dot and response time for each.
- **Fits any screen:** it picks the biggest row size where everything is visible, from a laptop to an ultrawide. Phones get one column.
- **Real logos:** drop `png` / `svg` files in a folder and they are used automatically.
- **Dark and light themes** (Catppuccin Macchiato and Latte), a live clock and date, and a full-screen mode for a wall display.
- **Tiny and portable:** one static Go binary (about 6 MB, standard library only) on `amd64`, `arm64` and `riscv64`. No database, no build step for the web page.

---

## Quick start

**1. Make the config folder** and copy the example config into it:

```bash
mkdir -p /srv/docker/cont/hs-dashboard/logos
cp config.jsonc /srv/docker/cont/hs-dashboard/
```

**2. Add the service** from [`compose.yaml`](compose.yaml) to your compose file, then:

```bash
docker compose up -d
```

**3. Open** `http://<your-server>:8080`.

Edit `config.jsonc` and save. The page picks up changes by itself within about 20 seconds, with no restart.

> Without a config file the dashboard starts with a built-in example, so you can always see it working first.

---

## The config file

`/srv/docker/cont/hs-dashboard/config.jsonc`

Lines starting with `//` are comments, and a trailing comma is fine. If you make a typo, a red banner shows the line number and the last working version stays on screen.

```jsonc
{
  "title": "",                  // big heading; empty = the machine's hostname
  "accent": "mauve",            // colours of the heading and graphs
  "accent2": "sapphire",        // mauve, blue, sapphire, teal, green, yellow, peach, red, pink
  "locale": "",                 // clock language, e.g. "en-GB", "ar-EG"; empty = browser
  "timezone": "Africa/Cairo",   // empty = browser timezone
  "hour12": false,              // true = 12-hour clock, false = 24-hour
  "newTab": false,              // true = open services in a new tab

  "services": [
    { "name": "Jellyfin", "url": "http://hs.mm:8096", "group": "Media", "desc": "Movies and shows" },
    { "name": "Immich",   "url": "http://hs.mm:2283", "group": "Media", "icon": "immich.svg" },
    { "name": "Router",   "url": "https://rt.mm",     "group": "Network", "icon": "📡", "check": "tcp://10.10.10.1:443" }
  ]
}
```

### Service fields

| Field | Required | What it does |
|---|---|---|
| `name` | yes | Title of the row. Also used to find a matching logo. |
| `url` | yes | Where the row links to. Must be `http://` or `https://`. |
| `group` | no | Which box the row goes in. Services without a group go in "Other". |
| `desc` | no | Small text under the name. Hidden automatically when space is tight. |
| `icon` | no | A logo file name (`"jellyfin.svg"`), a logo name without extension (`"jellyfin"`), or an emoji (`"🎬"`). See [Logos](#logos). |
| `check` | no | What to test for the up/down dot, if it differs from `url`. See below. |

### How the up/down dot works

By default the dashboard requests `url` every 20 seconds. The service counts as **up** if it answers with anything below HTTP 500, so a login page (401, 403) or a redirect is still "up". Self-signed certificates are accepted, because this only checks that something is answering.

Use `check` when the link and the health test should differ:

| `check` value | Meaning |
|---|---|
| `"http://127.0.0.1:8096/health"` | Test this address instead of `url`. |
| `"tcp://host:port"` | Up if the port accepts a connection. Good for databases and non-web services. |
| `"none"` | Never checked. Shows a grey dot. |

---

## Logos

Logos are optional. Without one, a row shows coloured initials (for example `JE` for Jellyseerr).

**1. Put the files in the `logos` folder** next to your config:

```
/srv/docker/cont/hs-dashboard/
├── config.jsonc
└── logos/
    ├── jellyfin.svg
    ├── immich.png
    └── paperless-ngx.png
```

Supported types: `svg`, `png`, `webp`, `avif`, `jpg`, `jpeg`, `gif`, `ico`. Square images with a transparent background look best. SVG is sharpest.

**2. Name the file after the service. That is all.**

A logo is matched automatically when its file name equals the service `name`, ignoring capitals, spaces, dashes and underscores:

| Service `name` | Matches any of |
|---|---|
| `Jellyfin` | `jellyfin.svg`, `Jellyfin.png` |
| `Paperless-ngx` | `paperless-ngx.png`, `paperlessngx.png`, `Paperless_ngx.webp` |
| `AdGuard Home` | `adguard-home.svg`, `adguard_home.png`, `AdGuardHome.png` |

If several formats exist for the same name, the order is `svg`, `png`, `webp`, `avif`, `jpg`, `gif`, `ico`.

**3. Or choose the file yourself with `icon`** when the names differ:

```jsonc
{ "name": "Movies",  "url": "http://hs.mm:8096", "icon": "jellyfin.svg" }   // exact file
{ "name": "Photos",  "url": "http://hs.mm:2283", "icon": "immich" }          // immich.svg / .png / ...
{ "name": "Router",  "url": "https://rt.mm",     "icon": "📡" }              // emoji, no file needed
```

New or changed logos appear within about 20 seconds. Files are served only from that one folder, as plain image files, with no folder listing.

> Tip: good logo collections exist for self-hosted apps, for example the [Dashboard Icons](https://github.com/homarr-labs/dashboard-icons) project. Download the `svg` or `png` you need and save it with the service name.

---

## Using it

| Action | How |
|---|---|
| Filter services | Press `/`, type, then `Enter` opens the first match and `Esc` clears. |
| Show only services that are down | Click the green or red chip at the top. |
| Full screen (wall display) | Press `f`, or click the corner icon. |
| Dark or light theme | Click the half-moon icon. It remembers your choice. |
| Hover a row or disk | Shows the full address, status and exact numbers. |

The browser tab title shows how many services are down, for example `(2 down) hs`.

---

## Docker setup explained

```yaml
services:
  hs-dashboard:
    image: ghcr.io/mmbesar/hs-dashboard:latest
    network_mode: host                      # real network cards + reach to local services
    environment:
      - PUID=1000                           # run as this user
      - PGID=1000
      - TZ=Africa/Cairo
      - PORT=8080
    volumes:
      - /proc:/host/proc:ro                 # CPU, memory, load, network
      - /sys:/host/sys:ro                   # temperatures
      - /:/host/root:ro,rslave              # disks and subvolumes
      - /etc/hostname:/host/hostname:ro     # name in the heading
      - /etc/os-release:/host/os-release:ro
      - /srv/docker/cont/hs-dashboard:/config:ro   # config.jsonc + logos/
    read_only: true
    cap_drop: [ALL]
    cap_add: [SETUID, SETGID]               # only so PUID/PGID can take effect
    security_opt: [no-new-privileges:true]
```

### Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | Port the dashboard listens on. |
| `PUID`, `PGID` | `1000` | The process switches to this user after starting. |
| `TZ` | `UTC` | Timezone for server logs. The clock on the page uses `timezone` in the config, or the browser's. |
| `CONFIG` | `/config/config.jsonc` | Path of the config file. |
| `LOGOS` | `/config/logos` | Folder with logos. |
| `HOST_ROOT` | `/host/root` | Where the host's root is mounted. Use `/` when running without Docker. |

The image has no shell. It checks its own health with `/hs-dashboard -healthcheck`.

---

## Run without Docker

```bash
git clone https://github.com/mmbesar/hs-dashboard
cd hs-dashboard
HOST_ROOT=/ PORT=8080 CONFIG=./config.jsonc go run .
```

Needs Go 1.22 or newer. Logos are read from a `logos/` folder next to the config.

---

## Building and updates

The GitHub Actions workflow in `.github/workflows/build.yml` builds the image for **amd64, arm64 and riscv64** and pushes it to `ghcr.io/mmbesar/hs-dashboard`. It runs when you change the Go files, `web/`, `config.jsonc` or the Dockerfile on `main`, or from the **Run workflow** button in the Actions tab.

No emulation is used. Go cross-compiles natively and the final image is only the binary, so nothing has to run on the target CPU.

To update on your server:

```bash
docker compose pull hs-dashboard && docker compose up -d hs-dashboard
```

---

## Customising

Everything is plain text, with no build step for the page.

| File | What to edit |
|---|---|
| `web/style.css` | Colours (Catppuccin tokens at the top) and sizes. |
| `web/index.html` | Page layout. |
| `web/app.js` | Page behaviour, including the row-size steps in `STEPS`. |
| `stats.go` | What is read from `/host/proc` and `/host/sys`. |
| `services.go` | Config loading, logo matching and service checks. |
| `main.go` | Web server, routes, PUID/PGID. |

After editing `web/` or the Go files, rebuild the image (push to `main` and let the workflow run).

<details>
<summary>API endpoints</summary>

| Endpoint | Returns |
|---|---|
| `GET /api/info` | Hostname, board model, OS, kernel, architecture. |
| `GET /api/stream` | Live stats every 2 seconds (Server-Sent Events). |
| `GET /api/history` | The last 5 minutes of graph points. |
| `GET /api/services` | Settings, services and their status. |
| `GET /api/health` | `ok`, used by the Docker health check. |
| `GET /logos/<file>` | One logo image. |

</details>

---

## Troubleshooting

| Problem | Fix |
|---|---|
| Red banner at the top | `config.jsonc` has a typo at the line shown. Fix it and save. The last working version stays on screen meanwhile. |
| "No disks found" | Add the `/:/host/root:ro,rslave` volume. On bare metal set `HOST_ROOT=/`. |
| A disk is missing | Only real filesystems are shown (ext4, btrfs, xfs, f2fs, vfat, exfat, ntfs, zfs, bcachefs). Several mounts on one device show once. |
| "No sensor found" | The board exposes no standard temperature sensor. The dashboard reads `thermal_zone*` and `hwmon`. |
| Network always 0 B/s | Use `network_mode: host`. Without it you only see the container's own traffic. |
| Hostname shows a random id | Mount `/etc/hostname` as shown, or set `"title"` in the config. |
| A service shows down but works | Its `url` may not be reachable from the dashboard. Set `check` to an address that is, or `"none"`. |
| A logo does not show | Check the file name against the table above, that it is in `logos/`, and that the extension is supported. Wait about 20 seconds. |
| Port already in use | Set a different `PORT`. With host networking it uses the host's ports directly. |
| Disks missing after setting `PUID` | A mount point inside a root-only folder cannot be read by a normal user. Leave `PUID`/`PGID` unset to run as root. |

---

## Security notes

- There is **no login**. Keep the dashboard on your LAN, behind your VPN (for example Tailscale), or behind a reverse proxy with authentication.
- The host folders are mounted **read-only**, the container filesystem is read-only, and all capabilities are dropped except the two needed for `PUID`/`PGID`.
- Service checks accept self-signed certificates on purpose. They only test that something answers.

---

## License

Add your license here, for example MIT.
