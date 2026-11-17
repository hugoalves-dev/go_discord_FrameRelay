import test from 'node:test';
import assert from 'node:assert/strict';
let contexts = [], starts, stopped;
class FakeContext {
 constructor() { this.state = 'running'; this.currentTime = 0; this.destination = {}; this.listeners = new Set(); contexts.push(this); }
 createGain() { return { gain: { setTargetAtTime() {} }, connect() {}, disconnect() {} }; }
 createBuffer(channels, frames, rate) { return { duration: frames / rate, copyToChannel() {} }; }
 createBufferSource() { return { connect() {}, disconnect() {}, start: at => starts.push(at), stop() { stopped++; } }; }
 addEventListener(_type, listener) { this.listeners.add(listener); }
 removeEventListener(_type, listener) { this.listeners.delete(listener); }
 async resume() { this.state = 'running'; this.listeners.forEach(l => l()); }
 async close() { this.state = 'closed'; }
}
globalThis.AudioContext = FakeContext;
const { AudioPlayback } = await import('../src/audio-playback.ts');
let closed;
const frame = timestamp => ({ timestamp, numberOfFrames: 960, numberOfChannels: 2, sampleRate: 48000, copyTo() {}, close() { closed++; } });
function playback() { starts = []; stopped = closed = 0; return new AudioPlayback(); }
const near = (actual, expected) => assert.ok(Math.abs(actual - expected) < 1e-9, `${actual} != ${expected}`);

test('schedules jittery and duplicated timestamps contiguously after a jitter buffer', () => {
 const p = playback(); p.context.currentTime = 1;
 for (const ts of [0, 0, 0, 63000, 58000, 120000]) p.push(frame(ts));
 assert.equal(starts.length, 6); assert.equal(closed, 6);
 const first = starts[0]; assert.ok(first >= 1.05 && first <= 1.1, `jitter buffer ${first}`);
 starts.forEach((at, i) => near(at, first + i * 0.02));
 p.close();
});
test('timestamp discontinuity keeps scheduled audio playing', () => {
 const p = playback(); p.push(frame(0)); p.push(frame(5_000_000)); p.push(frame(20000));
 assert.equal(stopped, 0); assert.equal(starts.length, 3); near(starts[2] - starts[0], 0.04); p.close();
});
test('restarts after an underrun and trims an over-full queue', () => {
 const p = playback(); p.push(frame(0)); p.context.currentTime = 5; p.push(frame(20000));
 assert.ok(starts[1] >= 5.05 && starts[1] <= 5.1, `restart ${starts[1]}`);
 for (let i = 0; i < 40; i++) p.push(frame(i * 20000));
 assert.ok(Math.max(...starts) - 5 <= 0.32, `queued ${Math.max(...starts) - 5}`); assert.equal(closed, 42); p.close();
});
test('reuses one audio context across playback instances', () => {
 const first = playback(); first.close(); const second = playback();
 assert.equal(second.context, first.context); assert.equal(second.context.state, 'running'); second.close();
});
