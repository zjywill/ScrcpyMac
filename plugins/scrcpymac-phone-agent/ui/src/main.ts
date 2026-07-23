import { App } from "@modelcontextprotocol/ext-apps";
import {
  ArrowLeft,
  ClipboardPaste,
  Expand,
  Home,
  LoaderCircle,
  MonitorSmartphone,
  MoreHorizontal,
  Play,
  RefreshCw,
  Smartphone,
  Square,
  Wifi,
  X,
  createIcons,
} from "lucide";

import packageInfo from "../package.json";
import "./styles.css";

type ToolContent = {
  type?: string;
  text?: string;
};

type ToolResult = {
  isError?: boolean;
  _meta?: Record<string, unknown>;
  structuredContent?: unknown;
  content?: ToolContent[];
};

type Device = {
  serial: string;
  state: string;
  model?: string;
  product?: string;
};

type StreamStatus = {
  ok: boolean;
  state: "idle" | "starting" | "streaming" | "error";
  backend: "plugin-h264" | "adb";
  encoding: "H.264" | "JPEG";
  error?: string;
  streamUrl?: string;
  serial?: string;
  deviceName?: string;
  deviceWidth?: number;
  deviceHeight?: number;
  frameWidth?: number;
  frameHeight?: number;
  maxFps?: number;
  resolutionPercent?: number;
  codec?: string;
  fps?: number;
  frames?: number;
};

type UiState = {
  ok: boolean;
  backend: string;
  selectedSerial: string;
  devices: Device[];
  stream: StreamStatus;
  error?: string;
};

type FramePayload = {
  ok: boolean;
  serial: string;
  backend: string;
  deviceWidth: number;
  deviceHeight: number;
  frameWidth: number;
  frameHeight: number;
  mimeType: string;
  dataBase64: string;
  sizeBytes: number;
};

type WsPacket = {
  isConfig: boolean;
  isKeyFrame: boolean;
  timestamp: number;
  payload: Uint8Array;
};

type StreamPullPayload = StreamStatus & {
  transport: "mcp";
  packetCount: number;
  sizeBytes: number;
  droppedGops: number;
  droppedPackets: number;
};

const APP_VERSION = packageInfo.version;

const appRoot = document.querySelector<HTMLDivElement>("#app");
if (!appRoot) throw new Error("Missing ScrcpyMac app root.");

appRoot.innerHTML = `
  <main class="workspace">
    <aside class="sidebar" aria-label="ScrcpyMac controls">
      <header class="brand">
        <span class="brand-mark"><i data-lucide="monitor-smartphone"></i></span>
        <span class="brand-copy">
          <span class="brand-title">
            <strong>ScrcpyMac</strong>
            <span class="brand-version">v${APP_VERSION}</span>
          </span>
          <small>Standalone Android workspace</small>
        </span>
      </header>

      <section class="control-section">
        <div class="section-heading">
          <span>Device</span>
          <button class="icon-button" id="refreshDevices" type="button" title="Refresh devices">
            <i data-lucide="refresh-cw"></i>
          </button>
        </div>
        <label class="field-label" for="deviceSelect">Connected device</label>
        <select id="deviceSelect" aria-label="Connected Android device">
          <option value="">No devices</option>
        </select>
        <div class="device-status" id="deviceStatus">
          <span class="status-dot"></span>
          <span id="deviceStatusText">Checking devices</span>
        </div>
      </section>

      <section class="control-section stream-settings">
        <div class="section-heading"><span>Stream</span></div>
        <div class="setting-grid">
          <label>
            <span class="field-label">Frame rate</span>
            <select id="frameRateSelect" aria-label="Stream frame rate">
              <option value="60">60 FPS</option>
              <option value="30">30 FPS</option>
            </select>
          </label>
          <label>
            <span class="field-label">Resolution</span>
            <select id="resolutionSelect" aria-label="Stream resolution">
              <option value="50">50%</option>
              <option value="75">75%</option>
              <option value="100">100%</option>
            </select>
          </label>
        </div>
      </section>

      <section class="control-section">
        <div class="section-heading"><span>Wireless</span></div>
        <label class="field-label" for="wifiHost">Device address</label>
        <div class="input-action">
          <input id="wifiHost" type="text" inputmode="decimal" placeholder="192.168.1.42" />
          <button class="icon-button" id="connectWifi" type="button" title="Connect over Wi-Fi">
            <i data-lucide="wifi"></i>
          </button>
        </div>
      </section>

      <section class="control-section paste-section">
        <div class="section-heading"><span>Input</span></div>
        <label class="field-label" for="pasteText">Paste text</label>
        <textarea id="pasteText" rows="3" placeholder="Text to paste into Android"></textarea>
        <button class="secondary-button" id="pasteButton" type="button">
          <i data-lucide="clipboard-paste"></i>
          <span>Paste</span>
        </button>
      </section>

      <div class="sidebar-spacer"></div>

      <div class="backend-status" id="backendStatus">
        <span class="status-dot"></span>
        <span id="backendLabel">Plugin runtime</span>
      </div>

      <button class="primary-button" id="previewButton" type="button">
        <i data-lucide="play"></i>
        <span>Start preview</span>
      </button>
    </aside>

    <section class="mirror-shell" aria-label="Android phone preview">
      <header class="mirror-toolbar">
        <div class="mirror-title">
          <span class="status-dot"></span>
          <span id="mirrorTitle">No device selected</span>
          <span class="device-lease" id="deviceLease" hidden>Codex is using this device</span>
        </div>
        <div class="toolbar-actions">
          <button class="icon-button" id="fullscreenButton" type="button" title="Open fullscreen">
            <i data-lucide="expand"></i>
          </button>
          <button class="icon-button danger-hover" id="stopButton" type="button" title="Stop preview" disabled>
            <i data-lucide="x"></i>
          </button>
        </div>
      </header>

      <div class="mirror-stage" id="mirrorStage">
        <div class="empty-state" id="emptyState">
          <span class="empty-icon"><i data-lucide="smartphone"></i></span>
          <strong>Ready to connect</strong>
          <span>Select a device and start the preview</span>
        </div>
        <canvas id="phoneCanvas" class="phone-surface" aria-label="Live Android phone screen"></canvas>
        <img id="phoneFrame" class="phone-surface" alt="Android phone screen" draggable="false" />
        <div class="loading-layer" id="loadingLayer" hidden>
          <i data-lucide="loader-circle"></i>
        </div>
      </div>

      <nav class="phone-navigation" aria-label="Android navigation">
        <button class="nav-button" data-key="back" type="button" title="Back">
          <i data-lucide="arrow-left"></i>
        </button>
        <button class="nav-button" data-key="home" type="button" title="Home">
          <i data-lucide="home"></i>
        </button>
        <button class="nav-button" data-key="recents" type="button" title="Recent apps">
          <i data-lucide="square"></i>
        </button>
        <button class="nav-button" data-key="menu" type="button" title="Menu">
          <i data-lucide="more-horizontal"></i>
        </button>
      </nav>

      <footer class="activity-bar">
        <span id="activityText">Waiting for device</span>
        <span id="frameMeta"></span>
      </footer>
    </section>
  </main>
`;

createIcons({
  icons: {
    ArrowLeft,
    ClipboardPaste,
    Expand,
    Home,
    LoaderCircle,
    MonitorSmartphone,
    MoreHorizontal,
    Play,
    RefreshCw,
    Smartphone,
    Square,
    Wifi,
    X,
  },
});

function requiredElement<T extends HTMLElement>(selector: string): T {
  const element = document.querySelector<T>(selector);
  if (!element) throw new Error(`Missing UI element: ${selector}`);
  return element;
}

const deviceSelect = requiredElement<HTMLSelectElement>("#deviceSelect");
const refreshDevices = requiredElement<HTMLButtonElement>("#refreshDevices");
const deviceStatus = requiredElement<HTMLDivElement>("#deviceStatus");
const deviceStatusText = requiredElement<HTMLSpanElement>("#deviceStatusText");
const backendStatus = requiredElement<HTMLDivElement>("#backendStatus");
const backendLabel = requiredElement<HTMLSpanElement>("#backendLabel");
const frameRateSelect = requiredElement<HTMLSelectElement>("#frameRateSelect");
const resolutionSelect = requiredElement<HTMLSelectElement>("#resolutionSelect");
const previewButton = requiredElement<HTMLButtonElement>("#previewButton");
const previewButtonText = requiredElement<HTMLSpanElement>("#previewButton span");
const stopButton = requiredElement<HTMLButtonElement>("#stopButton");
const fullscreenButton = requiredElement<HTMLButtonElement>("#fullscreenButton");
const mirrorTitle = requiredElement<HTMLSpanElement>("#mirrorTitle");
const mirrorStage = requiredElement<HTMLDivElement>("#mirrorStage");
const phoneCanvas = requiredElement<HTMLCanvasElement>("#phoneCanvas");
const phoneFrame = requiredElement<HTMLImageElement>("#phoneFrame");
const emptyState = requiredElement<HTMLDivElement>("#emptyState");
const loadingLayer = requiredElement<HTMLDivElement>("#loadingLayer");
const deviceLease = requiredElement<HTMLSpanElement>("#deviceLease");
const activityText = requiredElement<HTMLSpanElement>("#activityText");
const frameMeta = requiredElement<HTMLSpanElement>("#frameMeta");
const pasteText = requiredElement<HTMLTextAreaElement>("#pasteText");
const pasteButton = requiredElement<HTMLButtonElement>("#pasteButton");
const wifiHost = requiredElement<HTMLInputElement>("#wifiHost");
const connectWifi = requiredElement<HTMLButtonElement>("#connectWifi");

const PREVIEW_MAX_WIDTH = 540;
const PREVIEW_QUALITY = 60;
const PREVIEW_FRAME_INTERVAL_MS = 100;
const WS_PACKET_HEADER_BYTES = 14;
// Only shed load in genuine distress. The plugin relay already drops whole GOPs
// server-side when a client falls behind, so reaching this queue depth means the
// decoder itself is struggling, not that the transport is bursting.
const DECODE_QUEUE_LIMIT = 30;

let mcpApp: App | null = null;
let previewRunning = false;
let previewMode: "h264" | "jpeg" | "none" = "none";
let frameRequestActive = false;
let refreshTimer: number | null = null;
let statusTimer: number | null = null;
let streamOpenTimer: number | null = null;
let pointerStart: { x: number; y: number; time: number } | null = null;
let currentState: UiState | null = null;
let webSocket: WebSocket | null = null;
let h264Transport: "websocket" | "mcp" | null = null;
let pullGeneration = 0;
let videoDecoder: VideoDecoder | null = null;
let h264Config: Uint8Array | null = null;
let lastFrameAt = 0;
let smoothedFps = 0;
let awaitingKeyFrame = false;
let droppedGroups = 0;
let serverPacketRate = 0;
let firstFrameResolve: (() => void) | null = null;
let firstFrameReject: ((reason: Error) => void) | null = null;

function parseTextPayload(result: ToolResult): unknown {
  const text = result.content?.find((item) => item.type === "text")?.text;
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

async function callToolResult<T>(
  name: string,
  args: Record<string, unknown> = {},
): Promise<{ payload: T; meta?: Record<string, unknown> }> {
  if (!mcpApp) throw new Error("Codex host bridge is not connected.");
  const result = (await mcpApp.callServerTool({
    name,
    arguments: args,
  })) as ToolResult;
  const payload = (result.structuredContent ?? parseTextPayload(result)) as T | null;
  if (result.isError) {
    const message =
      payload && typeof payload === "object" && "error" in payload
        ? String((payload as { error?: unknown }).error)
        : "ScrcpyMac action failed.";
    throw new Error(message);
  }
  if (!payload) throw new Error(`ScrcpyMac tool ${name} returned no structured data.`);
  return { payload, meta: result._meta };
}

async function callTool<T>(name: string, args: Record<string, unknown> = {}): Promise<T> {
  return (await callToolResult<T>(name, args)).payload;
}

function setActivity(message: string, tone: "normal" | "error" = "normal"): void {
  activityText.textContent = message;
  activityText.dataset.tone = tone;
}

function selectedDevice(): Device | undefined {
  return currentState?.devices.find((device) => device.serial === deviceSelect.value);
}

function updateDeviceStatus(): void {
  const device = selectedDevice();
  deviceStatus.dataset.state = device?.state ?? "missing";
  if (!device) {
    deviceStatusText.textContent = currentState?.devices.length ? "Choose a device" : "No devices found";
    mirrorTitle.textContent = "No device selected";
    previewButton.disabled = true;
    return;
  }
  deviceStatusText.textContent = device.state === "device" ? "Ready" : device.state;
  mirrorTitle.textContent = device.model || device.serial;
  previewButton.disabled = previewRunning || device.state !== "device";
}

function renderBackend(backend: string): void {
  const h264 = backend === "plugin-h264";
  backendStatus.dataset.state = h264 ? "streaming" : "fallback";
  backendLabel.textContent = h264 ? "H.264 · plugin runtime" : "JPEG · ADB fallback";
}

function renderDevices(state: UiState): void {
  const previous = deviceSelect.value;
  deviceSelect.replaceChildren();
  if (!state.devices.length) {
    deviceSelect.add(new Option("No devices", ""));
  } else {
    deviceSelect.add(new Option("Choose a device", ""));
    for (const device of state.devices) {
      const label = device.model ? `${device.model} · ${device.serial}` : device.serial;
      deviceSelect.add(new Option(label, device.serial, false, false));
    }
  }
  const preferred = state.devices.some((device) => device.serial === previous)
    ? previous
    : state.selectedSerial || (state.devices.length === 1 ? state.devices[0].serial : "");
  deviceSelect.value = preferred;
  renderBackend(state.backend);
  updateDeviceStatus();
}

async function refreshState(): Promise<void> {
  refreshDevices.classList.add("is-spinning");
  try {
    const state = await callTool<UiState>("scrcpymac_ui_state");
    currentState = state;
    renderDevices(state);
    setActivity(state.error || "Device list updated", state.error ? "error" : "normal");
  } catch (error) {
    setActivity(error instanceof Error ? error.message : String(error), "error");
  } finally {
    refreshDevices.classList.remove("is-spinning");
  }
}

async function selectDevice(serial: string): Promise<void> {
  if (!serial) {
    updateDeviceStatus();
    return;
  }
  try {
    await callTool("scrcpymac_ui_select_device", { serial });
    if (currentState) currentState.selectedSerial = serial;
    updateDeviceStatus();
    setActivity("Device selected");
  } catch (error) {
    setActivity(error instanceof Error ? error.message : String(error), "error");
  }
}

function recordRenderedFrame(width: number, height: number, encoding: string): void {
  const now = performance.now();
  if (lastFrameAt > 0) {
    const instantFps = 1000 / Math.max(1, now - lastFrameAt);
    smoothedFps = smoothedFps > 0 ? smoothedFps * 0.82 + instantFps * 0.18 : instantFps;
  }
  lastFrameAt = now;
  const fpsLabel = smoothedFps > 0 ? `${smoothedFps.toFixed(1)} FPS` : "Starting";
  // Render rate and source rate are shown side by side on purpose: when they
  // diverge, the gap says immediately whether frames are being lost upstream of
  // the decoder or inside it.
  const sourceLabel = serverPacketRate > 0 ? ` · src ${serverPacketRate.toFixed(1)} pkt/s` : "";
  const dropLabel = droppedGroups > 0 ? ` · ${droppedGroups} GOP dropped` : "";
  frameMeta.textContent = `${width}×${height} · ${encoding} · ${fpsLabel}${sourceLabel}${dropLabel}`;
}

function showCanvas(): void {
  phoneCanvas.classList.add("is-visible");
  phoneFrame.classList.remove("is-visible");
  phoneFrame.removeAttribute("src");
  emptyState.hidden = true;
}

function showJpeg(): void {
  phoneFrame.classList.add("is-visible");
  phoneCanvas.classList.remove("is-visible");
  emptyState.hidden = true;
}

function hideSurfaces(): void {
  phoneCanvas.classList.remove("is-visible");
  phoneFrame.classList.remove("is-visible");
  phoneFrame.removeAttribute("src");
  emptyState.hidden = false;
}

function resetFrameStats(): void {
  lastFrameAt = 0;
  smoothedFps = 0;
  droppedGroups = 0;
  serverPacketRate = 0;
  frameMeta.textContent = "";
}

function closeDecoder(): void {
  h264Config = null;
  awaitingKeyFrame = false;
  if (videoDecoder) {
    try {
      videoDecoder.close();
    } catch {
      // Decoder may already be closed after a codec error.
    }
  }
  videoDecoder = null;
}

function closeWebSocket(): void {
  if (streamOpenTimer !== null) window.clearTimeout(streamOpenTimer);
  streamOpenTimer = null;
  const socket = webSocket;
  webSocket = null;
  if (socket && socket.readyState <= WebSocket.OPEN) socket.close();
  firstFrameResolve = null;
  firstFrameReject = null;
}

function closeH264Transport(): void {
  pullGeneration += 1;
  h264Transport = null;
  closeWebSocket();
  closeDecoder();
}

function splitAnnexB(data: Uint8Array): Uint8Array[] {
  const starts: Array<{ index: number; prefix: number }> = [];
  for (let index = 0; index + 3 <= data.length; index += 1) {
    if (
      index + 4 <= data.length &&
      data[index] === 0 &&
      data[index + 1] === 0 &&
      data[index + 2] === 0 &&
      data[index + 3] === 1
    ) {
      starts.push({ index, prefix: 4 });
      index += 3;
    } else if (data[index] === 0 && data[index + 1] === 0 && data[index + 2] === 1) {
      starts.push({ index, prefix: 3 });
      index += 2;
    }
  }
  return starts
    .map((start, position) => {
      const end = starts[position + 1]?.index ?? data.length;
      return data.subarray(start.index + start.prefix, end);
    })
    .filter((nalUnit) => nalUnit.length > 0);
}

function codecFromConfig(data: Uint8Array): string {
  const sps = splitAnnexB(data).find((nalUnit) => (nalUnit[0] & 0x1f) === 7);
  if (!sps || sps.length < 4) return "avc1.42E01E";
  return `avc1.${[sps[1], sps[2], sps[3]]
    .map((value) => value.toString(16).padStart(2, "0").toUpperCase())
    .join("")}`;
}

function concatBytes(first: Uint8Array, second: Uint8Array): Uint8Array {
  const result = new Uint8Array(first.length + second.length);
  result.set(first, 0);
  result.set(second, first.length);
  return result;
}

function parseWsPacket(buffer: ArrayBuffer): WsPacket {
  if (buffer.byteLength < WS_PACKET_HEADER_BYTES) {
    throw new Error("Received a truncated ScrcpyMac H.264 packet.");
  }
  const view = new DataView(buffer);
  if (view.getUint8(0) !== 1) throw new Error("Unsupported ScrcpyMac stream version.");
  const flags = view.getUint8(1);
  const timestamp = Number(view.getBigUint64(2, false));
  const payloadLength = view.getUint32(10, false);
  if (payloadLength !== buffer.byteLength - WS_PACKET_HEADER_BYTES) {
    throw new Error("ScrcpyMac H.264 packet length mismatch.");
  }
  return {
    isConfig: Boolean(flags & 1),
    isKeyFrame: Boolean(flags & 2),
    timestamp,
    payload: new Uint8Array(buffer, WS_PACKET_HEADER_BYTES, payloadLength),
  };
}

function decodeBase64(value: string): Uint8Array {
  const decoded = atob(value);
  const bytes = new Uint8Array(decoded.length);
  for (let index = 0; index < decoded.length; index += 1) {
    bytes[index] = decoded.charCodeAt(index);
  }
  return bytes;
}

function decodePacketBatch(data: Uint8Array): void {
  let offset = 0;
  while (offset < data.byteLength) {
    if (data.byteLength - offset < WS_PACKET_HEADER_BYTES) {
      throw new Error("Received a truncated ScrcpyMac H.264 packet batch.");
    }
    const view = new DataView(data.buffer, data.byteOffset + offset);
    const payloadLength = view.getUint32(10, false);
    const packetLength = WS_PACKET_HEADER_BYTES + payloadLength;
    if (offset + packetLength > data.byteLength) {
      throw new Error("ScrcpyMac H.264 packet batch length mismatch.");
    }
    const packet = data.slice(offset, offset + packetLength);
    decodePacket(parseWsPacket(packet.buffer));
    offset += packetLength;
  }
}

function configureDecoder(codec: string): void {
  closeDecoder();
  const context = phoneCanvas.getContext("2d", { alpha: false, desynchronized: true });
  if (!context) throw new Error("Canvas 2D rendering is unavailable.");
  videoDecoder = new VideoDecoder({
    output: (frame) => {
      const width = frame.displayWidth || frame.codedWidth;
      const height = frame.displayHeight || frame.codedHeight;
      if (phoneCanvas.width !== width || phoneCanvas.height !== height) {
        phoneCanvas.width = width;
        phoneCanvas.height = height;
      }
      context.drawImage(frame, 0, 0, width, height);
      frame.close();
      showCanvas();
      recordRenderedFrame(width, height, "H.264");
      if (streamOpenTimer !== null) {
        window.clearTimeout(streamOpenTimer);
        streamOpenTimer = null;
      }
      if (firstFrameResolve) {
        firstFrameResolve();
        firstFrameResolve = null;
        firstFrameReject = null;
      }
    },
    error: (error) => {
      const resolved = error instanceof Error ? error : new Error(String(error));
      firstFrameReject?.(resolved);
      firstFrameResolve = null;
      firstFrameReject = null;
      if (previewRunning && previewMode === "h264") void fallBackToJpeg(resolved.message);
    },
  });
  videoDecoder.configure({
    codec,
    hardwareAcceleration: "prefer-hardware",
    optimizeForLatency: true,
  });
}

function decodePacket(packet: WsPacket): void {
  if (packet.isConfig) {
    const config = new Uint8Array(packet.payload);
    configureDecoder(codecFromConfig(config));
    h264Config = config;
    return;
  }
  if (!videoDecoder || videoDecoder.state !== "configured" || !h264Config) return;
  // Backpressure must never break the GOP. Discarding one delta frame orphans
  // every later frame that references it, so the picture stays corrupt — or the
  // decoder errors out into the JPEG fallback — until the next keyframe, which
  // scrcpy only emits every few seconds. Skip whole groups and resync instead.
  if (packet.isKeyFrame) {
    awaitingKeyFrame = false;
  } else if (awaitingKeyFrame) {
    return;
  } else if (videoDecoder.decodeQueueSize > DECODE_QUEUE_LIMIT) {
    awaitingKeyFrame = true;
    droppedGroups += 1;
    return;
  }
  const payload = packet.isKeyFrame ? concatBytes(h264Config, packet.payload) : packet.payload;
  videoDecoder.decode(
    new EncodedVideoChunk({
      type: packet.isKeyFrame ? "key" : "delta",
      timestamp: packet.timestamp,
      data: payload,
    }),
  );
}

function applyStreamStatus(status: StreamStatus): void {
  renderBackend(status.backend);
  serverPacketRate = status.fps ?? 0;
  // Only own the label until the decoder produces its first frame. Overwriting a
  // measured render rate with the server's packet rate is what made the earlier
  // ~1 FPS stall impossible to localise from the UI.
  if (status.frameWidth && status.frameHeight && lastFrameAt === 0) {
    frameMeta.textContent =
      `${status.frameWidth}×${status.frameHeight} · ${status.encoding} · src ${serverPacketRate.toFixed(1)} pkt/s`;
  }
}

async function connectWebSocketH264(status: StreamStatus): Promise<void> {
  if (!("VideoDecoder" in window)) {
    throw new Error("WebCodecs VideoDecoder is unavailable in this Codex build.");
  }
  if (!status.streamUrl) throw new Error("Plugin runtime did not return a stream URL.");

  return new Promise<void>((resolve, reject) => {
    firstFrameResolve = () => {
      h264Transport = "websocket";
      resolve();
    };
    firstFrameReject = reject;
    const socket = new WebSocket(status.streamUrl as string);
    socket.binaryType = "arraybuffer";
    webSocket = socket;

    streamOpenTimer = window.setTimeout(() => {
      reject(new Error("Timed out waiting for the first H.264 frame."));
      socket.close();
    }, 8000);

    socket.addEventListener("open", () => {
      setActivity("Connected to standalone H.264 stream");
    });
    socket.addEventListener("message", (event) => {
      try {
        if (typeof event.data === "string") {
          const message = JSON.parse(event.data) as {
            type?: string;
            error?: string;
            codec?: string;
          } & StreamStatus;
          if (message.type === "ready") applyStreamStatus(message);
          if (message.type === "config" && message.codec) setActivity(`Decoding ${message.codec}`);
          if (message.type === "error") throw new Error(message.error || "H.264 stream failed.");
          return;
        }
        decodePacket(parseWsPacket(event.data as ArrayBuffer));
      } catch (error) {
        const resolved = error instanceof Error ? error : new Error(String(error));
        reject(resolved);
        if (previewRunning && previewMode === "h264") void fallBackToJpeg(resolved.message);
      }
    });
    socket.addEventListener("error", () => {
      reject(new Error("Codex could not open the plugin loopback H.264 stream."));
    });
    socket.addEventListener("close", () => {
      if (previewRunning && previewMode === "h264" && h264Transport === "websocket") {
        void fallBackToJpeg("Standalone H.264 stream closed.");
      }
    });
  });
}

async function pullH264Packets(generation: number): Promise<void> {
  while (
    previewRunning &&
    previewMode === "h264" &&
    generation === pullGeneration
  ) {
    const result = await callToolResult<StreamPullPayload>("scrcpymac_ui_stream_pull", {
      max_bytes: 524288,
      timeout_ms: 250,
    });
    applyStreamStatus(result.payload);
    droppedGroups = result.payload.droppedGops || 0;
    if (result.payload.state !== "streaming") {
      throw new Error(result.payload.error || "Standalone H.264 stream stopped.");
    }
    const transportMeta = result.meta?.["scrcpymac/h264"] as
      | { dataBase64?: unknown }
      | undefined;
    const dataBase64 =
      typeof transportMeta?.dataBase64 === "string" ? transportMeta.dataBase64 : "";
    if (dataBase64) decodePacketBatch(decodeBase64(dataBase64));
  }
}

async function connectMcpH264(status: StreamStatus): Promise<void> {
  if (!("VideoDecoder" in window)) {
    throw new Error("WebCodecs VideoDecoder is unavailable in this Codex build.");
  }
  applyStreamStatus(status);
  const generation = ++pullGeneration;
  return new Promise<void>((resolve, reject) => {
    firstFrameResolve = () => {
      h264Transport = "mcp";
      resolve();
    };
    firstFrameReject = reject;
    streamOpenTimer = window.setTimeout(() => {
      reject(new Error("Timed out waiting for the first H.264 frame over the MCP bridge."));
    }, 8000);
    void pullH264Packets(generation).catch((error) => {
      if (generation !== pullGeneration) return;
      const resolved = error instanceof Error ? error : new Error(String(error));
      firstFrameReject?.(resolved);
      firstFrameResolve = null;
      firstFrameReject = null;
      if (previewRunning && previewMode === "h264" && h264Transport === "mcp") {
        void fallBackToJpeg(resolved.message);
      }
    });
  });
}

async function requestFrame(): Promise<void> {
  if (!previewRunning || previewMode !== "jpeg" || frameRequestActive) return;
  frameRequestActive = true;
  const requestStartedAt = performance.now();
  try {
    const frame = await callTool<FramePayload>("scrcpymac_ui_snapshot", {
      max_width: PREVIEW_MAX_WIDTH,
      quality: PREVIEW_QUALITY,
    });
    phoneFrame.src = `data:${frame.mimeType};base64,${frame.dataBase64}`;
    showJpeg();
    recordRenderedFrame(frame.frameWidth, frame.frameHeight, "JPEG");
    setActivity("Compatibility preview · ADB screenshot polling");
  } catch (error) {
    setActivity(error instanceof Error ? error.message : String(error), "error");
  } finally {
    frameRequestActive = false;
    if (previewRunning && previewMode === "jpeg") {
      const elapsed = performance.now() - requestStartedAt;
      refreshTimer = window.setTimeout(requestFrame, Math.max(0, PREVIEW_FRAME_INTERVAL_MS - elapsed));
    }
  }
}

async function fallBackToJpeg(reason: string): Promise<void> {
  if (!previewRunning || previewMode === "jpeg") return;
  previewMode = "jpeg";
  closeH264Transport();
  await callTool<StreamStatus>("scrcpymac_ui_stop_stream").catch(() => undefined);
  renderBackend("adb");
  setActivity(`${reason} Using JPEG fallback.`, "error");
  void requestFrame();
}

function startStatusPolling(): void {
  if (statusTimer !== null) window.clearInterval(statusTimer);
  statusTimer = window.setInterval(() => {
    if (!previewRunning || previewMode !== "h264" || h264Transport !== "websocket") return;
    void callTool<StreamStatus>("scrcpymac_ui_stream_status")
      .then(applyStreamStatus)
      .catch(() => undefined);
  }, 1500);
}

async function startPreview(): Promise<void> {
  if (!deviceSelect.value || previewRunning) return;
  loadingLayer.hidden = false;
  previewRunning = true;
  previewMode = "h264";
  previewButtonText.textContent = "Preview running";
  previewButton.disabled = true;
  stopButton.disabled = false;
  mirrorStage.classList.add("is-active");
  deviceLease.hidden = false;
  resetFrameStats();

  try {
    await selectDevice(deviceSelect.value);
    setActivity("Starting standalone plugin runtime");
    const status = await callTool<StreamStatus>("scrcpymac_ui_start_stream", {
      serial: deviceSelect.value,
      max_fps: Number(frameRateSelect.value),
      resolution_percent: Number(resolutionSelect.value),
    });
    applyStreamStatus(status);
    try {
      await connectWebSocketH264(status);
      setActivity("Live H.264 stream · plugin runtime");
      startStatusPolling();
    } catch (webSocketError) {
      closeWebSocket();
      closeDecoder();
      const reason =
        webSocketError instanceof Error ? webSocketError.message : String(webSocketError);
      setActivity(`${reason} Switching to the MCP H.264 bridge.`);
      await connectMcpH264(status);
      setActivity("Live H.264 stream · MCP bridge");
    }
  } catch (error) {
    await fallBackToJpeg(error instanceof Error ? error.message : String(error));
  } finally {
    loadingLayer.hidden = true;
  }
}

async function stopPreview(callServer = true): Promise<void> {
  previewRunning = false;
  previewMode = "none";
  frameRequestActive = false;
  if (refreshTimer !== null) window.clearTimeout(refreshTimer);
  if (statusTimer !== null) window.clearInterval(statusTimer);
  refreshTimer = null;
  statusTimer = null;
  closeH264Transport();
  if (callServer) {
    await callTool<StreamStatus>("scrcpymac_ui_stop_stream").catch(() => undefined);
  }
  previewButtonText.textContent = "Start preview";
  previewButton.disabled = !selectedDevice() || selectedDevice()?.state !== "device";
  stopButton.disabled = true;
  mirrorStage.classList.remove("is-active");
  deviceLease.hidden = true;
  hideSurfaces();
  resetFrameStats();
  renderBackend("adb");
  setActivity("Preview stopped");
}

async function restartPreview(): Promise<void> {
  if (!previewRunning) return;
  await stopPreview();
  await startPreview();
}

function visibleSurface(): HTMLCanvasElement | HTMLImageElement | null {
  if (phoneCanvas.classList.contains("is-visible")) return phoneCanvas;
  if (phoneFrame.classList.contains("is-visible")) return phoneFrame;
  return null;
}

function normalizedPoint(event: PointerEvent): { x: number; y: number } {
  const surface = visibleSurface();
  if (!surface) return { x: 0, y: 0 };
  const rect = surface.getBoundingClientRect();
  return {
    x: Math.max(0, Math.min(1, (event.clientX - rect.left) / Math.max(rect.width, 1))),
    y: Math.max(0, Math.min(1, (event.clientY - rect.top) / Math.max(rect.height, 1))),
  };
}

async function handlePointerUp(event: PointerEvent): Promise<void> {
  if (!previewRunning || !pointerStart || !visibleSurface()) return;
  const end = normalizedPoint(event);
  const start = pointerStart;
  pointerStart = null;
  const distance = Math.hypot(end.x - start.x, end.y - start.y);
  try {
    if (distance < 0.025) {
      await callTool("scrcpymac_ui_tap", { x: end.x, y: end.y });
      setActivity("Tap sent");
    } else {
      const duration = Math.max(80, Math.min(1200, Date.now() - start.time));
      await callTool("scrcpymac_ui_swipe", {
        x1: start.x,
        y1: start.y,
        x2: end.x,
        y2: end.y,
        duration_ms: duration,
      });
      setActivity("Swipe sent");
    }
    if (previewMode === "jpeg") void requestFrame();
  } catch (error) {
    setActivity(error instanceof Error ? error.message : String(error), "error");
  }
}

async function pressKey(name: string): Promise<void> {
  try {
    await callTool("scrcpymac_ui_key", { name });
    setActivity(`${name[0].toUpperCase()}${name.slice(1)} sent`);
    if (previewMode === "jpeg") void requestFrame();
  } catch (error) {
    setActivity(error instanceof Error ? error.message : String(error), "error");
  }
}

function bindSurfacePointerEvents(surface: HTMLCanvasElement | HTMLImageElement): void {
  surface.addEventListener("pointerdown", (rawEvent) => {
    const event = rawEvent as PointerEvent;
    if (!previewRunning) return;
    surface.setPointerCapture(event.pointerId);
    pointerStart = { ...normalizedPoint(event), time: Date.now() };
  });
  surface.addEventListener("pointerup", (rawEvent) => {
    void handlePointerUp(rawEvent as PointerEvent);
  });
  surface.addEventListener("pointercancel", () => {
    pointerStart = null;
  });
}

refreshDevices.addEventListener("click", () => void refreshState());
deviceSelect.addEventListener("change", () => void selectDevice(deviceSelect.value));
frameRateSelect.addEventListener("change", () => void restartPreview());
resolutionSelect.addEventListener("change", () => void restartPreview());
previewButton.addEventListener("click", () => void startPreview());
stopButton.addEventListener("click", () => void stopPreview());
fullscreenButton.addEventListener("click", () => {
  void mcpApp?.requestDisplayMode({ mode: "fullscreen" });
});

bindSurfacePointerEvents(phoneCanvas);
bindSurfacePointerEvents(phoneFrame);

document.querySelectorAll<HTMLButtonElement>("[data-key]").forEach((button) => {
  button.addEventListener("click", () => void pressKey(button.dataset.key || ""));
});

pasteButton.addEventListener("click", async () => {
  const text = pasteText.value;
  if (!text.trim()) return;
  pasteButton.disabled = true;
  try {
    await callTool("scrcpymac_ui_paste", { text });
    pasteText.value = "";
    setActivity("Text pasted");
    if (previewMode === "jpeg") void requestFrame();
  } catch (error) {
    setActivity(error instanceof Error ? error.message : String(error), "error");
  } finally {
    pasteButton.disabled = false;
  }
});

connectWifi.addEventListener("click", async () => {
  const host = wifiHost.value.trim();
  if (!host) return;
  connectWifi.disabled = true;
  try {
    await callTool("scrcpymac_ui_connect_wifi", { host, port: 5555 });
    setActivity("Wi-Fi device connected");
    await refreshState();
  } catch (error) {
    setActivity(error instanceof Error ? error.message : String(error), "error");
  } finally {
    connectWifi.disabled = false;
  }
});

async function bootstrap(): Promise<void> {
  mcpApp = new App(
    { name: "scrcpymac", version: APP_VERSION },
    { availableDisplayModes: ["inline", "fullscreen"] },
    { autoResize: true },
  );
  mcpApp.addEventListener("hostcontextchanged", (event) => {
    document.documentElement.dataset.theme = event.theme || "system";
  });
  mcpApp.onteardown = async () => {
    await stopPreview();
    return {};
  };
  await mcpApp.connect();
  const context = mcpApp.getHostContext();
  document.documentElement.dataset.theme = context?.theme || "system";
  await mcpApp.requestDisplayMode({ mode: "fullscreen" }).catch(() => undefined);
  await refreshState();
}

void bootstrap().catch((error) => {
  setActivity(error instanceof Error ? error.message : String(error), "error");
  deviceStatusText.textContent = "Host bridge unavailable";
  deviceStatus.dataset.state = "error";
});
