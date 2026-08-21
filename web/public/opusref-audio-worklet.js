class OpusRefAudioProcessor extends AudioWorkletProcessor {
  constructor() {
    super()
    this.capture = []
    this.playback = []
    this.port.onmessage = event => {
      if (event.data.type === 'play' && event.data.pcm instanceof Float32Array) this.playback.push(...event.data.pcm)
      if (event.data.type === 'clear') this.playback.length = 0
    }
  }
  process(inputs, outputs) {
    const input = inputs[0]?.[0]
    if (input) {
      this.capture.push(...input)
      while (this.capture.length >= 960) {
        const block = new Float32Array(this.capture.splice(0, 960))
        this.port.postMessage({ type: 'capture', pcm: block }, [block.buffer])
      }
    }
    const output = outputs[0]?.[0]
    if (output) for (let i = 0; i < output.length; i++) output[i] = this.playback.shift() ?? 0
    return true
  }
}
registerProcessor('opusref-audio', OpusRefAudioProcessor)
