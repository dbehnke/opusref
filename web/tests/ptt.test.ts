import { describe, expect, it } from 'vitest'
import { PTTMachine } from '../src/lib/ptt'

describe('PTT state machine', () => {
  it('holds until release by default', () => { const ptt = new PTTMachine(); expect(ptt.activate()).toBe('request'); ptt.granted(); expect(ptt.state).toBe('transmitting'); expect(ptt.release()).toBe('stop') })
  it('uses the next activation to stop in latch mode', () => { const ptt = new PTTMachine(); ptt.latched = true; ptt.activate(); ptt.granted(); expect(ptt.release()).toBe('none'); expect(ptt.activate()).toBe('stop') })
  it('stops on a safety event', () => { const ptt = new PTTMachine(); ptt.activate(); ptt.granted(); expect(ptt.safetyStop()).toBe('stop'); expect(ptt.state).toBe('stopping') })
})
