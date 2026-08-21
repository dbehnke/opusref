import { describe, expect, it } from 'vitest'
import { JitterBuffer } from '../src/lib/jitter'

const pcm = (frames: number) => new Float32Array(frames).fill(0.25)

describe('bounded jitter buffer', () => {
  it('waits for 60 ms of decoded PCM before playout', () => {
    const queue = new JitterBuffer()
    queue.push(0, pcm(960)); queue.push(960, pcm(960))
    expect(queue.take(0)).toEqual([])
    queue.push(1920, pcm(960))
    expect(queue.take(0).reduce((sum, item) => sum + item.length, 0)).toBe(2880)
  })
  it('does not count a timestamp gap as contiguous prebuffer', () => { const queue = new JitterBuffer(); queue.push(0, pcm(1920)); queue.push(2880, pcm(960)); expect(queue.take(0)).toEqual([]) })
  it('supports variable packet duration and sequence wrap', () => {
    const queue = new JitterBuffer()
    queue.push(0xfffffc40, pcm(960)); queue.push(0, pcm(1920))
    expect(queue.take(0).reduce((sum, item) => sum + item.length, 0)).toBe(2880)
  })
  it('inserts no more than 120 ms of silence for a positive gap', () => {
    const queue = new JitterBuffer()
    queue.push(0, pcm(2880)); queue.take(0)
    queue.push(8640, pcm(960))
    const output = queue.take(200)
    expect(output[0]?.length).toBe(5760)
  })
  it('resets on a timestamp jump over two seconds', () => {
    const queue = new JitterBuffer(); queue.push(0, pcm(2880)); queue.take(0)
    expect(queue.push(96001, pcm(960))).toBe('reset')
    expect(queue.bufferedFrames).toBe(960)
  })
  it('never retains more than 500 ms or one MiB', () => {
    const queue = new JitterBuffer()
    for (let n = 0; n < 30; n++) queue.push(n * 960, pcm(960))
    expect(queue.bufferedFrames).toBeLessThanOrEqual(24000)
    expect(queue.bufferedBytes).toBeLessThanOrEqual(1048576)
  })
  it('drops PCM that misses its deadline', () => {
    const queue = new JitterBuffer(); queue.push(0, pcm(2880)); queue.take(0); queue.push(2880, pcm(960))
    expect(queue.take(1000)).toEqual([])
    expect(queue.deadlineDrops).toBe(1)
  })
})
