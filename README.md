# snapshoter

A tiny Go web app that drives a USB webcam from a Raspberry Pi to record
timelapse videos. Single static binary, no DB, no framework, stdlib-only.

## Features

- Named **sessions** stored on disk. Only one session captures at a time.
  Pressing **Start** on an existing session **resumes** it — frame
  numbering continues from where it left off.
- Per-session knobs: capture interval, resolution, JPEG quality
  (`-q:v` 1–31), output video FPS.
- **On-demand video compile** per session. Resulting MP4 plays in-browser
  and can be downloaded.
- Configurable data directory (in the UI) and camera device.
- LAN-only / no auth — intended for a home Pi behind a router.

## Build

```sh
go build -o snapshoter
```

Cross-compile for a Raspberry Pi:

```sh
# 64-bit Pi OS (Pi 3/4/5)
GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o snapshoter

# 32-bit Pi OS / armv7
GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o snapshoter
```

Copy the binary onto the Pi (`scp snapshoter pi@<host>:`), no other files
are needed — templates and static assets are embedded.

## Run

```sh
./snapshoter -addr :8080 -data-dir /home/pi/timelapses -camera /dev/video0
```

Flags:

| Flag             | Default        | Notes                                                         |
|------------------|----------------|---------------------------------------------------------------|
| `-addr`          | `:8080`        | listen address                                                |
| `-data-dir`      | `./data`       | initial data directory (also changeable in the Settings page) |
| `-camera`        | `/dev/video0`  | V4L2 device for the USB webcam                                |
| `-fake-camera`   | off            | dev mode — generates synthetic JPEGs, no ffmpeg/v4l2 needed   |

Then open `http://<pi-ip>:8080`.

## Pi setup

```sh
sudo apt update
sudo apt install -y ffmpeg v4l-utils
v4l2-ctl --list-devices       # confirm /dev/video0 exists
ffmpeg -f v4l2 -list_formats all -i /dev/video0   # confirm formats
```

## Run as a systemd service (recommended for an always-on Pi)

A ready-to-use unit lives at [snapshoter.service](snapshoter.service). It runs
the binary as user `junaid`, listens on `:8080`, stores data under
`/home/junaid/snapshoter/data`, and uses `/dev/video0`. Adjust user / paths
in the unit before installing if your layout differs.

### 1. Build for the Pi (on your dev machine)

```sh
# 64-bit Pi OS (Pi 3 / 4 / 5)
GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o snapshoter

# 32-bit Pi OS / armv7
# GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o snapshoter
```

### 2. Copy binary + unit file to the Pi

```sh
PI=junaid@<pi-ip>
ssh "$PI" 'mkdir -p /home/junaid/snapshoter/data'
scp snapshoter         "$PI:/home/junaid/snapshoter/snapshoter"
scp snapshoter.service "$PI:/tmp/snapshoter.service"
```

### 3. On the Pi: install prerequisites and the service

```sh
# ffmpeg + v4l2 tools
sudo apt update
sudo apt install -y ffmpeg v4l-utils

# Confirm the camera is detected
v4l2-ctl --list-devices

# Make sure the service user can read the camera and use the H.264 M2M encoder.
# (Required if your user isn't already in the video group.)
sudo usermod -aG video junaid
# Note: take effect requires logging out and back in, or rebooting.

# Permissions on the binary
chmod +x /home/junaid/snapshoter/snapshoter

# Install the unit
sudo mv /tmp/snapshoter.service /etc/systemd/system/snapshoter.service
sudo systemctl daemon-reload
sudo systemctl enable --now snapshoter
```

### 4. Verify

```sh
sudo systemctl status snapshoter        # should show "active (running)"
journalctl -u snapshoter -f             # follow the app log live
```

Open `http://<pi-ip>:8080` from a laptop on the same LAN.

### Upgrading later

```sh
scp snapshoter "$PI:/tmp/snapshoter.new"
ssh "$PI" 'sudo systemctl stop snapshoter \
  && mv /tmp/snapshoter.new /home/junaid/snapshoter/snapshoter \
  && chmod +x /home/junaid/snapshoter/snapshoter \
  && sudo systemctl start snapshoter'
```

### Common operations

```sh
sudo systemctl restart snapshoter       # restart after editing config in the UI is unnecessary — only restart for binary upgrades or unit changes
sudo systemctl disable --now snapshoter # stop and prevent autostart
journalctl -u snapshoter --since "1 hour ago"
```

## On-disk layout

```
<data_dir>/
  config.json
  sessions/
    <session-id>/
      session.json
      frames/frame_000001.jpg ...
      videos/timelapse-YYYYMMDD-HHMMSS.mp4
```

## Notes

- Encoder is `libx264 -preset veryfast -tune stillimage -crf 23`. On a
  1 GB Pi this is fine for short timelapses; for long ones consider
  hardware encoding (`h264_v4l2m2m`) — not enabled by default because
  it depends on the OS image.
- The per-frame ffmpeg invocation has a 30-second timeout so a hung
  capture can't wedge the loop. The compile has a 1-hour ceiling.
- USB webcams often briefly leave `/dev/video0` marked busy after the
  previous ffmpeg child exits. The capture loop retries up to 4 times
  with exponential backoff (200 ms → 1.4 s total) when ffmpeg reports
  "Device or resource busy", so transient EBUSY does not drop frames.
- State is persisted on every 10th frame (or every 30s, whichever comes
  first), and on Stop / shutdown. If the process dies hard, the next
  Start will re-scan the frames directory to recover the correct counter.
