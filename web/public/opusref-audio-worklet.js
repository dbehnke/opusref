class OpusRefAudioProcessor extends AudioWorkletProcessor {
  constructor() {
    super()
    this.capture = []
    this.playback = new Float32Array(24000)
    this.playRead = 0
    this.playWrite = 0
    this.playSize = 0
    this.captureCredits = 4
    this.port.onmessage = event => {
      if (event.data.type === 'play' && event.data.pcm instanceof Float32Array) {
        if (event.data.pcm.length > this.playback.length - this.playSize) {
          this.playRead = this.playWrite = this.playSize = 0
          this.port.postMessage({ type: 'playback-overflow' })
          return
        }
        for (const sample of event.data.pcm) {
          this.playback[this.playWrite] = sample
          this.playWrite = (this.playWrite + 1) % this.playback.length
          this.playSize++
        }
      }
      if (event.data.type === 'clear') this.playRead = this.playWrite = this.playSize = 0
      if (event.data.type === 'capture-ack') this.captureCredits = Math.min(4, this.captureCredits + 1)
    }
  }
  process(inputs, outputs) {
    const input = inputs[0]?.[0]
    if (input) {
      this.capture.push(...input)
      while (this.capture.length >= 960) {
        const block = new Float32Array(this.capture.splice(0, 960))
        if (this.captureCredits === 0) { this.port.postMessage({ type: 'capture-overflow' }); continue }
        this.captureCredits--
        this.port.postMessage({ type: 'capture', pcm: block }, [block.buffer])
      }
    }
    const output = outputs[0]?.[0]
    if (output) for (let i = 0; i < output.length; i++) {
      if (this.playSize === 0) { output[i] = 0; continue }
      output[i] = this.playback[this.playRead]
      this.playRead = (this.playRead + 1) % this.playback.length
      this.playSize--
    }
    return true
  }
}
registerProcessor('opusref-audio', OpusRefAudioProcessor)
