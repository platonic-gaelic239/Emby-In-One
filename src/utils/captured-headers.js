/**
 * Stores real client headers captured during authentication.
 * Captures are isolated per proxy token so one device's identity does not
 * leak into another device's passthrough requests.
 */
const CAPTURE_KEYS = [
  'user-agent',
  'x-emby-client', 'x-emby-client-version',
  'x-emby-device-name', 'x-emby-device-id',
  'accept', 'accept-language',
];

const capturedByToken = new Map();
let captureSequence = 0;

function buildCapturedHeaders(reqHeaders = {}) {
  const captured = {};
  for (const key of CAPTURE_KEYS) {
    if (reqHeaders[key]) captured[key] = reqHeaders[key];
  }
  return captured;
}

function getLatestEntry() {
  let latest = null;
  for (const entry of capturedByToken.values()) {
    if (!latest || entry.sequence > latest.sequence) {
      latest = entry;
    }
  }
  return latest;
}

function buildInfo(entry) {
  if (!entry) return null;
  const captured = entry.headers;
  return {
    userAgent: captured['user-agent'] || null,
    client: captured['x-emby-client'] || null,
    clientVersion: captured['x-emby-client-version'] || null,
    deviceName: captured['x-emby-device-name'] || null,
    deviceId: captured['x-emby-device-id'] || null,
    capturedAt: entry.capturedAt,
  };
}

const MAX_CAPTURED = 500;

module.exports = {
  set(token, reqHeaders) {
    if (!token) return null;
    if (capturedByToken.size >= MAX_CAPTURED && !capturedByToken.has(token)) {
      let oldestKey = null, oldestSeq = Infinity;
      for (const [k, v] of capturedByToken) {
        if (v.sequence < oldestSeq) { oldestSeq = v.sequence; oldestKey = k; }
      }
      if (oldestKey) capturedByToken.delete(oldestKey);
    }
    const entry = {
      headers: buildCapturedHeaders(reqHeaders),
      capturedAt: new Date().toISOString(),
      sequence: ++captureSequence,
    };
    capturedByToken.set(token, entry);
    return entry.headers;
  },
  get(token) {
    if (!token) return null;
    return capturedByToken.get(token)?.headers || null;
  },
  delete(token) {
    if (!token) return false;
    return capturedByToken.delete(token);
  },
  clear() {
    capturedByToken.clear();
    captureSequence = 0;
  },
  getLatest() {
    const latest = getLatestEntry();
    return latest ? latest.headers : null;
  },
  getInfo() {
    return buildInfo(getLatestEntry());
  },
};