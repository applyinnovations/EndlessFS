"use strict";

const md5Shifts = [
  7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22,
  5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20,
  4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23,
  6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21,
];
const md5Constants = Array.from({ length: 64 }, (_, index) => Math.floor(Math.abs(Math.sin(index + 1)) * 0x100000000) >>> 0);
const crc32cTable = (() => {
  const table = new Uint32Array(256);
  for (let index = 0; index < 256; index += 1) {
    let value = index;
    for (let bit = 0; bit < 8; bit += 1) value = (value >>> 1) ^ ((value & 1) ? 0x82f63b78 : 0);
    table[index] = value >>> 0;
  }
  return table;
})();

function rotateLeft(value, count) {
  return ((value << count) | (value >>> (32 - count))) >>> 0;
}

function processMD5Block(state, bytes, offset) {
  const words = new Uint32Array(16);
  for (let index = 0; index < 16; index += 1) {
    const position = offset + index * 4;
    words[index] = (bytes[position] | (bytes[position + 1] << 8) | (bytes[position + 2] << 16) | (bytes[position + 3] << 24)) >>> 0;
  }
  let [a, b, c, d] = state;
  for (let index = 0; index < 64; index += 1) {
    let value; let word;
    if (index < 16) {
      value = (b & c) | (~b & d); word = index;
    } else if (index < 32) {
      value = (d & b) | (~d & c); word = (5 * index + 1) % 16;
    } else if (index < 48) {
      value = b ^ c ^ d; word = (3 * index + 5) % 16;
    } else {
      value = c ^ (b | ~d); word = (7 * index) % 16;
    }
    const previousD = d;
    d = c; c = b;
    b = (b + rotateLeft((a + value + md5Constants[index] + words[word]) >>> 0, md5Shifts[index])) >>> 0;
    a = previousD;
  }
  state[0] = (state[0] + a) >>> 0;
  state[1] = (state[1] + b) >>> 0;
  state[2] = (state[2] + c) >>> 0;
  state[3] = (state[3] + d) >>> 0;
}

function base64URL(bytes) {
  let binary = "";
  for (const value of bytes) binary += String.fromCharCode(value);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/u, "");
}

const cancelled = new Set();

async function fingerprint(id, file) {
  const state = new Uint32Array([0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476]);
  const chunkSize = 4 << 20;
  let tail = new Uint8Array(0);
  let crc = 0xffffffff;
  for (let offset = 0; offset < file.size; offset += chunkSize) {
    if (cancelled.has(id)) throw new DOMException("Cancelled", "AbortError");
    const incoming = new Uint8Array(await file.slice(offset, Math.min(file.size, offset + chunkSize)).arrayBuffer());
    for (const value of incoming) crc = (crc32cTable[(crc ^ value) & 0xff] ^ (crc >>> 8)) >>> 0;
    const bytes = new Uint8Array(tail.length + incoming.length);
    bytes.set(tail); bytes.set(incoming, tail.length);
    const complete = bytes.length - (bytes.length % 64);
    for (let position = 0; position < complete; position += 64) processMD5Block(state, bytes, position);
    tail = bytes.slice(complete);
  }
  if (cancelled.has(id)) throw new DOMException("Cancelled", "AbortError");
  const paddingLength = tail.length < 56 ? 64 : 128;
  const finalBytes = new Uint8Array(paddingLength);
  finalBytes.set(tail); finalBytes[tail.length] = 0x80;
  const bitLow = (file.size * 8) >>> 0;
  const bitHigh = Math.floor(file.size / 0x20000000) >>> 0;
  const lengthOffset = paddingLength - 8;
  for (let index = 0; index < 4; index += 1) {
    finalBytes[lengthOffset + index] = bitLow >>> (index * 8);
    finalBytes[lengthOffset + 4 + index] = bitHigh >>> (index * 8);
  }
  for (let position = 0; position < finalBytes.length; position += 64) processMD5Block(state, finalBytes, position);
  const md5 = new Uint8Array(16);
  for (let word = 0; word < 4; word += 1) {
    for (let index = 0; index < 4; index += 1) md5[word * 4 + index] = state[word] >>> (index * 8);
  }
  const crcBytes = new Uint8Array(4);
  const finalizedCRC = (crc ^ 0xffffffff) >>> 0;
  crcBytes[0] = finalizedCRC >>> 24; crcBytes[1] = finalizedCRC >>> 16; crcBytes[2] = finalizedCRC >>> 8; crcBytes[3] = finalizedCRC;
  return { md5: base64URL(md5), crc32c: base64URL(crcBytes) };
}

self.addEventListener("message", async (event) => {
  const { id, type, file } = event.data || {};
  if (type === "cancel") { cancelled.add(id); return; }
  if (type !== "hash" || !id || !(file instanceof Blob)) return;
  try {
    const result = await fingerprint(id, file);
    if (!cancelled.has(id)) self.postMessage({ id, ...result });
  } catch (error) {
    if (!cancelled.has(id)) self.postMessage({ id, error: error && error.message ? error.message : "Hashing failed." });
  } finally {
    cancelled.delete(id);
  }
});
