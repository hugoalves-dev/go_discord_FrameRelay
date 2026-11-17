// Packet timestamps are wall-clock capture times (jittery, often shared by several frames), so audio is
// scheduled back-to-back on a play cursor instead. One context is reused so a resumed context survives reconnects.
const jitterBuffer = 0.08, maxQueued = 0.3;
let sharedContext: AudioContext | undefined;
export class AudioPlayback {
 readonly context: AudioContext;
 private gain: GainNode;
 private sources = new Set<AudioBufferSourceNode>();
 private nextTime = 0;
 private onState: () => void;
 constructor(onState = () => {}) {
  if (!sharedContext || sharedContext.state === 'closed') sharedContext = new AudioContext({ sampleRate: 48000, latencyHint: 'interactive' });
  this.context = sharedContext; this.onState = onState; this.context.addEventListener('statechange', this.onState);
  this.gain = this.context.createGain(); this.gain.connect(this.context.destination);
 }
 get running() { return this.context.state === 'running'; }
 async resume() { await this.context.resume(); }
 setVolume(volume: number) { this.gain.gain.setTargetAtTime(Math.min(1, Math.max(0, volume)), this.context.currentTime, 0.015); }
 push(data: AudioData) {
  try {
   if (!this.running) return;
   if (data.numberOfChannels < 1 || data.numberOfChannels > 2 || data.numberOfFrames > 5760 || data.sampleRate !== 48000)
    throw new Error('Invalid decoded audio');
   const now = this.context.currentTime;
   if (this.nextTime < now) this.nextTime = now + jitterBuffer;
   else if (this.nextTime > now + maxQueued || this.sources.size >= 64) return;
   const buffer = this.context.createBuffer(data.numberOfChannels, data.numberOfFrames, data.sampleRate);
   for (let planeIndex = 0; planeIndex < data.numberOfChannels; planeIndex++) {
    const samples = new Float32Array(data.numberOfFrames); data.copyTo(samples, { planeIndex, format: 'f32-planar' });
    buffer.copyToChannel(samples, planeIndex);
   }
   const source = this.context.createBufferSource(); source.buffer = buffer; source.connect(this.gain);
   this.sources.add(source); source.onended = () => { this.sources.delete(source); source.disconnect(); };
   source.start(this.nextTime); this.nextTime += buffer.duration;
  } finally { data.close(); }
 }
 reset() {
  for (const source of this.sources) { source.onended = null; try { source.stop(); } catch { } source.disconnect(); }
  this.sources.clear(); this.nextTime = 0;
 }
 close() { this.reset(); this.gain.disconnect(); this.context.removeEventListener('statechange', this.onState); }
}
