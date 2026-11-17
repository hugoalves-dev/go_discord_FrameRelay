import test from 'node:test';
import assert from 'node:assert/strict';
import { MediaPlayer } from '../src/media-player.ts';
const config = { video: { codec: 'avc1.42e01f', width: 640, height: 360, format: 'annexb' }, audio: null };
let outputs, decoded, closed, requests, supported, throwDecode;
class Decoder {
 static async isConfigSupported(c) { return { supported, config: c }; }
 constructor(callbacks) { outputs = callbacks; this.state = 'unconfigured'; this.decodeQueueSize = 0; }
 configure(c) { this.state = 'configured'; this.config = c; }
 decode(c) { if (throwDecode) throw throwDecode; decoded.push(c); }
 reset() { this.state = 'unconfigured'; }
 close() { this.state = 'closed'; closed++; }
}
globalThis.VideoDecoder = Decoder;
globalThis.EncodedVideoChunk = class { constructor(init) { Object.assign(this, init); } };
const canvas = { width: 0, height: 0, getContext: () => ({ drawImage() {} }) };
function player(onDiagnostic = () => {}) { decoded = []; closed = requests = 0; supported = true; throwDecode = undefined; return new MediaPlayer(canvas, () => requests++, () => {}, () => {}, onDiagnostic); }
function message(type, sequence, flags = 0, generation = 1) {
 return { type, sequence, flags, generation, timestampUs: sequence * 16667, durationUs: 16667,
  payload: type === 1 ? new TextEncoder().encode(JSON.stringify(config)) : new Uint8Array([1]) };
}
test('waits for keyframe and closes every output', async () => {
 const p = player(); await p.accept(message(1, 0)); await p.accept(message(2, 1)); assert.equal(decoded.length, 0);
 await p.accept(message(2, 2, 1)); assert.equal(decoded.length, 1);
 let released = 0; outputs.output({ timestamp: 33334, close() { released++; } });
 p.stop(); assert.equal(released, 1); assert.equal(closed, 1);
});
test('stale generation and video backlog require recovery', async () => {
 const p = player(); await p.accept(message(1, 0)); await p.accept(message(2, 1, 1));
 await p.accept(message(2, 2, 0, 0)); assert.equal(decoded.length, 1);
 p.video.decodeQueueSize = 8; await p.accept(message(2, 2)); assert.ok(requests > 0);
 p.stop();
});
test('rejects unsupported exact codec before decoding', async () => {
 const p = player(); supported = false; await assert.rejects(p.accept(message(1, 0)), /H.264/); p.stop();
});
test('closed decoder is recreated and recovers on a fresh keyframe', async () => {
 const p = player(); await p.accept(message(1, 0)); await p.accept(message(2, 1, 1));
 p.video.state = 'closed'; outputs.error(new Error('decode failed'));
 await p.accept(message(2, 2, 1)); assert.equal(decoded.length, 2); p.stop();
});

test('startup failure distinguishes missing transport from decoder failure', async () => {
 const p = player(); assert.match(p.startupFailure(), /No media configuration/);
 await p.accept(message(1, 0)); assert.match(p.startupFailure(), /No video packets/);
 await p.accept(message(2, 1, 1));
 const error = new Error('private decoder message'); error.name = 'EncodingError';
 outputs.error(error);
 const failure = p.startupFailure();
 assert.match(failure, /received 1 video packets/);
 assert.match(failure, /EncodingError/);
 assert.doesNotMatch(failure, /private decoder message/);
 p.stop();
});
test('startup failure identifies decoded frames discarded by playback timing', async () => {
 const p = player(); await p.accept(message(1, 0)); await p.accept(message(2, 1, 1));
 let released = 0; outputs.output({timestamp: 1000000, close() { released++; }});
 outputs.output({timestamp: 0, close() { released++; }});
 assert.match(p.startupFailure(), /decoded 2 frames/);
 assert.match(p.startupFailure(), /discarded 1/);
 p.stop(); assert.equal(released, 2);
});

test('counts decoded frames cleared during recovery', async () => {
 const p = player(); await p.accept(message(1, 0)); await p.accept(message(2, 1, 1));
 let released = 0; outputs.output({ timestamp: 1_000_000, close() { released++; } });
 p.recover();
 assert.match(p.startupFailure(), /discarded 1/);
 p.stop(); assert.equal(released, 1);
});

test('logs the first keyframe NAL structure and compares SPS to configured codec', async () => {
 const diagnostics = []; const p = player((...entry) => diagnostics.push(entry));
 await p.accept(message(1, 0));
 const key = message(2, 1, 1); key.payload = Uint8Array.from([
  0, 0, 0, 1, 0x67, 0x42, 0xc0, 0x28, 0x80, 0, 0, 1, 0x68, 0xce, 0x06,
  0, 0, 0, 1, 0x65, 0x88, 0x84,
 ]);
 await p.accept(key);
 const [level, event, detail] = diagnostics.find(([, name]) => name === 'H.264 keyframe structure');
 assert.equal(level, 'info'); assert.match(detail, /Annex-B yes/); assert.match(detail, /SPS\/PPS yes/);
 assert.match(detail, /IDR yes/); assert.match(detail, /does not match configured avc1\.42e01f/);
 assert.doesNotMatch(detail, /\b88?84\b/i); p.stop();
});

test('distinguishes synchronous decode rejection from decoder callback failure', async () => {
 const diagnostics = []; const p = player((...entry) => diagnostics.push(entry));
 await p.accept(message(1, 0)); const key = message(2, 1, 1);
 throwDecode = new DOMException('invalid access unit token=private-token', 'DataError'); await p.accept(key);
 let detail = diagnostics.find(([, event]) => event === 'H.264 decode() rejected chunk')?.[2];
 assert.match(detail, /DataError: invalid access unit/); assert.match(detail, /token=\[redacted\]/);
 throwDecode = undefined; outputs.error(new DOMException('decoder rejected access unit', 'DataError'));
 detail = diagnostics.find(([, event]) => event === 'H.264 decoder callback failed')?.[2];
 assert.match(detail, /DataError: decoder rejected access unit/);
 assert.doesNotMatch(detail, /private-token/); p.stop();
});

// Audio-enabled and timing fakes.
const audioConfig = { ...config, audio: { codec: 'opus', sampleRate: 48000, channels: 2 } };
let audioOutputs, audioDecoders, audioDecoded;
class AudioDecoderFake extends Decoder {
 constructor(callbacks) { super({}); outputs = videoOutputs; audioOutputs = callbacks; audioDecoders.push(this); }
 decode(c) { audioDecoded.push(c); }
}
let videoOutputs; const VideoDecoderFake = class extends Decoder { constructor(callbacks) { super(callbacks); videoOutputs = callbacks; } };
globalThis.VideoDecoder = VideoDecoderFake;
globalThis.AudioDecoder = AudioDecoderFake;
globalThis.EncodedAudioChunk = class { constructor(init) { Object.assign(this, init); } };
globalThis.AudioContext = class {
 constructor() { this.state = 'running'; this.currentTime = 0; this.destination = {}; this.listeners = new Set(); }
 createGain() { return { gain: { setTargetAtTime() {} }, connect() {}, disconnect() {} }; }
 createBuffer(_c, frames, rate) { return { duration: frames / rate, copyToChannel() {} }; }
 createBufferSource() { return { connect() {}, disconnect() {}, start() {}, stop() {} }; }
 addEventListener(_t, l) { this.listeners.add(l); } removeEventListener(_t, l) { this.listeners.delete(l); }
 async resume() { this.state = 'running'; this.listeners.forEach(l => l()); } async close() { this.state = 'closed'; }
 async suspend() { this.state = 'suspended'; this.listeners.forEach(l => l()); }
};
const realNow = performance.now.bind(performance); let clock = 0;
function timed(t) { clock = 0; performance.now = () => clock; t.mock.timers.enable({ apis: ['setTimeout'] }); t.after(() => { performance.now = realNow; }); }
function richPlayer() {
 decoded = []; closed = requests = 0; supported = true; throwDecode = undefined; audioDecoders = []; audioDecoded = [];
 const log = { frames: 0, diagnostics: [], audioStates: [] };
 const p = new MediaPlayer(canvas, () => requests++, () => log.frames++, () => {}, (...entry) => log.diagnostics.push(entry), blocked => log.audioStates.push(blocked));
 return Object.assign(p, { log });
}
const configMessage = (cfg, sequence = 0) => ({ ...message(1, sequence), payload: new TextEncoder().encode(JSON.stringify(cfg)) });
const audioFrame = timestamp => ({ timestamp, numberOfFrames: 960, numberOfChannels: 2, sampleRate: 48000, copyTo() {}, close() {} });

test('video recovery keeps the audio decoder and audio packets flowing', async () => {
 const p = richPlayer(); await p.accept(configMessage(audioConfig));
 await p.accept(message(3, 1)); assert.equal(audioDecoded.length, 1, 'audio before first keyframe is decoded');
 await p.accept(message(2, 2, 1)); await p.accept(message(3, 3));
 await p.accept(message(3, 6)); assert.equal(requests, 1, 'sequence gap requests a keyframe');
 await p.accept(message(3, 7)); p.video.decodeQueueSize = 8; await p.accept(message(2, 8));
 assert.equal(audioDecoded.length, 4); assert.equal(audioDecoders.length, 1); assert.notEqual(audioDecoders[0].state, 'closed');
 let released = 0; audioOutputs.output({ ...audioFrame(0), close() { released++; } }); assert.equal(released, 1);
 p.stop(); assert.equal(audioDecoders[0].state, 'closed');
});
test('opus decoder error resets audio only', async () => {
 const p = richPlayer(); await p.accept(configMessage(audioConfig)); await p.accept(message(2, 1, 1));
 const video = p.video; audioOutputs.error(new DOMException('bad packet', 'EncodingError'));
 assert.equal(p.video, video); assert.equal(audioDecoders.length, 2); await p.accept(message(3, 2)); assert.equal(audioDecoded.length, 1); p.stop();
});
test('persistently late video re-anchors the timeline instead of freezing', async t => {
 timed(t); const p = richPlayer(); await p.accept(message(1, 0)); await p.accept(message(2, 1, 1));
 videoOutputs.output({ timestamp: 0, close() {} }); t.mock.timers.tick(200); assert.equal(p.log.frames, 1);
 clock = 10000; for (let i = 1; i <= 6; i++) videoOutputs.output({ timestamp: i * 33333, close() {} });
 t.mock.timers.tick(1000); assert.ok(p.log.frames >= 3, `presented ${p.log.frames}`);
 assert.ok(p.log.diagnostics.some(([, event]) => event === 'Video timeline re-anchored')); p.stop();
});
test('stall watchdog requests recovery when packets arrive but nothing is presented', async t => {
 timed(t); const p = richPlayer(); await p.accept(message(1, 0)); await p.accept(message(2, 1, 1));
 videoOutputs.output({ timestamp: 0, close() {} }); t.mock.timers.tick(200);
 clock = 1500; await p.accept(message(2, 2)); assert.equal(requests, 0);
 clock = 2500; await p.accept(message(2, 3)); assert.equal(requests, 1);
 assert.ok(p.log.diagnostics.some(([level, event]) => level === 'warn' && event === 'Video stalled'));
 clock = 2600; await p.accept(message(2, 4)); assert.equal(requests, 1, 'watchdog backs off'); p.stop();
});
test('static screen with only audio packets does not trip the stall watchdog', async t => {
 timed(t); const p = richPlayer(); await p.accept(configMessage(audioConfig)); await p.accept(message(2, 1, 1));
 videoOutputs.output({ timestamp: 0, close() {} }); t.mock.timers.tick(200);
 clock = 2500; await p.accept(message(3, 2)); clock = 5000; await p.accept(message(3, 3));
 assert.equal(requests, 0, 'capture sends no video while the screen is unchanged');
 assert.ok(!p.log.diagnostics.some(([, event]) => event === 'Video stalled')); p.stop();
});
test('reports audio blocked state on configuration and on context state changes', async () => {
 const p = richPlayer(); await p.accept(configMessage(audioConfig)); assert.deepEqual(p.log.audioStates, [false]);
 const context = p.playback.context; await context.suspend(); assert.deepEqual(p.log.audioStates, [false, true]);
 await p.resumeAudio(); assert.deepEqual(p.log.audioStates, [false, true, false]);
 p.stop(); await context.suspend(); assert.equal(p.log.audioStates.length, 3, 'stopped player stops listening');
});
