import { fireEvent, render, screen } from '@testing-library/vue'
import { beforeEach, describe, expect, it } from 'vitest'
import PTTControl from '../src/components/PTTControl.vue'
beforeEach(() => localStorage.clear())
describe('PTT control', () => {
  it('has a 44 pixel control and visible latch label', async () => { const { emitted } = render(PTTControl); const button = screen.getByRole('button', { name: 'Push to talk' }); expect(screen.getByLabelText('Latch PTT')).toBeVisible(); await fireEvent.pointerDown(button); expect(emitted().request).toHaveLength(1); await fireEvent.pointerUp(button); expect(emitted().stop).toHaveLength(1) })
  it('stores latch preference per device', async () => { render(PTTControl); await fireEvent.click(screen.getByLabelText('Latch PTT')); expect(localStorage.getItem('opusref.ptt.latched')).toBe('true') })
})
