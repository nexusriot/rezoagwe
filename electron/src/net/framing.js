'use strict';

// Streams carry the same authenticated frames as datagrams, with a 4-byte
// big-endian length prefix, so a payload larger than a datagram needs no
// separate chunking protocol.

/** Caps a stream frame so a corrupt length prefix cannot make the app allocate wildly. */
const MAX_FRAME_SIZE = 64 << 20;

function frameWithLength(data) {
  if (data.length > MAX_FRAME_SIZE) throw new RangeError('frame exceeds maximum size');
  const hdr = Buffer.alloc(4);
  hdr.writeUInt32BE(data.length, 0);
  return Buffer.concat([hdr, data]);
}

/** Writes one length-prefixed frame to a stream (anything with write()). */
function writeFrame(stream, data) {
  stream.write(frameWithLength(data));
}

/**
 * Incremental parser for length-prefixed frames: feed it chunks, it yields whole
 * frames. Kept separate from the socket so the parsing can be tested on its own.
 */
class FrameReader {
  constructor() {
    this.buf = Buffer.alloc(0);
  }

  /** Appends a chunk and returns every complete frame now available. Throws on an oversized prefix. */
  push(chunk) {
    this.buf = this.buf.length ? Buffer.concat([this.buf, chunk]) : Buffer.from(chunk);
    const frames = [];
    for (;;) {
      if (this.buf.length < 4) break;
      const n = this.buf.readUInt32BE(0);
      if (n > MAX_FRAME_SIZE) throw new RangeError('frame exceeds maximum size');
      if (this.buf.length < 4 + n) break;
      frames.push(this.buf.subarray(4, 4 + n));
      this.buf = this.buf.subarray(4 + n);
    }
    return frames;
  }
}

/**
 * Reads exactly one frame from a stream, or rejects on close/error/timeout. A
 * stream in this protocol carries one request and one response, so one frame is
 * all anybody ever waits for.
 */
function readFrame(stream, timeoutMs) {
  return new Promise((resolve, reject) => {
    const reader = new FrameReader();
    let done = false;
    const finish = (err, frame) => {
      if (done) return;
      done = true;
      clearTimeout(timer);
      stream.removeListener('data', onData);
      stream.removeListener('end', onEnd);
      stream.removeListener('close', onEnd);
      stream.removeListener('error', onError);
      if (err) reject(err);
      else resolve(frame);
    };
    const onData = (chunk) => {
      let frames;
      try {
        frames = reader.push(chunk);
      } catch (e) {
        finish(e);
        return;
      }
      if (frames.length) finish(null, frames[0]);
    };
    const onEnd = () => finish(new Error('stream closed before a frame arrived'));
    const onError = (e) => finish(e);
    const timer = setTimeout(() => finish(new Error('stream read timed out')), timeoutMs || 10_000);
    stream.on('data', onData);
    stream.on('end', onEnd);
    stream.on('close', onEnd);
    stream.on('error', onError);
  });
}

module.exports = { MAX_FRAME_SIZE, frameWithLength, writeFrame, readFrame, FrameReader };
