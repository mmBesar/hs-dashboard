# hs-dashboard

A self-hosted server dashboard in a single container.  
Reads host metrics directly from `/proc` and `/sys` — no agents, no extra tools.

**Supported architectures:** `linux/amd64` · `linux/arm64` · `linux/riscv64`

---

## Features

- **Live metrics** — CPU, RAM, load, uptime, temperatures, disk usage, GPU (AMD/Intel)
- **Service status** — auto-checks all services every 30s, shows online/offline
- **Multi-server** — show services from multiple servers in one view
- **Auto arch detection** — reads `/proc/cpuinfo`, works on x86/ARM/RISC-V
- **Catppuccin** — Macchiato (dark) + Latte (light) + auto theme
- **Scalable UI** — one `scale` value in config controls everything
- **Optional logo** — image or text logo, your choice
- **Zero dependencies** — single Go binary, distroless image, ~10MB

---

## Quick Start

### 1. Create config files

```bash
mkdir -p /srv/docker/cont/hs-dashboard
cp config.json.sample /srv/docker/cont/hs-dashboard/config.json
cp services.json.sample /srv/docker/cont/hs-dashboard/services.json

# Edit both files for your server
nano /srv/docker/cont/hs-dashboard/config.json
nano /srv/docker/cont/hs-dashboard/services.json
```

### 2. Add to your compose file

```yaml
  hs-dashboard:
    image: ghcr.io/mmbesar/hs-dashboard:latest
    container_name: hs-dashboard
    networks:
      - ${HS_NETWORK}
    volumes:
      - ${CONTAINER_DIR}/hs-dashboard/config.json:/config/config.json:ro
      - ${CONTAINER_DIR}/hs-dashboard/services.json:/config/services.json:ro
      - /proc:/host/proc:ro
      - /sys:/host/sys:ro
    environment:
      PORT: "8080"
    restart: always
```

### 3. Add to your reverse proxy

**Caddy:**
```
yourdomain.com {
    tls internal
    reverse_proxy hs-dashboard:8080
}
```

**Nginx:**
```nginx
server {
    listen 80;
    server_name yourdomain.com;
    location / {
        proxy_pass http://hs-dashboard:8080;
        proxy_set_header Host $host;
    }
}
```

---

## Configuration

### config.json

| Field | Description |
|-------|-------------|
| `server.name` | Short name shown in logo. Last character gets accent color. |
| `server.fqdn` | Fully qualified domain name |
| `server.ip` | Server IP address |
| `server.dns` | DNS server IP (set `null` to hide) |
| `server.description` | Shown below logo |
| `hardware.arch` | Architecture string. `null` = auto-detect from `/proc/cpuinfo` |
| `logo.image` | Optional image filename. Place file in a mounted volume. `null` = text logo |
| `thermal.zones` | List thermal zones manually, or leave `[]` for auto-detection |
| `display.timezone` | IANA timezone e.g. `Europe/London`, `America/New_York` |
| `display.theme` | `auto`, `light`, or `dark` |
| `display.scale` | UI scale. `1.0`=normal `1.2`=larger `1.4`=largest |
| `display.refresh_stats_ms` | Stats refresh interval (default `5000`) |
| `display.refresh_status_ms` | Status check interval (default `30000`) |

### Thermal zones

Leave `zones: []` for auto-detection — works on most boards.  
For manual control (custom labels, specific zones):

```json
"thermal": {
  "zones": [
    {
      "path": "/sys/class/thermal/thermal_zone0/temp",
      "label": "CPU",
      "description": "main cluster"
    }
  ]
}
```

Find your zones:
```bash
for z in /sys/class/thermal/thermal_zone*; do
  echo "$z: $(cat $z/type) — $(cat $z/temp)m°C"
done
```

### services.json

Add services to the `servers` array. Each service needs:

```json
{
  "name": "My Service",
  "desc": "What it does",
  "url": "https://service.yourdomain.com",
  "display": "service.yourdomain.com",
  "accent": "#c6a0f6",
  "icon": "server"
}
```

Set `"disabled": true` for planned services — shown greyed out.

### Available icons

`dns` `proxy` `router` `wifi` `shield` `server` `nas` `database` `file` `cloud`
`download` `video` `audio` `image` `tv` `camera` `rss` `cal` `mail` `vault`
`git` `book` `chart` `monitor` `terminal` `code` `settings` `lock` `share`
`search` `printer` `home` `disk` `gpu`

### Accent colors (Catppuccin Macchiato)

| Color | Hex |
|-------|-----|
| Mauve | `#c6a0f6` |
| Green | `#a6da95` |
| Teal | `#8bd5ca` |
| Sky | `#91d7e3` |
| Peach | `#f5a97f` |
| Yellow | `#eed49f` |
| Pink | `#f5bde6` |
| Lavender | `#b7bdf8` |
| Blue | `#8aadf4` |
| Red | `#ed8796` |

---

## Metrics

All metrics read from host `/proc` and `/sys` via read-only mounts:

| Metric | Source |
|--------|--------|
| CPU % | `/host/proc/stat` — two-sample delta |
| RAM | `/host/proc/meminfo` |
| Load average | `/host/proc/loadavg` |
| Uptime | `/host/proc/uptime` |
| CPU temps | `/host/sys/class/thermal/thermal_zone*` |
| AMD GPU temp | `/host/sys/class/drm/card*/device/hwmon/*/temp1_input` |
| AMD GPU usage | `/host/sys/class/drm/card*/device/gpu_busy_percent` |
| AMD VRAM | `/host/sys/class/drm/card*/device/mem_info_vram_*` |
| Intel GPU temp | `/host/sys/class/drm/card*/device/hwmon/*/temp1_input` |
| Disk usage | `/host/proc/mounts` + `statfs` syscall |
| Architecture | `/host/proc/cpuinfo` |

**NVIDIA:** Not supported — `nvidia-smi` cannot run inside a distroless container.

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | HTTP listening port |
| `HOST_PROC` | `/host/proc` | Path to host `/proc` mount |
| `HOST_SYS` | `/host/sys` | Path to host `/sys` mount |
| `CONFIG_DIR` | `/config` | Path to config directory |
| `STATS_INTERVAL` | `5` | Seconds between stat reads |
| `STATUS_INTERVAL` | `30` | Seconds between service checks |

---

## Security

- Runs as non-root (distroless `nonroot` user)
- `/proc` and `/sys` mounted read-only — no write access to host
- No shell in the container — distroless base
- No network access required beyond service status checks

---

## License

MIT — see [LICENSE](LICENSE)
