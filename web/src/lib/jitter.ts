const RATE = 48
const PREBUFFER = 2880
const MAX_FRAMES = 24000
const MAX_BYTES = 1048576
const MAX_SILENCE = 5760
const JUMP = 96000

interface QueuedPCM { timestamp: number; unwrapped: number; pcm: Float32Array }

export class JitterBuffer {
  private queue: QueuedPCM[] = []
  private lastRaw?: number
  private lastUnwrapped?: number
  private startedAt?: number
  private origin?: number
  private expected?: number
  deadlineDrops = 0

  get bufferedFrames() { return this.queue.reduce((sum, item) => sum + item.pcm.length, 0) }
  get bufferedBytes() { return this.bufferedFrames * Float32Array.BYTES_PER_ELEMENT }
  private get contiguousFrames() { if (!this.queue.length) return 0; let end = this.queue[0]!.unwrapped + this.queue[0]!.pcm.length; for (const item of this.queue.slice(1)) { if (item.unwrapped > end) break; end = Math.max(end, item.unwrapped + item.pcm.length) } return end - this.queue[0]!.unwrapped }

  push(timestamp: number, pcm: Float32Array): 'queued' | 'reset' | 'overflow' {
    timestamp >>>= 0
    let unwrapped = timestamp
    let result: 'queued' | 'reset' | 'overflow' = 'queued'
    if (this.lastRaw !== undefined && this.lastUnwrapped !== undefined) {
      const delta = ((timestamp - this.lastRaw + 0x80000000) >>> 0) - 0x80000000
      if (Math.abs(delta) > JUMP) { this.reset(); result = 'reset' }
      else unwrapped = this.lastUnwrapped + delta
    }
    this.lastRaw = timestamp; this.lastUnwrapped = unwrapped
    this.queue.push({ timestamp, unwrapped, pcm })
    while (this.bufferedFrames > MAX_FRAMES || this.bufferedBytes > MAX_BYTES) { this.queue.shift(); result = 'overflow'; this.startedAt = undefined; this.origin = undefined; this.expected = undefined }
    return result
  }

  take(nowMs: number): Float32Array[] {
    if (this.startedAt === undefined) {
      if (this.contiguousFrames < PREBUFFER) return []
      this.startedAt = nowMs; this.origin = this.queue[0]!.unwrapped; this.expected = this.origin
    }
    const due = this.origin! + Math.floor((nowMs - this.startedAt) * RATE) + PREBUFFER
    const output: Float32Array[] = []
    while (this.queue.length && this.queue[0]!.unwrapped <= due) {
      const item = this.queue.shift()!
      const end = item.unwrapped + item.pcm.length
      if (end < due - MAX_FRAMES) { this.deadlineDrops++; this.expected = Math.max(this.expected!, end); continue }
      const gap = item.unwrapped - this.expected!
      if (gap > 0) output.push(new Float32Array(Math.min(gap, MAX_SILENCE)))
      output.push(item.pcm)
      this.expected = end
    }
    return output
  }

  reset() { this.queue = []; this.lastRaw = undefined; this.lastUnwrapped = undefined; this.startedAt = undefined; this.origin = undefined; this.expected = undefined }
}
