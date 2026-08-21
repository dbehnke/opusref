export type PTTState = 'idle' | 'requesting' | 'transmitting' | 'stopping'
export type PTTEffect = 'request' | 'stop' | 'none'

export class PTTMachine {
  state: PTTState = 'idle'
  latched = false

  activate(): PTTEffect {
    if (this.state === 'idle') { this.state = 'requesting'; return 'request' }
    if (this.latched && this.state === 'transmitting') { this.state = 'stopping'; return 'stop' }
    return 'none'
  }

  release(): PTTEffect {
    if (!this.latched && (this.state === 'requesting' || this.state === 'transmitting')) { this.state = 'stopping'; return 'stop' }
    return 'none'
  }

  granted(): void { if (this.state === 'requesting') this.state = 'transmitting' }
  ended(): void { this.state = 'idle' }
  safetyStop(): PTTEffect {
    if (this.state === 'idle' || this.state === 'stopping') return 'none'
    this.state = 'stopping'
    return 'stop'
  }
}
