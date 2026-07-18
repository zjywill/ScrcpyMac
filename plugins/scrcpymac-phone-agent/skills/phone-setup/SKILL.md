---
name: phone-setup
description: Connect and troubleshoot an Android phone for ScrcpyMac Phone Agent. Use when adb device is missing, unauthorized, or the user asks how to set up USB debugging.
---

# Phone Setup

## Prerequisites

- macOS 13+ with the ScrcpyMac Phone Agent plugin installed
- Android 10+ device with USB debugging enabled
- USB cable or Wi-Fi adb

## First-time device setup

1. On the phone: **Settings → About phone → tap Build number 7 times**
2. **Settings → Developer options → USB debugging → ON**
3. Connect USB and accept the RSA fingerprint prompt on the phone
4. Run the `phone_doctor` tool (or `phone-agent doctor` in terminal)

## If no device is found

- Replug the USB cable
- Run `phone_list_devices` and check state is `device` (not `unauthorized`)
- Multiple devices: set environment variable `PHONE_AGENT_SERIAL` to the target serial
- Install adb if doctor reports missing: run `scripts/install.sh`

## Multi-device

When more than one device is connected, set `PHONE_AGENT_SERIAL` before tool calls.

## Wi-Fi adb (optional)

1. USB-connect the phone first
2. `phone_enable_wifi_adb` — enables TCP/IP mode
3. `phone_get_device_ip` — read device Wi-Fi IP
4. Unplug USB (optional)
5. `phone_connect_wifi` with the IP address


## Security

Only automate devices you own. Do not use on shared or untrusted phones.
